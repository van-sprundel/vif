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

// prefetchInto performs the same BFS crawl as prefetchMetadata but populates
// the shared version cache incrementally as each package is fetched, instead of
// collecting all results first. This allows the solver to start working
// concurrently using cache entries that are ready while remaining packages are
// still being fetched.
func prefetchInto(ctx context.Context, client packagist.Fetcher, rootNames []string,
	cache map[string]candidateCacheEntry, cacheMu *sync.RWMutex,
	preferStable bool, minimumStability version.Stability, lockedEntries map[string]packagist.VersionEntry,
	progress func(string), lookupDone func(string, time.Duration, error)) {
	includeDev := minimumStability == version.Dev

	var mu sync.Mutex
	queued := make(map[string]bool, len(rootNames)*4)

	work := make(chan string, 512)
	var outstanding sync.WaitGroup

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
		go func() { work <- name }()
	}

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
				record, hasSolver, solverErr := fetchSolverPackage(ctx, client, name, includeDev)
				var (
					versions []packagist.VersionEntry
					err      error
				)
				if hasSolver {
					err = solverErr
				} else {
					versions, err = fetchPackageVersions(ctx, client, name, includeDev)
				}
				if lookupDone != nil {
					lookupDone(name, time.Since(start), err)
				}

				// Populate cache immediately as each result arrives.
				var entry *candidateCacheEntry
				if hasSolver {
					entry = buildCacheEntryFromSolverRecord(name, record, err, preferStable, includeDev, lockedEntries)
				} else {
					entry = buildCacheEntry(name, versions, err, preferStable, includeDev, lockedEntries)
				}
				if entry != nil {
					cacheMu.Lock()
					if _, exists := cache[name]; !exists {
						cache[name] = *entry
					}
					cacheMu.Unlock()
				}

				// Discover transitive dependencies and enqueue new ones.
				if err == nil {
					if hasSolver {
						scan := record.Versions
						if len(scan) > prefetchDependencyScanVersionLimit {
							scan = scan[:prefetchDependencyScanVersionLimit]
						}
						for _, v := range scan {
							for dep := range v.Require {
								enqueue(dep)
							}
						}
					} else {
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
				}

				outstanding.Done()
			}
		}()
	}

	for _, name := range rootNames {
		enqueue(name)
	}

	go func() {
		outstanding.Wait()
		close(work)
	}()

	wg.Wait()
}

// buildCacheEntry converts a single package fetch result into a candidateCacheEntry.
// Returns nil for ErrPackageNotFound without a locked fallback (virtual packages
// handled by the solver via provide/replace).
func buildCacheEntry(name string, versions []packagist.VersionEntry, err error, preferStable bool, includeDev bool, lockedEntries map[string]packagist.VersionEntry) *candidateCacheEntry {
	if err != nil {
		if errors.Is(err, packagist.ErrPackageNotFound) {
			// Try locked entry for VCS-only packages.
			if entry, ok := lockedEntries[name]; ok {
				if cand, ok := candidateFromVersionEntry(entry, true); ok {
					candidates := []candidate{cand}
					sortCandidates(candidates, preferStable)
					return &candidateCacheEntry{candidates: candidates, hasDev: includeDev}
				}
			}
			return nil
		}
		return &candidateCacheEntry{err: err, hasDev: includeDev}
	}

	var candidates []candidate
	for _, entry := range versions {
		cand, ok := candidateFromVersionEntry(entry, true)
		if !ok {
			continue
		}
		candidates = append(candidates, cand)
	}

	// Inject locked entry if Packagist returned no usable versions.
	if len(candidates) == 0 {
		if entry, ok := lockedEntries[name]; ok {
			if cand, ok := candidateFromVersionEntry(entry, true); ok {
				candidates = []candidate{cand}
			}
		}
	}

	sortCandidates(candidates, preferStable)
	return &candidateCacheEntry{candidates: candidates, hasDev: includeDev}
}

func buildCacheEntryFromSolverRecord(name string, record packagist.SolverPackageRecord, err error, preferStable bool, includeDev bool, lockedEntries map[string]packagist.VersionEntry) *candidateCacheEntry {
	if err != nil {
		if errors.Is(err, packagist.ErrPackageNotFound) {
			if entry, ok := lockedEntries[name]; ok {
				if cand, ok := candidateFromVersionEntry(entry, true); ok {
					candidates := []candidate{cand}
					sortCandidates(candidates, preferStable)
					return &candidateCacheEntry{candidates: candidates, hasDev: includeDev}
				}
			}
			return nil
		}
		return &candidateCacheEntry{err: err, hasDev: includeDev}
	}

	candidates := candidatesFromSolverRecord(name, record, func(packagist.VersionEntry) bool { return true })
	if len(candidates) == 0 {
		if entry, ok := lockedEntries[name]; ok {
			if cand, ok := candidateFromVersionEntry(entry, true); ok {
				candidates = []candidate{cand}
			}
		}
	}
	sortCandidates(candidates, preferStable)
	return &candidateCacheEntry{candidates: candidates, hasDev: includeDev}
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
			cand, ok := candidateFromVersionEntry(entry, true)
			if !ok {
				continue
			}
			candidates = append(candidates, cand)
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

		cand, ok := candidateFromVersionEntry(entry, true)
		if !ok {
			continue
		}

		// Inject the locked entry as the only candidate.
		candidates := []candidate{cand}
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
