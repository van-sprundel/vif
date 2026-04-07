package cmd

import (
	"context"
	"strings"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
)

type routedFetcher struct {
	packagist.Fetcher
	label   string
	matcher func(string) bool
}

func (r routedFetcher) RepositoryLabel() string {
	return r.label
}

func (r routedFetcher) MatchPackage(name string) bool {
	if r.matcher == nil {
		return true
	}
	return r.matcher(name)
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
			matcher: repositoryMatcher(repo.URL),
		})
	}

	packagistClient := packagist.NewClient("https://repo.packagist.org")
	packagistClient.SetAuth(auth)
	if mc != nil {
		packagistClient.SetMetadataCache(mc)
	}
	sources = append(sources, routedFetcher{
		Fetcher: packagistClient,
		label:   packagistClient.RepositoryLabel(),
		matcher: repositoryMatcher(packagistClient.RepositoryLabel()),
	})

	return packagist.NewChain(sources...), nil
}
