package resolver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/van-sprundel/vif/internal/packagist"
)

// mockFetcher is a thread-safe in-memory Fetcher for testing prefetchMetadata.
type mockFetcher struct {
	mu       sync.Mutex
	packages map[string][]packagist.VersionEntry
	fetched  atomic.Int64 // total GetPackage calls
}

func newMockFetcher() *mockFetcher {
	return &mockFetcher{packages: make(map[string][]packagist.VersionEntry)}
}

func (f *mockFetcher) add(name, ver string, require map[string]string) {
	req := packagist.RelationMap(require)
	f.packages[name] = append(f.packages[name], packagist.VersionEntry{
		Name:    name,
		Version: ver,
		Require: req,
	})
}

func (f *mockFetcher) GetPackage(_ context.Context, name string) ([]packagist.VersionEntry, error) {
	f.fetched.Add(1)
	f.mu.Lock()
	versions, ok := f.packages[name]
	f.mu.Unlock()
	if !ok {
		return nil, packagist.ErrPackageNotFound
	}
	return versions, nil
}

// TestPrefetchMetadataDiscoversTransitiveDeps verifies that prefetchMetadata
// performs BFS and returns results for all reachable packages.
func TestPrefetchMetadataDiscoversTransitiveDeps(t *testing.T) {
	client := newMockFetcher()
	client.add("acme/foo", "1.0.0", map[string]string{"acme/bar": "^1.0"})
	client.add("acme/bar", "1.2.0", map[string]string{"acme/baz": "^2.0"})
	client.add("acme/baz", "2.0.0", nil)

	results := prefetchMetadata(context.Background(), client, []string{"acme/foo"}, nil, nil)

	for _, name := range []string{"acme/foo", "acme/bar", "acme/baz"} {
		r, ok := results[name]
		if !ok {
			t.Errorf("missing result for %s", name)
			continue
		}
		if r.err != nil {
			t.Errorf("unexpected error for %s: %v", name, r.err)
		}
		if len(r.versions) == 0 {
			t.Errorf("no versions for %s", name)
		}
	}

	if len(results) != 3 {
		t.Errorf("got %d results, want 3", len(results))
	}
}

// TestPrefetchMetadataDeduplicatesPackages verifies that each package is
// fetched exactly once even when multiple packages depend on it.
func TestPrefetchMetadataDeduplicatesPackages(t *testing.T) {
	client := newMockFetcher()
	// Both acme/a and acme/b require acme/shared.
	client.add("acme/a", "1.0.0", map[string]string{"acme/shared": "^1.0"})
	client.add("acme/b", "1.0.0", map[string]string{"acme/shared": "^1.0"})
	client.add("acme/shared", "1.5.0", nil)

	results := prefetchMetadata(context.Background(), client, []string{"acme/a", "acme/b"}, nil, nil)

	if _, ok := results["acme/shared"]; !ok {
		t.Fatal("missing acme/shared in results")
	}

	// Count how many times acme/shared was fetched; it must be exactly 1.
	// The total fetched count should be 3 (a, b, shared).
	total := client.fetched.Load()
	if total != 3 {
		t.Errorf("total fetches = %d, want 3 (a, b, shared each once)", total)
	}
}

// TestPrefetchMetadataProgressCallback verifies that the progress callback is
// invoked for each unique package fetch.
func TestPrefetchMetadataProgressCallback(t *testing.T) {
	client := newMockFetcher()
	client.add("acme/foo", "1.0.0", map[string]string{"acme/bar": "^1.0"})
	client.add("acme/bar", "1.0.0", nil)

	var (
		mu       sync.Mutex
		reported []string
	)
	progress := func(name string) {
		mu.Lock()
		reported = append(reported, name)
		mu.Unlock()
	}

	prefetchMetadata(context.Background(), client, []string{"acme/foo"}, progress, nil)

	mu.Lock()
	defer mu.Unlock()

	if len(reported) != 2 {
		t.Errorf("got %d progress events, want 2: %v", len(reported), reported)
	}
	seen := make(map[string]bool)
	for _, n := range reported {
		seen[n] = true
	}
	if !seen["acme/foo"] || !seen["acme/bar"] {
		t.Errorf("progress events = %v, want both acme/foo and acme/bar", reported)
	}
}

