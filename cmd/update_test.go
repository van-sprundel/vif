package cmd

import (
	"context"
	"testing"
	"time"

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

func TestNewUpdateCmdProfileFlag(t *testing.T) {
	cmd := newUpdateCmd()
	flag := cmd.Flags().Lookup("profile")
	if flag == nil {
		t.Fatal("expected --profile flag to exist")
	}
	if got, want := flag.Usage, "print per-phase timings and slowest packages"; got != want {
		t.Fatalf("profile flag usage = %q, want %q", got, want)
	}
}

func TestProfileDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "milliseconds", in: 500 * time.Millisecond, want: "500ms"},
		{name: "seconds", in: 1500 * time.Millisecond, want: "1.50s"},
	}

	for _, tt := range tests {
		got := profileDuration(tt.in)
		if got != tt.want {
			t.Fatalf("%s: profileDuration(%s) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}
