package cmd

import (
	"strings"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
)

func metadataClient(cj *composer.ComposerJSON) (packagist.Fetcher, error) {
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
		sources = append(sources, client)
	}

	packagistClient := packagist.NewClient("https://repo.packagist.org")
	packagistClient.SetAuth(auth)
	sources = append(sources, packagistClient)

	return packagist.NewChain(sources...), nil
}
