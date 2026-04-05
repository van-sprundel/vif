package resolver

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/version"
)

var defaultPrefetchWorkers = min(runtime.NumCPU(), 16)
var prefetchDependencyScanVersionLimit = 32

// prefetchResult holds the fetched metadata for one package.
type prefetchResult struct {
	versions []packagist.VersionEntry
	err      error
}

// prefetchMetadata performs a bounded BFS crawl of package metadata using
// parallel workers. It returns when all queued work is done or the context is
// cancelled.
//
// Each package is fetched exactly once (tracked via the queued map). Transitive deps
// are discovered from only the first N version entries returned by the server
// (N is prefetchDependencyScanVersionLimit). Scanning all versions can explode
// on large ecosystems and dominate update time. Missing transitive packages are
// still safe: the solver falls back to on-demand fetches on cache misses.
func prefetchMetadata(ctx context.Context, client packagist.Fetcher, rootNames []string, progress func(string), lookupDone func(string, time.Duration, error)) map[string]prefetchResult {
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
				start := time.Now()
				versions, err := client.GetPackage(ctx, name)
				if lookupDone != nil {
					lookupDone(name, time.Since(start), err)
				}

				mu.Lock()
				results[name] = prefetchResult{versions: versions, err: err}
				mu.Unlock()

				// Discover transitive dependencies and enqueue new ones.
				if err == nil {
					scan := versions
					if len(scan) > prefetchDependencyScanVersionLimit {
						scan = scan[:prefetchDependencyScanVersionLimit]
					}
					for _, v := range scan {
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
//
// Note: Stability filtering is NOT applied here. All parseable versions are cached.
// Stability filtering happens at retrieval time via getCandidates to support
// per-requirement stability overrides (e.g., @dev, @alpha flags).
func populateVersionCache(cache map[string]candidateCacheEntry, prefetched map[string]prefetchResult, preferStable bool) {
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
			candidates = append(candidates, candidate{
				entry:        entry,
				version:      v,
				dependencies: parseDependencyRequirements(entry),
				conflicts:    parseConflictRequirements(entry),
			})
		}
		// Sort descending by version (highest first), matching getCandidates order.
		sortCandidates(candidates, preferStable)
		cache[name] = candidateCacheEntry{candidates: candidates}
	}
}

// injectLockedEntries adds locked package versions to the cache for packages
// that have no Packagist releases (VCS-only packages). This allows the resolver
// to preserve them from the lockfile instead of failing.
func injectLockedEntries(cache map[string]candidateCacheEntry, prefetched map[string]prefetchResult, lockedEntries map[string]packagist.VersionEntry, preferStable bool) {
	if len(lockedEntries) == 0 {
		return
	}

	for name, entry := range lockedEntries {
		// Check if Packagist returned an empty version list for this package.
		result, fetched := prefetched[name]
		if !fetched {
			// Package wasn't even fetched (not in dependency tree) - skip.
			continue
		}
		if result.err != nil && !errors.Is(result.err, packagist.ErrPackageNotFound) {
			// Fetch error other than "not found" - don't override.
			continue
		}
		if len(result.versions) > 0 {
			// Package has Packagist versions - don't need to inject locked entry.
			continue
		}

		// Parse the locked version.
		v, err := version.Parse(entry.Version)
		if err != nil {
			continue
		}

		// Inject the locked entry as the only candidate.
		candidates := []candidate{{
			entry:        entry,
			version:      v,
			dependencies: parseDependencyRequirements(entry),
			conflicts:    parseConflictRequirements(entry),
		}}
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

func preferLocked(candidates []candidate, lockedVersion string) []candidate {
	if lockedVersion == "" || len(candidates) < 2 {
		return candidates
	}

	for i, candidate := range candidates {
		if candidate.entry.Version != lockedVersion {
			continue
		}
		if i == 0 {
			return candidates
		}

		preferred := candidate
		copy(candidates[1:i+1], candidates[0:i])
		candidates[0] = preferred
		return candidates
	}

	return candidates
}
