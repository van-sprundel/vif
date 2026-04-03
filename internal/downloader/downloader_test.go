package downloader_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composerauth"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/testhelper"
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

	cacheDir := testhelper.TempDir(t, "cache")
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

	cacheDir := testhelper.TempDir(t, "cache")
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

	cacheDir := testhelper.TempDir(t, "cache")
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

func TestDownloadUsesComposerAuth(t *testing.T) {
	zipData := makeZip(t, map[string]string{
		"src/Auth.php": "<?php class Auth {}",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Private-Token"); got != "token-123" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := testhelper.TempDir(t, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	targetURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("Parse server URL: %v", err)
	}

	packages := []pkg.Package{{
		Name:    "vendor/auth",
		Version: "1.0.0",
		Dist: pkg.Dist{
			Type:      "zip",
			URL:       "https://gitlab.com/api/v4/projects/1/packages/composer/archives/vendor/pkg.zip?sha=abc",
			Reference: "ref",
		},
	}}

	d := downloader.New(c, 1)
	d.SetAuth(&composerauth.Config{
		GitLabToken: map[string]string{"gitlab.com": "token-123"},
	})
	d.SetHTTPClient(&http.Client{
		Transport: rewriteTransport{
			target: targetURL,
			base:   srv.Client().Transport,
		},
	})

	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("result error: %v", results[0].Err)
	}
}

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = req.URL.Host
	return t.base.RoundTrip(clone)
}

func TestDownloadShasumMismatch(t *testing.T) {
	zipData := makeZip(t, map[string]string{
		"src/Baz.php": "<?php class Baz {}",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := testhelper.TempDir(t, "cache")
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

	cacheDir := testhelper.TempDir(t, "cache")
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

	cacheDir := testhelper.TempDir(t, "cache")
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

func TestDownloadSkipsSourceOnlyPackage(t *testing.T) {
	cacheDir := testhelper.TempDir(t, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	packages := []pkg.Package{
		{
			Name:    "vendor/source-only",
			Version: "1.0.0",
			Dist: pkg.Dist{
				Type: "zip",
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
		t.Fatalf("unexpected error: %v", results[0].Err)
	}
	if !results[0].Skipped {
		t.Fatal("expected source-only package to be skipped")
	}
}

// makeZipWithPrefix creates a zip where all files are nested under a top-level
// directory, mimicking how packagist zips are structured.
func makeZipWithPrefix(t *testing.T, prefix string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// Create the top-level directory entry.
	if _, err := w.Create(prefix); err != nil {
		t.Fatalf("zip create dir %q: %v", prefix, err)
	}
	for name, content := range files {
		f, err := w.Create(prefix + name)
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

func TestDownloadStripsTopLevelPrefix(t *testing.T) {
	// Simulate a packagist-style zip with a top-level "vendor-foo-abc123/" wrapper.
	zipData := makeZipWithPrefix(t, "vendor-foo-abc123/", map[string]string{
		"src/Foo.php":   "<?php class Foo {}",
		"composer.json": `{"name":"vendor/foo"}`,
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := testhelper.TempDir(t, "cache")
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
			},
		},
	}

	d := downloader.New(c, 1)
	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("result error: %v", results[0].Err)
	}

	// Files should be extracted WITHOUT the top-level prefix directory.
	key := cache.CacheKey(packages[0].Dist.URL)
	extractedDir := c.ExtractedDir(key)

	// src/Foo.php should exist directly, not under vendor-foo-abc123/.
	fooPath := filepath.Join(extractedDir, "src", "Foo.php")
	data, err := os.ReadFile(fooPath)
	if err != nil {
		t.Fatalf("expected src/Foo.php at root, got error: %v", err)
	}
	if string(data) != "<?php class Foo {}" {
		t.Errorf("content = %q, want %q", data, "<?php class Foo {}")
	}

	// The wrapper directory should NOT exist.
	wrapperPath := filepath.Join(extractedDir, "vendor-foo-abc123")
	if _, err := os.Stat(wrapperPath); err == nil {
		t.Error("top-level wrapper directory should have been stripped")
	}
}

func TestDownloadRetriesOnTransientError(t *testing.T) {
	zipData := makeZip(t, map[string]string{
		"src/Retry.php": "<?php class Retry {}",
	})

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := testhelper.TempDir(t, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	packages := []pkg.Package{
		{
			Name:    "vendor/retry",
			Version: "1.0.0",
			Dist: pkg.Dist{
				Type:      "zip",
				URL:       srv.URL + "/retry.zip",
				Reference: "abc123",
			},
		},
	}

	d := downloader.New(c, 1)
	results, err := d.Download(context.Background(), packages)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if results[0].Err != nil {
		t.Fatalf("result error: %v", results[0].Err)
	}

	if got := attempts.Load(); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
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
				cacheDir := testhelper.TempDir(b, "cache")
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
