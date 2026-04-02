package packagist

import (
	"context"
	"errors"
	"fmt"
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
	var lastNotFound error
	var lastAuthErr error

	for _, source := range c.sources {
		if source == nil {
			continue
		}
		versions, err := source.GetPackage(ctx, name)
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
		return nil, err
	}

	if lastAuthErr != nil {
		return nil, lastAuthErr
	}
	if lastNotFound != nil {
		return nil, lastNotFound
	}
	return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
}
