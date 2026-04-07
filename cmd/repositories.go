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

func repositoryMatcher(repoURL string) func(string) bool {
	lowerURL := strings.ToLower(strings.TrimSpace(repoURL))

	switch {
	case strings.Contains(lowerURL, "packages.drupal.org"):
		return func(name string) bool {
			return strings.HasPrefix(name, "drupal/")
		}
	case strings.Contains(lowerURL, "asset-packagist.org"):
		return func(name string) bool {
			return strings.HasPrefix(name, "bower-asset/") || strings.HasPrefix(name, "npm-asset/")
		}
	case strings.Contains(lowerURL, "satis.urban-heroes.nl"),
		strings.Contains(lowerURL, "gitlab.com/api/v4/group/13017208/-/packages/composer"):
		return func(name string) bool {
			return strings.HasPrefix(name, "urbanheroes-")
		}
	case strings.Contains(lowerURL, "repo.packagist.org"):
		return func(name string) bool {
			if strings.HasPrefix(name, "bower-asset/") || strings.HasPrefix(name, "npm-asset/") {
				return false
			}
			if strings.HasPrefix(name, "urbanheroes-") {
				return false
			}
			return !strings.HasPrefix(name, "drupal/")
		}
	default:
		return nil
	}
}
