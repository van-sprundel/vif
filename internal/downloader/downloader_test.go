package downloader_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/pkg"
)

// makeZip creates an in-memory zip archive containing the given files.
func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// sha256hex returns the hex-encoded SHA-256 of data.
func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

func TestDownloadSingle(t *testing.T) {
	zipData := makeZip(t, map[string]string{
		"src/Foo.php": "<?php class Foo {}",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	packages := []pkg.Package{
		{
			Name:    "vendor/foo",
			Version: "1.0.0",
			Dist: pkg.Dist{
				Type:      "zip",
				URL:       srv.URL + "/vendor-foo-1.0.0.zip",
				Reference: "abc123",
				Shasum:    sha256hex(zipData),
			},
		},
	}

	d := downloader.New(c, 1)
	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("result error: %v", results[0].Err)
	}
	if results[0].FromCache {
		t.Error("expected FromCache=false for fresh download")
	}

	// Verify extracted file exists on disk.
	key := cache.CacheKey(packages[0].Dist.URL)
	extracted := filepath.Join(c.ExtractedDir(key), "src", "Foo.php")
	data, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "<?php class Foo {}" {
		t.Errorf("extracted content = %q, want %q", data, "<?php class Foo {}")
	}
}

func TestDownloadParallel(t *testing.T) {
	var requestCount atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		zipData := makeZip(t, map[string]string{
			"index.php": "<?php // " + r.URL.Path,
		})
		w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	n := 10
	packages := make([]pkg.Package, n)
	for i := range n {
		packages[i] = pkg.Package{
			Name:    fmt.Sprintf("vendor/pkg%d", i),
			Version: "1.0.0",
			Dist: pkg.Dist{
				Type:      "zip",
				URL:       fmt.Sprintf("%s/pkg%d.zip", srv.URL, i),
				Reference: "ref",
			},
		}
	}

	d := downloader.New(c, 4)
	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("result[%d] error: %v", i, r.Err)
		}
	}

	if got := requestCount.Load(); got != int64(n) {
		t.Errorf("server received %d requests, want %d", got, n)
	}
}

func TestDownloadCacheHit(t *testing.T) {
	var requestCount atomic.Int64

	zipData := makeZip(t, map[string]string{
		"src/Bar.php": "<?php class Bar {}",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	packages := []pkg.Package{
		{
			Name:    "vendor/bar",
			Version: "2.0.0",
			Dist: pkg.Dist{
				Type:      "zip",
				URL:       srv.URL + "/bar.zip",
				Reference: "def456",
			},
		},
	}

	d := downloader.New(c, 1)

	// First download — should hit the server.
	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download 1: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("result 1 error: %v", results[0].Err)
	}
	if results[0].FromCache {
		t.Error("first download should not be from cache")
	}

	if got := requestCount.Load(); got != 1 {
		t.Fatalf("after first download: %d requests, want 1", got)
	}

	// Second download — should skip the server (cache hit).
	results, err = d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download 2: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("result 2 error: %v", results[0].Err)
	}
	if !results[0].FromCache {
		t.Error("second download should be from cache")
	}

	// Server should still have only 1 request.
	if got := requestCount.Load(); got != 1 {
		t.Errorf("after second download: %d requests, want 1", got)
	}
}

func TestDownloadShasumMismatch(t *testing.T) {
	zipData := makeZip(t, map[string]string{
		"src/Baz.php": "<?php class Baz {}",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	packages := []pkg.Package{
		{
			Name:    "vendor/baz",
			Version: "1.0.0",
			Dist: pkg.Dist{
				Type:      "zip",
				URL:       srv.URL + "/baz.zip",
				Reference: "ref",
				Shasum:    "0000000000000000000000000000000000000000000000000000000000000000",
			},
		},
	}

	d := downloader.New(c, 1)
	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if results[0].Err == nil {
		t.Fatal("expected error for SHA mismatch, got nil")
	}
}

func TestDownloadHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	packages := []pkg.Package{
		{
			Name:    "vendor/missing",
			Version: "1.0.0",
			Dist: pkg.Dist{
				Type:      "zip",
				URL:       srv.URL + "/missing.zip",
				Reference: "ref",
			},
		},
	}

	d := downloader.New(c, 1)
	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if results[0].Err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}

func TestDownloadContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until context is done.
		<-r.Context().Done()
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	packages := []pkg.Package{
		{
			Name:    "vendor/slow",
			Version: "1.0.0",
			Dist: pkg.Dist{
				Type: "zip",
				URL:  srv.URL + "/slow.zip",
			},
		},
	}

	d := downloader.New(c, 1)
	results, err := d.Download(ctx, packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if results[0].Err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func BenchmarkDownloadParallel(b *testing.B) {
	zipData := makeZipB(b, map[string]string{
		"src/Bench.php": "<?php class Bench {}",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipData)
	}))
	defer srv.Close()

	for _, workers := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				cacheDir := b.TempDir()
				c, err := cache.New(cacheDir)
				if err != nil {
					b.Fatalf("cache.New: %v", err)
				}

				packages := make([]pkg.Package, 10)
				for j := range packages {
					packages[j] = pkg.Package{
						Name:    fmt.Sprintf("vendor/bench%d", j),
						Version: "1.0.0",
						Dist: pkg.Dist{
							Type: "zip",
							URL:  fmt.Sprintf("%s/bench%d-%d.zip", srv.URL, i, j),
						},
					}
				}

				d := downloader.New(c, workers)
				_, err = d.Download(context.Background(), packages)
				if err != nil {
					b.Fatalf("Download: %v", err)
				}
				c.Close()
			}
		})
	}
}

// makeZipB is the benchmark variant of makeZip.
func makeZipB(b *testing.B, files map[string]string) []byte {
	b.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			b.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			b.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		b.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
