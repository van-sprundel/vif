package packagist

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Chain tries multiple Composer-compatible repositories in order.
// It dynamically learns which vendor prefixes each source serves and skips
// sources that have only returned "not found" for a given prefix.
type Chain struct {
	sources []Fetcher
	trace   func(LookupTrace)
	// prefixStates tracks hit/miss counts per (source index, vendor prefix).
	prefixStates sync.Map // key: prefixKey → *prefixState
	// prefixProbes gates concurrent lookups so only one goroutine per
	// (source, prefix) does the initial HTTP request; others wait and reuse
	// the recorded hit/miss result.
	prefixProbes sync.Map // key: prefixKey → chan struct{}
}

type prefixKey struct {
	sourceIdx int
	prefix    string
}

type prefixState struct {
	hits   atomic.Int32
	misses atomic.Int32
}

type packageMatcher interface {
	MatchPackage(name string) bool
}

// LookupTrace describes one repository attempt for a package metadata lookup.
type LookupTrace struct {
	Source   string
	Package  string
	Duration time.Duration
	Err      error
}

// NewChain builds a metadata lookup chain.
func NewChain(sources ...Fetcher) *Chain {
	return &Chain{sources: sources}
}

// SetLookupTrace installs an optional callback for per-repository lookup events.
func (c *Chain) SetLookupTrace(fn func(LookupTrace)) {
	c.trace = fn
}

// GetPackage returns the first successful package hit across configured sources.
func (c *Chain) GetPackage(ctx context.Context, name string) ([]VersionEntry, error) {
	return c.GetPackageWithDev(ctx, name, true)
}

// GetPackageWithDev returns the first successful package hit across configured
// sources, optionally skipping separate ~dev metadata lookups.
// It queries all eligible sources in parallel and returns the result from the
// highest-priority source that succeeds.
func (c *Chain) GetPackageWithDev(ctx context.Context, name string, includeDev bool) ([]VersionEntry, error) {
	// Collect eligible sources (non-blocking checks only).
	type candidate struct {
		idx    int
		source Fetcher
	}
	var candidates []candidate
	for i, source := range c.sources {
		if source == nil {
			continue
		}
		if matcher, ok := source.(packageMatcher); ok && !matcher.MatchPackage(name) {
			continue
		}
		if c.shouldSkipPrefix(i, name) {
			continue
		}
		candidates = append(candidates, candidate{idx: i, source: source})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
	}

	// Single eligible source — skip goroutine overhead.
	if len(candidates) == 1 {
		cand := candidates[0]
		return c.fetchFromSource(ctx, cand.idx, cand.source, name, includeDev)
	}

	// Multiple eligible sources — query in parallel.
	type result struct {
		idx      int
		source   Fetcher
		versions []VersionEntry
		err      error
		duration time.Duration
	}
	results := make([]result, len(candidates))
	var wg sync.WaitGroup
	wg.Add(len(candidates))
	for j, cand := range candidates {
		go func(j int, cand candidate) {
			defer wg.Done()
			// Apply probe gate within the goroutine so probes don't block parallelism.
			isProber := c.awaitOrStartProbe(ctx, cand.idx, name)
			if !isProber && c.shouldSkipPrefix(cand.idx, name) {
				results[j] = result{
					idx:    cand.idx,
					source: cand.source,
					err:    ErrPackageNotFound,
				}
				return
			}
			start := time.Now()
			var (
				versions []VersionEntry
				err      error
			)
			if devSource, ok := cand.source.(DevFetcher); ok {
				versions, err = devSource.GetPackageWithDev(ctx, name, includeDev)
			} else {
				versions, err = cand.source.GetPackage(ctx, name)
			}
			c.recordPrefixResult(cand.idx, name, err)
			if isProber {
				c.finishProbe(cand.idx, name)
			}
			results[j] = result{
				idx:      cand.idx,
				source:   cand.source,
				versions: versions,
				err:      err,
				duration: time.Since(start),
			}
		}(j, cand)
	}
	wg.Wait()

	// Emit traces and pick the best result in priority order.
	var lastNotFound, lastAuthErr, lastTransientErr error
	for _, r := range results {
		if c.trace != nil && r.duration > 0 {
			c.trace(LookupTrace{
				Source:   repositoryLabel(r.source),
				Package:  name,
				Duration: r.duration,
				Err:      r.err,
			})
		}
		if r.err == nil {
			return r.versions, nil
		}
		if errors.Is(r.err, ErrPackageNotFound) {
			lastNotFound = r.err
			continue
		}
		if errors.Is(r.err, ErrAuthRequired) {
			lastAuthErr = r.err
			continue
		}
		if isTransientRepositoryError(r.err) {
			lastTransientErr = r.err
			continue
		}
		// Hard error from a source — but keep checking higher-priority hits.
	}

	if lastAuthErr != nil {
		return nil, lastAuthErr
	}
	if lastTransientErr != nil {
		return nil, lastTransientErr
	}
	if lastNotFound != nil {
		return nil, lastNotFound
	}
	return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
}

