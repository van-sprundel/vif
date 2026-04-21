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

// vendorPrefixSource is implemented by sources that can report which vendor
// prefixes they serve (e.g. after warmup).
type vendorPrefixSource interface {
	KnownVendorPrefixes() []string
}

// SeedPrefixExclusions pre-populates the chain's prefix skip table using
// warmup results from each source. For every vendor prefix that at least one
// source claims (hits > 0), all other sources that don't claim it get a
// synthetic miss recorded so shouldSkipPrefix returns true from the very first
// lookup — no wasted probe needed.
func (c *Chain) SeedPrefixExclusions() {
	// Collect known prefixes per source index.
	sourceKnown := make([]map[string]bool, len(c.sources))
	anyKnown := make(map[string]bool)
	for i, source := range c.sources {
		if vps, ok := source.(vendorPrefixSource); ok {
			prefixes := vps.KnownVendorPrefixes()
			if len(prefixes) > 0 {
				sourceKnown[i] = make(map[string]bool, len(prefixes))
				for _, p := range prefixes {
					sourceKnown[i][p] = true
					anyKnown[p] = true
				}
			}
		}
	}
	if len(anyKnown) == 0 {
		return
	}

	// For each claimed prefix, record a miss on sources that don't claim it.
	for prefix := range anyKnown {
		for i := range c.sources {
			if sourceKnown[i] != nil && sourceKnown[i][prefix] {
				continue // this source serves the prefix
			}
			key := prefixKey{sourceIdx: i, prefix: prefix}
			val, _ := c.prefixStates.LoadOrStore(key, &prefixState{})
			state := val.(*prefixState)
			state.misses.CompareAndSwap(0, 1)
		}
	}
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

	// Multiple eligible sources — query in parallel with early return.
	// When any source returns a hit, we return immediately instead of waiting
	// for all sources. This avoids blocking on slow "not found" responses from
	// sources that don't serve the package.
	type result struct {
		idx      int
		source   Fetcher
		versions []VersionEntry
		err      error
		duration time.Duration
	}
	lookupCtx, lookupCancel := context.WithCancel(ctx)
	defer lookupCancel()

	ch := make(chan result, len(candidates))
	for j, cand := range candidates {
		go func(j int, cand candidate) {
			// Apply probe gate within the goroutine so probes don't block parallelism.
			isProber := c.awaitOrStartProbe(lookupCtx, cand.idx, name)
			if !isProber && c.shouldSkipPrefix(cand.idx, name) {
				ch <- result{
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
				versions, err = devSource.GetPackageWithDev(lookupCtx, name, includeDev)
			} else {
				versions, err = cand.source.GetPackage(lookupCtx, name)
			}
			c.recordPrefixResult(cand.idx, name, err)
			if isProber {
				c.finishProbe(cand.idx, name)
			}
			ch <- result{
				idx:      cand.idx,
				source:   cand.source,
				versions: versions,
				err:      err,
				duration: time.Since(start),
			}
		}(j, cand)
	}

	// Return as soon as the first repository hits. Remaining goroutines may still
	// finish in the background, but the buffered channel prevents them from
	// blocking the caller once we've found the highest-priority successful source.
	var errs []error
	for range candidates {
		r := <-ch
		if c.trace != nil && r.duration > 0 {
			c.trace(LookupTrace{
				Source:   repositoryLabel(r.source),
				Package:  name,
				Duration: r.duration,
				Err:      r.err,
			})
		}
		if r.err == nil {
			lookupCancel()
			return r.versions, nil
		}
		errs = append(errs, r.err)
	}

	// No hit — classify errors.
	var lastNotFound, lastAuthErr, lastTransientErr error
	for _, err := range errs {
		if errors.Is(err, ErrPackageNotFound) {
			lastNotFound = err
		} else if errors.Is(err, ErrAuthRequired) {
			lastAuthErr = err
		} else if isTransientRepositoryError(err) {
			lastTransientErr = err
		}
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

func (c *Chain) GetSolverPackage(ctx context.Context, name string, includeDev bool) (SolverPackageRecord, error) {
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
		return SolverPackageRecord{}, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
	}
	if len(candidates) == 1 {
		return c.fetchSolverFromSource(ctx, candidates[0].idx, candidates[0].source, name, includeDev)
	}

	type result struct {
		idx      int
		source   Fetcher
		record   SolverPackageRecord
		err      error
		duration time.Duration
	}
	lookupCtx, lookupCancel := context.WithCancel(ctx)
	defer lookupCancel()

	ch := make(chan result, len(candidates))
	for _, cand := range candidates {
		go func(cand candidate) {
			isProber := c.awaitOrStartProbe(lookupCtx, cand.idx, name)
			if !isProber && c.shouldSkipPrefix(cand.idx, name) {
				ch <- result{idx: cand.idx, source: cand.source, err: ErrPackageNotFound}
				return
			}
			start := time.Now()
			record, err := fetchSolverFromFetcher(lookupCtx, cand.source, name, includeDev)
			c.recordPrefixResult(cand.idx, name, err)
			if isProber {
				c.finishProbe(cand.idx, name)
			}
			ch <- result{
				idx:      cand.idx,
				source:   cand.source,
				record:   record,
				err:      err,
				duration: time.Since(start),
			}
		}(cand)
	}

	var errs []error
	for range candidates {
		r := <-ch
		if c.trace != nil && r.duration > 0 {
			c.trace(LookupTrace{
				Source:   repositoryLabel(r.source),
				Package:  name,
				Duration: r.duration,
				Err:      r.err,
			})
		}
		if r.err == nil {
			lookupCancel()
			return r.record, nil
		}
		errs = append(errs, r.err)
	}
	return SolverPackageRecord{}, classifyLookupErrors(errs, name)
}

func (c *Chain) GetPackageVersion(ctx context.Context, name, selectedVersion string, includeDev bool) (VersionEntry, error) {
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
		return VersionEntry{}, fmt.Errorf("%w: %s@%s", ErrPackageNotFound, name, selectedVersion)
	}
	if len(candidates) == 1 {
		return c.fetchVersionFromSource(ctx, candidates[0].idx, candidates[0].source, name, selectedVersion, includeDev)
	}

	type result struct {
		idx      int
		source   Fetcher
		entry    VersionEntry
		err      error
		duration time.Duration
	}
	lookupCtx, lookupCancel := context.WithCancel(ctx)
	defer lookupCancel()

	ch := make(chan result, len(candidates))
	for _, cand := range candidates {
		go func(cand candidate) {
			isProber := c.awaitOrStartProbe(lookupCtx, cand.idx, name)
			if !isProber && c.shouldSkipPrefix(cand.idx, name) {
				ch <- result{idx: cand.idx, source: cand.source, err: ErrPackageNotFound}
				return
			}
			start := time.Now()
			entry, err := fetchVersionFromFetcher(lookupCtx, cand.source, name, selectedVersion, includeDev)
			c.recordPrefixResult(cand.idx, name, err)
			if isProber {
				c.finishProbe(cand.idx, name)
			}
			ch <- result{
				idx:      cand.idx,
				source:   cand.source,
				entry:    entry,
				err:      err,
				duration: time.Since(start),
			}
		}(cand)
	}

	var errs []error
	for range candidates {
		r := <-ch
		if c.trace != nil && r.duration > 0 {
			c.trace(LookupTrace{
				Source:   repositoryLabel(r.source),
				Package:  name,
				Duration: r.duration,
				Err:      r.err,
			})
		}
		if r.err == nil {
			lookupCancel()
			return r.entry, nil
		}
		errs = append(errs, r.err)
	}
	return VersionEntry{}, classifyLookupErrors(errs, name)
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

func (c *Chain) fetchSolverFromSource(ctx context.Context, idx int, source Fetcher, name string, includeDev bool) (SolverPackageRecord, error) {
	isProber := c.awaitOrStartProbe(ctx, idx, name)
	if !isProber && c.shouldSkipPrefix(idx, name) {
		return SolverPackageRecord{}, ErrPackageNotFound
	}
	start := time.Now()
	record, err := fetchSolverFromFetcher(ctx, source, name, includeDev)
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
	return record, err
}

func (c *Chain) fetchVersionFromSource(ctx context.Context, idx int, source Fetcher, name, selectedVersion string, includeDev bool) (VersionEntry, error) {
	isProber := c.awaitOrStartProbe(ctx, idx, name)
	if !isProber && c.shouldSkipPrefix(idx, name) {
		return VersionEntry{}, ErrPackageNotFound
	}
	start := time.Now()
	entry, err := fetchVersionFromFetcher(ctx, source, name, selectedVersion, includeDev)
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
	return entry, err
}

func fetchSolverFromFetcher(ctx context.Context, source Fetcher, name string, includeDev bool) (SolverPackageRecord, error) {
	if solverSource, ok := source.(SolverFetcher); ok {
		return solverSource.GetSolverPackage(ctx, name, includeDev)
	}
	if devSource, ok := source.(DevFetcher); ok {
		versions, err := devSource.GetPackageWithDev(ctx, name, includeDev)
		if err != nil {
			return SolverPackageRecord{}, err
		}
		return NormalizeForSolver(repositoryLabel(source), name, versions, includeDev, "", nil), nil
	}
	versions, err := source.GetPackage(ctx, name)
	if err != nil {
		return SolverPackageRecord{}, err
	}
	return NormalizeForSolver(repositoryLabel(source), name, versions, includeDev, "", nil), nil
}

func fetchVersionFromFetcher(ctx context.Context, source Fetcher, name, selectedVersion string, includeDev bool) (VersionEntry, error) {
	if versionSource, ok := source.(PackageVersionFetcher); ok {
		return versionSource.GetPackageVersion(ctx, name, selectedVersion, includeDev)
	}
	var (
		versions []VersionEntry
		err      error
	)
	if devSource, ok := source.(DevFetcher); ok {
		versions, err = devSource.GetPackageWithDev(ctx, name, includeDev)
	} else {
		versions, err = source.GetPackage(ctx, name)
	}
	if err != nil {
		return VersionEntry{}, err
	}
	for _, entry := range versions {
		if entry.Version == selectedVersion {
			return entry, nil
		}
	}
	return VersionEntry{}, fmt.Errorf("%w: %s@%s", ErrPackageNotFound, name, selectedVersion)
}

func classifyLookupErrors(results []error, name string) error {
	var lastNotFound, lastAuthErr, lastTransientErr error
	for _, err := range results {
		if errors.Is(err, ErrPackageNotFound) {
			lastNotFound = err
		} else if errors.Is(err, ErrAuthRequired) {
			lastAuthErr = err
		} else if isTransientRepositoryError(err) {
			lastTransientErr = err
		}
	}
	if lastAuthErr != nil {
		return lastAuthErr
	}
	if lastTransientErr != nil {
		return lastTransientErr
	}
	if lastNotFound != nil {
		return lastNotFound
	}
	return fmt.Errorf("%w: %s", ErrPackageNotFound, name)
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

// shouldSkipPrefix returns true if the given source should be skipped for the
// vendor prefix of name. A source is skipped when:
//  1. It has only returned "not found" for that prefix (misses > 0, hits == 0), OR
//  2. Another source has proven it serves the prefix (hits > 0) AND this source
//     has at least one miss and zero hits — meaning the prefix is exclusively
//     owned by the other source.
func (c *Chain) shouldSkipPrefix(sourceIdx int, name string) bool {
	prefix := vendorPrefix(name)
	if prefix == "" {
		return false
	}

	val, ok := c.prefixStates.Load(prefixKey{sourceIdx: sourceIdx, prefix: prefix})
	if ok {
		state := val.(*prefixState)
		if state.misses.Load() > 0 && state.hits.Load() == 0 {
			return true
		}
	}

	// If this source has never been probed for this prefix but another source
	// already claims it, skip probing entirely.
	if !ok {
		for i := range c.sources {
			if i == sourceIdx {
				continue
			}
			otherVal, otherOk := c.prefixStates.Load(prefixKey{sourceIdx: i, prefix: prefix})
			if otherOk {
				otherState := otherVal.(*prefixState)
				if otherState.hits.Load() > 0 {
					// Another source serves this prefix. Pre-record a miss here
					// so subsequent lookups skip without re-checking.
					newVal, _ := c.prefixStates.LoadOrStore(prefixKey{sourceIdx: sourceIdx, prefix: prefix}, &prefixState{})
					newVal.(*prefixState).misses.CompareAndSwap(0, 1)
					return true
				}
			}
		}
	}

	return false
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
