package cmd

import (
	"context"
	"testing"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
)

type staticFetcher map[string][]packagist.VersionEntry

func (f staticFetcher) GetPackage(_ context.Context, name string) ([]packagist.VersionEntry, error) {
	return f[name], nil
}

func TestResolveRestrictedPackagesFromSymfonyMetaPackage(t *testing.T) {
	cj := &composer.ComposerJSON{}
	cj.Extra.Symfony.Require = "6.4.*"

	fetcher := staticFetcher{
		"symfony/symfony": {
			{
				Name:    "symfony/symfony",
				Version: "v7.4.8",
				Replace: map[string]string{
					"symfony/config": "self.version",
				},
			},
			{
				Name:    "symfony/symfony",
				Version: "v6.4.34",
				Replace: map[string]string{
					"symfony/config":      "self.version",
					"symfony/http-kernel": "self.version",
				},
			},
		},
	}

	restricted, restriction, err := resolveRestrictedPackages(context.Background(), fetcher, cj)
	if err != nil {
		t.Fatalf("resolveRestrictedPackages: %v", err)
	}

	if restriction != "6.4.*" {
		t.Fatalf("restriction = %q, want 6.4.*", restriction)
	}
	if _, ok := restricted["symfony/config"]; !ok {
		t.Fatalf("expected symfony/config in restricted set")
	}
	if _, ok := restricted["symfony/http-kernel"]; !ok {
		t.Fatalf("expected symfony/http-kernel in restricted set")
	}
}

func TestResolveRestrictedPackagesNoSymfonyRequirement(t *testing.T) {
	cj := &composer.ComposerJSON{}

	restricted, restriction, err := resolveRestrictedPackages(context.Background(), staticFetcher{}, cj)
	if err != nil {
		t.Fatalf("resolveRestrictedPackages: %v", err)
	}
	if restricted != nil {
		t.Fatalf("restricted = %v, want nil", restricted)
	}
	if restriction != "" {
		t.Fatalf("restriction = %q, want empty", restriction)
	}
}

func TestFormatPackageList(t *testing.T) {
	tests := []struct {
		packages []string
		want     string
	}{
		{[]string{"a"}, "a"},
		{[]string{"a", "b", "c"}, "a, b, c"},
		{[]string{"a", "b", "c", "d", "e"}, "a, b, c, d, e"},
		{[]string{"a", "b", "c", "d", "e", "f"}, "a, b, c, d, e and 1 more"},
		{[]string{"a", "b", "c", "d", "e", "f", "g", "h"}, "a, b, c, d, e and 3 more"},
	}

	for _, tt := range tests {
		got := formatPackageList(tt.packages)
		if got != tt.want {
			t.Errorf("formatPackageList(%v) = %q, want %q", tt.packages, got, tt.want)
		}
	}
}
