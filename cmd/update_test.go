package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/resolver"
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

func TestFormatLookupLog(t *testing.T) {
	if got, want := formatLookupLog("acme/foo", 1500*time.Millisecond, nil), "  Lookup acme/foo (1.50s)"; got != want {
		t.Fatalf("formatLookupLog success = %q, want %q", got, want)
	}

	err := errors.New("timeout")
	if got, want := formatLookupLog("acme/foo", 500*time.Millisecond, err), "  Lookup acme/foo (500ms, error: timeout)"; got != want {
		t.Fatalf("formatLookupLog error = %q, want %q", got, want)
	}
}

func TestFormatRepositoryLookupLog(t *testing.T) {
	got := formatRepositoryLookupLog(packagist.LookupTrace{
		Source:   "https://repo.example.test",
		Package:  "acme/foo",
		Duration: 1500 * time.Millisecond,
	})
	want := "  Repo https://repo.example.test acme/foo (1.50s, hit)"
	if got != want {
		t.Fatalf("formatRepositoryLookupLog hit = %q, want %q", got, want)
	}

	got = formatRepositoryLookupLog(packagist.LookupTrace{
		Source:   "https://repo.example.test",
		Package:  "acme/foo",
		Duration: 500 * time.Millisecond,
		Err:      fmt.Errorf("%w: acme/foo", packagist.ErrPackageNotFound),
	})
	want = "  Repo https://repo.example.test acme/foo (500ms, not found)"
	if got != want {
		t.Fatalf("formatRepositoryLookupLog miss = %q, want %q", got, want)
	}
}

func TestApplyBumpAfterUpdate(t *testing.T) {
	cj := parseComposerJSONForUpdateTest(t, `{
		"name": "test/project",
		"require": {"symfony/flex": "^2.8.0"},
		"require-dev": {"phpunit/phpunit": "^13.1.1"},
		"config": {"bump-after-update": true}
	}`)

	resolved := []resolver.ResolvedPackage{
		{Name: "symfony/flex", Version: "v2.8.2"},
		{Name: "phpunit/phpunit", Version: "13.1.3"},
	}

	if changed := applyBumpAfterUpdate(cj, resolved, false); !changed {
		t.Fatalf("applyBumpAfterUpdate() = false, want true")
	}

	if got, want := cj.Require["symfony/flex"], "^2.8.2"; got != want {
		t.Fatalf("require symfony/flex = %q, want %q", got, want)
	}
	if got, want := cj.RequireDev["phpunit/phpunit"], "^13.1.3"; got != want {
		t.Fatalf("require-dev phpunit/phpunit = %q, want %q", got, want)
	}
}

func TestApplyBumpAfterUpdateNoDevMode(t *testing.T) {
	cj := parseComposerJSONForUpdateTest(t, `{
		"name": "test/project",
		"require": {"symfony/flex": "^2.8.0"},
		"require-dev": {"phpunit/phpunit": "^13.1.1"},
		"config": {"bump-after-update": "no-dev"}
	}`)

	resolved := []resolver.ResolvedPackage{
		{Name: "symfony/flex", Version: "v2.8.2"},
		{Name: "phpunit/phpunit", Version: "13.1.3"},
	}

	if changed := applyBumpAfterUpdate(cj, resolved, false); !changed {
		t.Fatalf("applyBumpAfterUpdate() = false, want true")
	}

	if got, want := cj.Require["symfony/flex"], "^2.8.2"; got != want {
		t.Fatalf("require symfony/flex = %q, want %q", got, want)
	}
	if got, want := cj.RequireDev["phpunit/phpunit"], "^13.1.1"; got != want {
		t.Fatalf("require-dev phpunit/phpunit = %q, want %q", got, want)
	}
}

func TestBumpConstraint(t *testing.T) {
	tests := []struct {
		constraint string
		version    string
		want       string
		ok         bool
	}{
		{constraint: "^3.94.2", version: "v3.95.1", want: "^3.95.1", ok: true},
		{constraint: "~2.1.46", version: "2.1.47", want: "~2.1.47", ok: true},
		{constraint: "13.1.1", version: "13.1.3", want: "13.1.3", ok: true},
		{constraint: "^13.1.1@dev", version: "13.1.3", want: "^13.1.3@dev", ok: true},
		{constraint: "^13.1 || ^14.0", version: "13.1.3", ok: false},
		{constraint: "dev-main", version: "13.1.3", ok: false},
	}

	for _, tt := range tests {
		got, ok := bumpConstraint(tt.constraint, tt.version)
		if ok != tt.ok {
			t.Fatalf("bumpConstraint(%q, %q) ok=%v, want %v", tt.constraint, tt.version, ok, tt.ok)
		}
		if ok && got != tt.want {
			t.Fatalf("bumpConstraint(%q, %q) = %q, want %q", tt.constraint, tt.version, got, tt.want)
		}
	}
}

