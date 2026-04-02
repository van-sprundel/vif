package cmd

import (
	"fmt"

	"github.com/van-sprundel/vif/internal/composerauth"
)

func loadComposerAuth() (*composerauth.Config, error) {
	cfg, err := composerauth.Load()
	if err != nil {
		return nil, fmt.Errorf("composer auth: %w", err)
	}
	return cfg, nil
}
