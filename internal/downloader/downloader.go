package downloader

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composerauth"
	"github.com/van-sprundel/vif/internal/pkg"
)

// Result holds the outcome of downloading a single package.
type Result struct {
	Package   pkg.Package
	FromCache bool
	Skipped   bool // true for path-type packages that don't need downloading
	Err       error
	Duration  time.Duration
}

// Downloader downloads and extracts packages into the cache.
type Downloader struct {
	cache   *cache.Cache
	workers int
	client  *http.Client
	auth    *composerauth.Config
}

// New creates a Downloader with the given cache and worker count.
// If workers <= 0, it defaults to min(numCPU, 16).
func New(c *cache.Cache, workers int) *Downloader {
	if workers <= 0 {
		workers = min(runtime.NumCPU(), 16)
	}
	return &Downloader{
		cache:   c,
		workers: workers,
		client:  &http.Client{},
	}
}

// SetAuth configures Composer-style HTTP auth for package archive downloads.
func (d *Downloader) SetAuth(cfg *composerauth.Config) {
	d.auth = cfg
}

// SetHTTPClient overrides the HTTP client used for downloads.
func (d *Downloader) SetHTTPClient(client *http.Client) {
	if client != nil {
		d.client = client
	}
}

// Download fetches all packages in parallel, skipping those already cached.
// Per-package errors are returned in each Result, not as the function error.
// The function error is only returned for fatal setup failures.
func (d *Downloader) Download(ctx context.Context, packages []pkg.Package) ([]Result, error) {
	results := make([]Result, len(packages))
	work := make(chan int, len(packages))

	var wg sync.WaitGroup
	for range d.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				results[i] = d.downloadOne(ctx, packages[i])
			}
		}()
	}

	for i := range packages {
		work <- i
	}
	close(work)
	wg.Wait()

	return results, nil
}

func (d *Downloader) downloadOne(ctx context.Context, p pkg.Package) Result {
	started := time.Now()
	finish := func(r Result) Result {
		r.Duration = time.Since(started)
		return r
	}

	if pkg.RequiresGitClone(p) {
		return finish(d.gitClone(ctx, p))
	}

	// Skip packages that cannot be fetched into the cache.
	if !pkg.RequiresDownload(p) {
		return finish(Result{Package: p, Skipped: true})
	}

	key := cache.CacheKey(p.Dist.URL)

	// Check cache: both SQLite row and extracted directory on disk.
	_, found, err := d.cache.Lookup(p.Name, p.Version)
	if err != nil {
		return finish(Result{Package: p, Err: fmt.Errorf("cache lookup: %w", err)})
	}
	if found && d.cache.HasExtracted(key) {
		return finish(Result{Package: p, FromCache: true})
	}

	// Download the archive.
	body, err := d.fetch(ctx, p.Dist.URL)
	if err != nil {
		return finish(Result{Package: p, Err: fmt.Errorf("download %s: %w", p.Name, err)})
	}

	// Composer dist shasum is historically SHA-1, but some repositories emit
	// SHA-256. Choose the algorithm from the hash width instead of assuming.
	if p.Dist.Shasum != "" {
		if err := verifyDistShasum(p.Name, body, p.Dist.Shasum); err != nil {
			return finish(Result{Package: p, Err: err})
		}
	}

	// Extract archive into cache.
	extractedDir := d.cache.ExtractedDir(key)
	if err := extractArchive(body, p.Dist.Type, extractedDir); err != nil {
		return finish(Result{Package: p, Err: fmt.Errorf("extract %s: %w", p.Name, err)})
	}

	// Record in SQLite.
	if err := d.cache.Insert(p.Name, p.Version, p.Dist.URL, p.Dist.Reference, key); err != nil {
		return finish(Result{Package: p, Err: fmt.Errorf("cache insert: %w", err)})
	}

	return finish(Result{Package: p})
}

