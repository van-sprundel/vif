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
func (c *Chain) GetPackageWithDev(ctx context.Context, name string, includeDev bool) ([]VersionEntry, error) {
	var lastNotFound error
	var lastAuthErr error
	var lastTransientErr error

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
		c.recordPrefixResult(i, name, err)
		if c.trace != nil {
			c.trace(LookupTrace{
				Source:   repositoryLabel(source),
				Package:  name,
				Duration: time.Since(start),
				Err:      err,
			})
		}
		if err == nil {
			return versions, nil
		}
		if errors.Is(err, ErrPackageNotFound) {
			lastNotFound = err
			continue
		}
		if errors.Is(err, ErrAuthRequired) {
			lastAuthErr = err
			continue
		}
		if isTransientRepositoryError(err) {
			lastTransientErr = err
			continue
		}
		return nil, err
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
