package resolver

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/version"
)

const defaultPrefetchWorkers = 8

// prefetchResult holds the fetched metadata for one package.
type prefetchResult struct {
	versions []packagist.VersionEntry
	err      error
}

// prefetchMetadata performs a BFS crawl of package metadata using parallel workers.
// It fetches all reachable packages (root + transitive) concurrently and returns when
// all work is done or the context is cancelled.
//
// Each package is fetched exactly once (tracked via the queued map). Transitive deps
// are discovered from NonPlatformRequire() on every version entry returned by the server,
// which may cause over-fetching (packages from versions that won't ultimately be selected),
// but that is acceptable given the parallelism gains.
func prefetchMetadata(ctx context.Context, client packagist.Fetcher, rootNames []string, progress func(string)) map[string]prefetchResult {
	results := make(map[string]prefetchResult)
	var mu sync.Mutex
	queued := make(map[string]bool, len(rootNames)*4)

	work := make(chan string, 512)
	var outstanding sync.WaitGroup

	// enqueue adds name to the work channel if it hasn't been queued yet.
	// outstanding.Add(1) is called synchronously before the goroutine send so
	// the closer goroutine never observes a zero count between enqueue and
	// delivery of the item to a worker.
	var enqueue func(name string)
	enqueue = func(name string) {
		mu.Lock()
		if queued[name] {
			mu.Unlock()
			return
		}
		queued[name] = true
		mu.Unlock()
		outstanding.Add(1)
		// Use a goroutine to send so that workers discovering transitive deps
		// cannot block indefinitely if the channel buffer is full while all
		// workers are also trying to enqueue.
		go func() { work <- name }()
	}

	// Start workers.
	var wg sync.WaitGroup
	for range defaultPrefetchWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range work {
				if ctx.Err() != nil {
					outstanding.Done()
					continue
				}
				if progress != nil {
					progress(name)
				}
				versions, err := client.GetPackage(ctx, name)

				mu.Lock()
				results[name] = prefetchResult{versions: versions, err: err}
				mu.Unlock()

				// Discover transitive dependencies and enqueue new ones.
				if err == nil {
					for _, v := range versions {
						for dep := range v.NonPlatformRequire() {
							enqueue(dep)
						}
					}
				}

				outstanding.Done()
			}
		}()
	}

	// Seed with root packages. This must happen before the closer goroutine
	// is started so that outstanding > 0 when outstanding.Wait() is first
	// called (otherwise Wait() returns immediately and closes the channel
	// before any work is dispatched).
	for _, name := range rootNames {
		enqueue(name)
	}

	// Closer: waits until all in-flight items are processed, then closes the
	// work channel so the worker range-loops terminate.
	// Started after seeding to guarantee outstanding >= len(rootNames) > 0
	// when Wait() is called (assuming at least one root package).
	go func() {
		outstanding.Wait()
		close(work)
	}()

	// Wait for all workers to finish.
	wg.Wait()
	return results
}

// populateVersionCache converts prefetch results into candidateCacheEntry values
// and stores them in the provided cache map.
//
// Packages that returned ErrPackageNotFound are intentionally omitted from the
// cache. Virtual packages (e.g. psr/log-implementation) may not exist on
// Packagist but can be satisfied via provide/replace from another resolved
// package. Caching a "not found" error here would give those packages an
// artificially low requirementScore (0), causing the solver to process them
// before the providing package is resolved and prematurely triggering a
// terminal error. Leaving them out of the cache lets the solver's normal
// deferred-resolution logic handle them.
func populateVersionCache(cache map[string]candidateCacheEntry, prefetched map[string]prefetchResult, minimumStability version.Stability, preferStable bool) {
	for name, result := range prefetched {
		if result.err != nil {
			if errors.Is(result.err, packagist.ErrPackageNotFound) {
				// Omit: let the solver handle via provide/replace or defer.
				continue
			}
			cache[name] = candidateCacheEntry{
				err: result.err,
			}
			continue
		}
		var candidates []candidate
		for _, entry := range result.versions {
			v, err := version.Parse(entry.Version)
			if err != nil {
				continue
			}
			if !v.StabilityAtLeast(minimumStability) {
				continue
			}
			candidates = append(candidates, candidate{entry: entry, version: v})
		}
		// Sort descending by version (highest first), matching getCandidates order.
		sortCandidates(candidates, preferStable)
		cache[name] = candidateCacheEntry{candidates: candidates}
	}
}

func sortCandidates(candidates []candidate, preferStable bool) {
	sort.Slice(candidates, func(i, j int) bool {
		if preferStable && candidates[i].version.Stability != candidates[j].version.Stability {
			return candidates[i].version.Stability > candidates[j].version.Stability
		}
		return version.Compare(candidates[i].version, candidates[j].version) > 0
	})
}