func (d *Downloader) gitClone(ctx context.Context, p pkg.Package) Result {
	key := cache.CacheKey(p.Source.URL + "@" + p.Source.Reference)

	_, found, err := d.cache.Lookup(p.Name, p.Version)
	if err != nil {
		return Result{Package: p, Err: fmt.Errorf("cache lookup: %w", err)}
	}
	if found && d.cache.HasExtracted(key) {
		return Result{Package: p, FromCache: true}
	}

	destDir := d.cache.ExtractedDir(key)
	if err := os.RemoveAll(destDir); err != nil {
		return Result{Package: p, Err: fmt.Errorf("clean clone dir: %w", err)}
	}

	args := []string{"clone", "--depth", "1"}
	if ref := strings.TrimSpace(p.Source.Reference); ref != "" {
		// Try cloning the ref as a branch/tag first. If it's a commit hash,
		// we'll do a full clone + checkout below.
		args = append(args, "--branch", ref)
	}
	args = append(args, p.Source.URL, destDir)

	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Shallow clone with --branch fails for bare commit SHAs.
		// Fall back to a full clone + checkout.
		if ref := strings.TrimSpace(p.Source.Reference); ref != "" {
			_ = os.RemoveAll(destDir)
			fallbackArgs := []string{"clone", p.Source.URL, destDir}
			cmd2 := exec.CommandContext(ctx, "git", fallbackArgs...)
			out2, err2 := cmd2.CombinedOutput()
			if err2 != nil {
				return Result{Package: p, Err: fmt.Errorf("git clone %s: %s", p.Name, strings.TrimSpace(string(out2)))}
			}
			cmd3 := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", ref)
			out3, err3 := cmd3.CombinedOutput()
			if err3 != nil {
				return Result{Package: p, Err: fmt.Errorf("git checkout %s@%s: %s", p.Name, ref, strings.TrimSpace(string(out3)))}
			}
		} else {
			return Result{Package: p, Err: fmt.Errorf("git clone %s: %s", p.Name, strings.TrimSpace(string(out)))}
		}
	}

	// Remove .git directory — we only need the source files.
	_ = os.RemoveAll(filepath.Join(destDir, ".git"))

	if err := d.cache.Insert(p.Name, p.Version, p.Source.URL, p.Source.Reference, key); err != nil {
		return Result{Package: p, Err: fmt.Errorf("cache insert: %w", err)}
	}

	return Result{Package: p}
}

func (d *Downloader) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	const maxRetries = 3
	baseDelay := 500 * time.Millisecond

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := baseDelay * time.Duration(1<<(attempt-1))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		d.auth.ApplyRequest(req)

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusOK {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
				continue
			}
			return body, nil
		}

		resp.Body.Close()

		if isRetryableStatus(resp.StatusCode) && attempt < maxRetries-1 {
			lastErr = fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
			continue
		}

		return nil, classifyFetchStatus(rawURL, resp.StatusCode)
	}

	return nil, fmt.Errorf("fetch %s: %d attempts failed: %w", rawURL, maxRetries, lastErr)
}

func verifyDistShasum(packageName string, body []byte, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))

	switch len(want) {
	case 40:
		got := fmt.Sprintf("%x", sha1.Sum(body))
		if got != want {
			return fmt.Errorf("sha1 mismatch for %s: got %s, want %s", packageName, got, want)
		}
		return nil
	case 64:
		got := fmt.Sprintf("%x", sha256.Sum256(body))
		if got != want {
			return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", packageName, got, want)
		}
		return nil
	default:
		return fmt.Errorf("unsupported shasum length for %s: %d", packageName, len(want))
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return code >= 500 && code <= 599
	}
}

func classifyFetchStatus(rawURL string, statusCode int) error {
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusForbidden {
		return fmt.Errorf("fetch %s: HTTP %d", rawURL, statusCode)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("fetch %s: HTTP %d", rawURL, statusCode)
	}

	if isPrivateArchiveHost(u) {
		return fmt.Errorf("fetch %s: HTTP %d; authenticated package archives are not supported yet", rawURL, statusCode)
	}

	return fmt.Errorf("fetch %s: HTTP %d", rawURL, statusCode)
}

func isPrivateArchiveHost(u *url.URL) bool {
	if u == nil {
		return false
	}

	switch strings.ToLower(u.Host) {
	case "gitlab.com", "www.gitlab.com":
		return strings.Contains(u.Path, "/api/v4/projects/") && strings.Contains(u.Path, "/packages/composer/archives/")
	case "api.github.com":
		return strings.Contains(u.Path, "/repos/") && strings.Contains(u.Path, "/zipball/")
	default:
		return false
	}
}

type tarFile struct {
	name    string
	isDir   bool
	mode    os.FileMode
	content []byte
}

type tarCompression int