// TestPrefetchMetadataHandlesNotFound verifies that packages returning
// ErrPackageNotFound are recorded in the results without panicking.
func TestPrefetchMetadataHandlesNotFound(t *testing.T) {
	client := newMockFetcher()
	client.add("acme/foo", "1.0.0", map[string]string{"missing/pkg": "^1.0"})
	// missing/pkg is intentionally absent from the registry.

	results := prefetchMetadata(context.Background(), client, []string{"acme/foo"}, nil, nil)

	if _, ok := results["acme/foo"]; !ok {
		t.Fatal("missing result for acme/foo")
	}

	r, ok := results["missing/pkg"]
	if !ok {
		t.Fatal("missing/pkg should appear in results (with error)")
	}
	if r.err == nil {
		t.Error("expected non-nil error for missing/pkg")
	}
}

func TestPrefetchMetadataBoundsDependencyScanToRecentVersions(t *testing.T) {
	oldLimit := prefetchDependencyScanVersionLimit
	prefetchDependencyScanVersionLimit = 2
	defer func() { prefetchDependencyScanVersionLimit = oldLimit }()

	client := newMockFetcher()
	// Newest two versions depend on acme/recent. Older versions depend on acme/old.
	client.add("acme/root", "3.0.0", map[string]string{"acme/recent": "^1.0"})
	client.add("acme/root", "2.0.0", map[string]string{"acme/recent": "^1.0"})
	client.add("acme/root", "1.0.0", map[string]string{"acme/old": "^1.0"})
	client.add("acme/recent", "1.0.0", nil)
	client.add("acme/old", "1.0.0", nil)

	results := prefetchMetadata(context.Background(), client, []string{"acme/root"}, nil, nil)

	if _, ok := results["acme/recent"]; !ok {
		t.Fatal("expected acme/recent to be prefetched")
	}
	if _, ok := results["acme/old"]; ok {
		t.Fatal("did not expect acme/old to be prefetched from versions beyond scan limit")
	}
}

// TestPrefetchMetadataContextCancellation verifies that prefetch terminates
// promptly when the context is cancelled.
func TestPrefetchMetadataContextCancellation(t *testing.T) {
	client := newMockFetcher()
	client.add("acme/foo", "1.0.0", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Should not block or panic.
	prefetchMetadata(ctx, client, []string{"acme/foo"}, nil, nil)
}

// TestPrefetchMetadataEmptyRoots verifies that prefetch with no root packages
// returns an empty map without blocking.
func TestPrefetchMetadataEmptyRoots(t *testing.T) {
	client := newMockFetcher()

	results := prefetchMetadata(context.Background(), client, nil, nil, nil)
	if len(results) != 0 {
		t.Errorf("expected empty results, got %v", results)
	}
}

// TestPopulateVersionCacheOmitsNotFound verifies that ErrPackageNotFound
// entries are not placed in the version cache (to preserve solver ordering).
func TestPopulateVersionCacheOmitsNotFound(t *testing.T) {
	cache := make(map[string]candidateCacheEntry)
	prefetched := map[string]prefetchResult{
		"acme/found":   {versions: []packagist.VersionEntry{{Name: "acme/found", Version: "1.0.0"}}},
		"acme/missing": {err: packagist.ErrPackageNotFound},
	}
	populateVersionCache(cache, prefetched, false)

	if _, ok := cache["acme/found"]; !ok {
		t.Error("acme/found should be in cache")
	}
	if _, ok := cache["acme/missing"]; ok {
		t.Error("acme/missing should NOT be in cache (ErrPackageNotFound must be omitted)")
	}
}
