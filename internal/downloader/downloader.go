package downloader

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/pkg"
)

// Result holds the outcome of downloading a single package.
type Result struct {
	Package   pkg.Package
	FromCache bool
	Skipped   bool // true for path-type packages that don't need downloading
	Err       error
}

// Downloader downloads and extracts packages into the cache.
type Downloader struct {
	cache   *cache.Cache
	workers int
	client  *http.Client
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
	// Skip packages that cannot be fetched into the cache yet.
	if !pkg.RequiresDownload(p) {
		return Result{Package: p, Skipped: true}
	}

	key := cache.CacheKey(p.Dist.URL)

	// Check cache: both SQLite row and extracted directory on disk.
	_, found, err := d.cache.Lookup(p.Name, p.Version)
	if err != nil {
		return Result{Package: p, Err: fmt.Errorf("cache lookup: %w", err)}
	}
	if found && d.cache.HasExtracted(key) {
		return Result{Package: p, FromCache: true}
	}

	// Download the zip.
	body, err := d.fetch(ctx, p.Dist.URL)
	if err != nil {
		return Result{Package: p, Err: fmt.Errorf("download %s: %w", p.Name, err)}
	}

	// Verify SHA if provided.
	if p.Dist.Shasum != "" {
		got := fmt.Sprintf("%x", sha256.Sum256(body))
		if got != p.Dist.Shasum {
			return Result{Package: p, Err: fmt.Errorf("sha256 mismatch for %s: got %s, want %s", p.Name, got, p.Dist.Shasum)}
		}
	}

	// Extract zip into cache.
	extractedDir := d.cache.ExtractedDir(key)
	if err := extractZip(body, extractedDir); err != nil {
		return Result{Package: p, Err: fmt.Errorf("extract %s: %w", p.Name, err)}
	}

	// Record in SQLite.
	if err := d.cache.Insert(p.Name, p.Version, p.Dist.URL, p.Dist.Reference, key); err != nil {
		return Result{Package: p, Err: fmt.Errorf("cache insert: %w", err)}
	}

	return Result{Package: p}
}

func (d *Downloader) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyFetchStatus(url, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
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