const (
	tarUncompressed tarCompression = iota
	tarGzip
	tarBzip2
)

func extractArchive(data []byte, distType string, dest string) error {
	switch distType {
	case "zip":
		return extractZip(data, dest)
	case "tar", "tgz":
		return extractTar(data, dest, tarUncompressed)
	case "tar.gz", "gzip":
		return extractTar(data, dest, tarGzip)
	case "tar.bz2", "bzip2":
		return extractTar(data, dest, tarBzip2)
	default:
		return fmt.Errorf("unsupported archive type %q", distType)
	}
}

func extractTar(data []byte, dest string, compression tarCompression) error {
	var r io.Reader = bytes.NewReader(data)
	switch compression {
	case tarGzip:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("open gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	case tarBzip2:
		r = bzip2.NewReader(r)
	}

	tr := tar.NewReader(r)
	var files []tarFile
	var dirs []string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeReg {
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, hdr.Name)
			files = append(files, tarFile{
				name:  hdr.Name,
				isDir: true,
				mode:  os.FileMode(hdr.Mode),
			})
		} else {
			content, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("read %q from tar: %w", hdr.Name, err)
			}
			files = append(files, tarFile{
				name:    hdr.Name,
				mode:    os.FileMode(hdr.Mode),
				content: content,
			})
		}
	}

	prefix := detectTarPrefix(files, dirs)

	for _, f := range files {
		name := f.name
		if prefix != "" {
			name = strings.TrimPrefix(name, prefix)
			if name == "" {
				continue
			}
		}

		target := filepath.Join(dest, name)

		if f.isDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %q: %w", target, err)
		}

		if err := os.WriteFile(target, f.content, f.mode); err != nil {
			return fmt.Errorf("write %q: %w", target, err)
		}
	}

	return nil
}

func detectTarPrefix(files []tarFile, dirs []string) string {
	var topDir string
	for _, d := range dirs {
		if strings.Count(d, "/") == 1 && strings.HasSuffix(d, "/") {
			if topDir != "" {
				return ""
			}
			topDir = d
		}
	}
	if topDir == "" {
		return ""
	}
	for _, f := range files {
		if f.name == topDir {
			continue
		}
		if !strings.HasPrefix(f.name, topDir) {
			return ""
		}
	}
	return topDir
}

func extractZip(data []byte, dest string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}

	// Detect common top-level prefix to strip.
	// Packagist zips wrap all files in a single top-level directory like
	// "vendor-name-reference/". We strip this to match Composer's behavior.
	prefix := detectCommonPrefix(r.File)

	for _, f := range r.File {
		name := f.Name
		if prefix != "" {
			name = strings.TrimPrefix(name, prefix)
			if name == "" {
				continue // skip the prefix directory entry itself
			}
		}

		target := filepath.Join(dest, name)

		// Directory entries.
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
			continue
		}

		// Ensure parent directory exists.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %q: %w", target, err)
		}

		if err := extractFile(f, target); err != nil {
			return err
		}
	}

	return nil
}

// detectCommonPrefix detects the single top-level wrapper directory that
// packagist zips use (e.g. "package-name-reference/"). Returns "prefix/" if
// found, or "" if there is no wrapper directory to strip.
//
// Heuristic: the zip must contain an explicit top-level directory entry (a name
// ending in "/" with no other "/" characters) and all other entries must be
// children of that directory. This avoids stripping legitimate source dirs.
func detectCommonPrefix(files []*zip.File) string {
	if len(files) == 0 {
		return ""
	}

	// Find top-level directory entries (entries like "name/" with no nested slash).
	var topDirs []string
	for _, f := range files {
		name := f.Name
		if !strings.HasSuffix(name, "/") {
			continue
		}
		// Top-level = only one slash, at the end.
		if strings.Count(name, "/") == 1 {
			topDirs = append(topDirs, name)
		}
	}

	// Must have exactly one top-level directory.
	if len(topDirs) != 1 {
		return ""
	}
	prefix := topDirs[0]

	// Verify all entries are either the prefix itself or children of it.
	for _, f := range files {
		if f.Name == prefix {
			continue
		}
		if !strings.HasPrefix(f.Name, prefix) {
			return ""
		}
	}

	return prefix
}

func extractFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %q in zip: %w", f.Name, err)
	}
	defer rc.Close()

	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
	if err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("write %q: %w", target, err)
	}

	return nil
}
