package downloader

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	// Skip path-type packages — they're local dependencies, not downloadable.
	if p.Dist.Type == "path" {
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
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func extractZip(data []byte, dest string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}

	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)

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
