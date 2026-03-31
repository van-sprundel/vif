package composer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/van-sprundel/vif/internal/composer"
)

func TestParseComposerJSON(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{
		"name": "acme/project",
		"type": "project",
		"require": {
			"php": ">=8.1",
			"monolog/monolog": "^3.0",
			"guzzlehttp/guzzle": "^7.0"
		},
		"require-dev": {
			"phpunit/phpunit": "^10.0"
		},
		"minimum-stability": "beta",
		"prefer-stable": true,
		"config": {
			"sort-packages": true
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cj.Name != "acme/project" {
		t.Errorf("Name = %q, want %q", cj.Name, "acme/project")
	}
	if cj.Type != "project" {
		t.Errorf("Type = %q, want %q", cj.Type, "project")
	}

	// Require.
	if len(cj.Require) != 3 {
		t.Errorf("len(Require) = %d, want 3", len(cj.Require))
	}
	if cj.Require["monolog/monolog"] != "^3.0" {
		t.Errorf("Require[monolog/monolog] = %q", cj.Require["monolog/monolog"])
	}

	// RequireDev.
	if len(cj.RequireDev) != 1 {
		t.Errorf("len(RequireDev) = %d, want 1", len(cj.RequireDev))
	}

	// Stability.
	if cj.MinimumStability != "beta" {
		t.Errorf("MinimumStability = %q, want %q", cj.MinimumStability, "beta")
	}
	if !cj.PreferStable {
		t.Error("PreferStable should be true")
	}
}

func TestParseDefaults(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{
		"name": "test/minimal",
		"require": {
			"acme/foo": "^1.0"
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Defaults.
	if cj.MinimumStability != "stable" {
		t.Errorf("default MinimumStability = %q, want %q", cj.MinimumStability, "stable")
	}
	if cj.PreferStable {
		t.Error("default PreferStable should be false")
	}
}

func TestParseMissingFile(t *testing.T) {
	_, err := composer.Parse("/nonexistent/composer.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{invalid json!!!`)

	_, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestNonPlatformRequire(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{
		"name": "test/filtered",
		"require": {
			"php": ">=8.0",
			"ext-json": "*",
			"ext-mbstring": "*",
			"acme/bar": "^1.0",
			"psr/log": "^3.0"
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	filtered := cj.NonPlatformRequire()
	if len(filtered) != 2 {
		t.Fatalf("got %d non-platform requires, want 2: %v", len(filtered), filtered)
	}
	if filtered["acme/bar"] != "^1.0" {
		t.Error("missing acme/bar")
	}
	if filtered["psr/log"] != "^3.0" {
		t.Error("missing psr/log")
	}
}

func TestNonPlatformRequireDev(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{
		"name": "test/dev",
		"require-dev": {
			"php": ">=8.0",
			"phpunit/phpunit": "^10.0"
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	filtered := cj.NonPlatformRequireDev()
	if len(filtered) != 1 {
		t.Fatalf("got %d, want 1: %v", len(filtered), filtered)
	}
}

func TestContentHash(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{
		"name": "test/hash",
		"require": {"acme/foo": "^1.0"},
		"require-dev": {"phpunit/phpunit": "^10.0"}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	hash := cj.ContentHash()
	if len(hash) != 32 {
		t.Errorf("ContentHash length = %d, want 32 (md5 hex)", len(hash))
	}

	// Deterministic.
	hash2 := cj.ContentHash()
	if hash != hash2 {
		t.Errorf("ContentHash not deterministic: %q != %q", hash, hash2)
	}
}

func writeJSON(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "composer.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
}
