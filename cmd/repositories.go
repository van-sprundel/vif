package cmd

import (
	"context"
	"strings"
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

	for _, warmer := range warmers {
		go func(w scopeWarmer) {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			w.WarmupMatchScope(ctx)
		}(warmer)
	}

	return packagist.NewChain(sources...), nil
}
