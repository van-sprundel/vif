package packagist

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// Chain tries multiple Composer-compatible repositories in order.
type Chain struct {
	sources []Fetcher
}

// NewChain builds a metadata lookup chain.
func NewChain(sources ...Fetcher) *Chain {
	return &Chain{sources: sources}
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

	for _, source := range c.sources {
		if source == nil {
			continue
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