func parseComposerJSONForUpdateTest(t *testing.T, content string) *composer.ComposerJSON {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "composer.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cj, err := composer.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cj
}

func TestResolvedToPackagePreservesMetadata(t *testing.T) {
	rp := resolver.ResolvedPackage{
		Name:    "symfony/flex",
		Version: "v2.8.2",
		Entry: packagist.VersionEntry{
			Name:              "symfony/flex",
			Version:           "v2.8.2",
			VersionNormalized: "2.8.2.0",
			Type:              "composer-plugin",
			Require: map[string]string{
				"php":                 ">=8.0",
				"composer-plugin-api": "^2.1",
			},
			RequireDev: map[string]string{
				"symfony/process": "^6.4|^7.0",
			},
			Provide: map[string]string{
				"acme/virtual": "self.version",
			},
			Replace: map[string]string{
				"acme/replaced": "*",
			},
			Conflict: map[string]string{
				"acme/conflict": "<1.0",
			},
			Time: "2026-01-02T03:04:05+00:00",
			Dist: packagist.DistEntry{
				Type:      "zip",
				URL:       "https://example.com/symfony-flex.zip",
				Reference: "abc123",
				Shasum:    "deadbeef",
			},
			Source: packagist.DistEntry{
				Type:      "git",
				URL:       "https://example.com/symfony-flex.git",
				Reference: "abc123",
				Shasum:    "deadbeef",
			},
			Autoload: mustRawJSON(t, map[string]any{
				"psr-4": map[string]any{
					"Symfony\\Flex\\": "src/",
				},
			}),
			AutoloadDev: mustRawJSON(t, map[string]any{
				"files": []string{"tests/bootstrap.php"},
			}),
		},
	}

	got, err := resolvedToPackage(rp)
	if err != nil {
		t.Fatalf("resolvedToPackage: %v", err)
	}

	if got.VersionNormalized != "2.8.2.0" {
		t.Fatalf("VersionNormalized = %q, want 2.8.2.0", got.VersionNormalized)
	}
	if got.Type != "composer-plugin" {
		t.Fatalf("Type = %q, want composer-plugin", got.Type)
	}
	if got.Require["composer-plugin-api"] != "^2.1" {
		t.Fatalf("Require[composer-plugin-api] = %q, want ^2.1", got.Require["composer-plugin-api"])
	}
	if got.RequireDev["symfony/process"] != "^6.4|^7.0" {
		t.Fatalf("RequireDev[symfony/process] = %q, want ^6.4|^7.0", got.RequireDev["symfony/process"])
	}
	if got.Provide["acme/virtual"] != "self.version" {
		t.Fatalf("Provide[acme/virtual] = %q, want self.version", got.Provide["acme/virtual"])
	}
	if got.Replace["acme/replaced"] != "*" {
		t.Fatalf("Replace[acme/replaced] = %q, want *", got.Replace["acme/replaced"])
	}
	if got.Conflict["acme/conflict"] != "<1.0" {
		t.Fatalf("Conflict[acme/conflict] = %q, want <1.0", got.Conflict["acme/conflict"])
	}
	if got.Time != "2026-01-02T03:04:05+00:00" {
		t.Fatalf("Time = %q, want 2026-01-02T03:04:05+00:00", got.Time)
	}
	if got.Autoload.PSR4["Symfony\\Flex\\"][0] != "src/" {
		t.Fatalf("Autoload PSR-4 = %v, want Symfony\\\\Flex\\\\ => src/", got.Autoload.PSR4)
	}
	if got.AutoloadDev.Files[0] != "tests/bootstrap.php" {
		t.Fatalf("AutoloadDev.Files = %v, want [tests/bootstrap.php]", got.AutoloadDev.Files)
	}
	if got.Dist != (pkg.Dist{
		Type:      "zip",
		URL:       "https://example.com/symfony-flex.zip",
		Reference: "abc123",
		Shasum:    "deadbeef",
	}) {
		t.Fatalf("Dist = %+v", got.Dist)
	}
}

func mustRawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}
