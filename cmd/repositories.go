package cmd

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
)

type routedFetcher struct {
	packagist.Fetcher
	label string
}

type scopeWarmer interface {
	WarmupMatchScope(context.Context)
}

func (r routedFetcher) RepositoryLabel() string {
	return r.label
}

func (r routedFetcher) GetPackageWithDev(ctx context.Context, name string, includeDev bool) ([]packagist.VersionEntry, error) {
	if devFetcher, ok := r.Fetcher.(packagist.DevFetcher); ok {
		return devFetcher.GetPackageWithDev(ctx, name, includeDev)
	}
	return r.Fetcher.GetPackage(ctx, name)
}

func (r routedFetcher) GetSolverPackage(ctx context.Context, name string, includeDev bool) (packagist.SolverPackageRecord, error) {
	if solverFetcher, ok := r.Fetcher.(packagist.SolverFetcher); ok {
		return solverFetcher.GetSolverPackage(ctx, name, includeDev)
	}
	versions, err := r.GetPackageWithDev(ctx, name, includeDev)
	if err != nil {
		return packagist.SolverPackageRecord{}, err
	}
	return packagist.NormalizeForSolver(r.label, name, versions, includeDev, "", nil), nil
}

func (r routedFetcher) GetPackageVersion(ctx context.Context, name, selectedVersion string, includeDev bool) (packagist.VersionEntry, error) {
	if versionFetcher, ok := r.Fetcher.(packagist.PackageVersionFetcher); ok {
		return versionFetcher.GetPackageVersion(ctx, name, selectedVersion, includeDev)
	}
	versions, err := r.GetPackageWithDev(ctx, name, includeDev)
	if err != nil {
		return packagist.VersionEntry{}, err
	}
	for _, entry := range versions {
		if entry.Version == selectedVersion {
			return entry, nil
		}
	}
	return packagist.VersionEntry{}, packagist.ErrPackageNotFound
}

func (r routedFetcher) KnownVendorPrefixes() []string {
	type prefixer interface {
		KnownVendorPrefixes() []string
	}
	if p, ok := r.Fetcher.(prefixer); ok {
		return p.KnownVendorPrefixes()
	}
	return nil
}

func metadataClient(cj *composer.ComposerJSON, mc packagist.MetadataCache) (packagist.Fetcher, error) {
	auth, err := loadComposerAuth()
	if err != nil {
		return nil, err
	}

	var sources []packagist.Fetcher
	var warmers []scopeWarmer
	for _, repo := range cj.Repositories {
		if strings.ToLower(strings.TrimSpace(repo.Type)) != "composer" {
			continue
		}
		if strings.TrimSpace(repo.URL) == "" {
			continue
		}
		client := packagist.NewClient(repo.URL)
		client.SetAuth(auth)
		if mc != nil {
			client.SetMetadataCache(mc)
		}
		sources = append(sources, routedFetcher{
			Fetcher: client,
			label:   client.RepositoryLabel(),
		})
		warmers = append(warmers, client)
	}

	packagistClient := packagist.NewClient("https://repo.packagist.org")
	packagistClient.SetAuth(auth)
	if mc != nil {
		packagistClient.SetMetadataCache(mc)
	}
	sources = append(sources, routedFetcher{
		Fetcher: packagistClient,
		label:   packagistClient.RepositoryLabel(),
	})
	warmers = append(warmers, packagistClient)

	var wg sync.WaitGroup
	for _, warmer := range warmers {
		wg.Add(1)
		go func(w scopeWarmer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			w.WarmupMatchScope(ctx)
		}(warmer)
	}
	wg.Wait()

	chain := packagist.NewChain(sources...)
	chain.SeedPrefixExclusions()
	return chain, nil
}