// fetchFromSource performs a single-source lookup with probe gating and tracing.
func (c *Chain) fetchFromSource(ctx context.Context, idx int, source Fetcher, name string, includeDev bool) ([]VersionEntry, error) {
	isProber := c.awaitOrStartProbe(ctx, idx, name)
	if !isProber && c.shouldSkipPrefix(idx, name) {
		return nil, ErrPackageNotFound
	}
	start := time.Now()
	var (
		versions []VersionEntry
		err      error
	)
	if devSource, ok := source.(DevFetcher); ok {
		versions, err = devSource.GetPackageWithDev(ctx, name, includeDev)
	} else {
		versions, err = source.GetPackage(ctx, name)
	}
	c.recordPrefixResult(idx, name, err)
	if isProber {
		c.finishProbe(idx, name)
	}
	if c.trace != nil {
		c.trace(LookupTrace{
			Source:   repositoryLabel(source),
			Package:  name,
			Duration: time.Since(start),
			Err:      err,
		})
	}
	return versions, err
}

func isTransientRepositoryError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func repositoryLabel(source Fetcher) string {
	if named, ok := source.(interface{ RepositoryLabel() string }); ok {
		return named.RepositoryLabel()
	}
	return fmt.Sprintf("%T", source)
}

// vendorPrefix returns the part before the first "/" in a package name.
func vendorPrefix(name string) string {
	if idx := strings.Index(name, "/"); idx > 0 {
		return name[:idx]
	}
	return ""
}

// shouldSkipPrefix returns true if the given source has only returned "not found"
// for the vendor prefix of name (i.e. misses > 0 and hits == 0).
func (c *Chain) shouldSkipPrefix(sourceIdx int, name string) bool {
	prefix := vendorPrefix(name)
	if prefix == "" {
		return false
	}
	val, ok := c.prefixStates.Load(prefixKey{sourceIdx: sourceIdx, prefix: prefix})
	if !ok {
		return false
	}
	state := val.(*prefixState)
	return state.misses.Load() > 0 && state.hits.Load() == 0
}

// awaitOrStartProbe coordinates concurrent lookups for the same (source, prefix).
// Returns true if the caller should proceed with the actual lookup (is the "prober").
// Returns false if another goroutine already probed — the caller should recheck
// shouldSkipPrefix to see if the result was a miss.
func (c *Chain) awaitOrStartProbe(ctx context.Context, sourceIdx int, name string) bool {
	prefix := vendorPrefix(name)
	if prefix == "" {
		return true
	}
	key := prefixKey{sourceIdx: sourceIdx, prefix: prefix}

	ch := make(chan struct{})
	existing, loaded := c.prefixProbes.LoadOrStore(key, ch)
	if !loaded {
		return true // we're the first — proceed with lookup
	}
	// Wait for the existing probe to finish (or context cancellation).
	select {
	case <-existing.(chan struct{}):
	case <-ctx.Done():
	}
	return false
}

// finishProbe signals waiting goroutines that the probe for (source, prefix) is done.
func (c *Chain) finishProbe(sourceIdx int, name string) {
	prefix := vendorPrefix(name)
	if prefix == "" {
		return
	}
	key := prefixKey{sourceIdx: sourceIdx, prefix: prefix}
	if val, ok := c.prefixProbes.Load(key); ok {
		close(val.(chan struct{}))
	}
}

// recordPrefixResult updates the prefix hit/miss tracking after a lookup attempt.
// Transient errors are not recorded to avoid false skips.
func (c *Chain) recordPrefixResult(sourceIdx int, name string, err error) {
	prefix := vendorPrefix(name)
	if prefix == "" {
		return
	}
	key := prefixKey{sourceIdx: sourceIdx, prefix: prefix}
	val, _ := c.prefixStates.LoadOrStore(key, &prefixState{})
	state := val.(*prefixState)
	if err == nil {
		state.hits.Add(1)
	} else if errors.Is(err, ErrPackageNotFound) {
		state.misses.Add(1)
	}
}
