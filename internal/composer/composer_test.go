package composer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/testhelper"
)

func TestParseComposerJSON(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
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
		"replace": {
			"symfony/polyfill-ctype": "*"
		},
		"provide": {
			"acme/contract": "1.0.0"
		},
		"conflict": {
			"acme/bad-lib": "^2.0"
		},
		"minimum-stability": "beta",
		"prefer-stable": true,
		"extra": {
			"symfony": {
				"require": "6.4.*"
			}
		},
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
	if cj.Replace["symfony/polyfill-ctype"] != "*" {
		t.Errorf("Replace[symfony/polyfill-ctype] = %q", cj.Replace["symfony/polyfill-ctype"])
	}
	if cj.Provide["acme/contract"] != "1.0.0" {
		t.Errorf("Provide[acme/contract] = %q", cj.Provide["acme/contract"])
	}
	if cj.Conflict["acme/bad-lib"] != "^2.0" {
		t.Errorf("Conflict[acme/bad-lib] = %q", cj.Conflict["acme/bad-lib"])
	}

	// Stability.
	if cj.MinimumStability != "beta" {
		t.Errorf("MinimumStability = %q, want %q", cj.MinimumStability, "beta")
	}
	if !cj.PreferStable {
		t.Error("PreferStable should be true")
	}
	if cj.Extra.Symfony.Require != "6.4.*" {
		t.Errorf("Extra.Symfony.Require = %q, want %q", cj.Extra.Symfony.Require, "6.4.*")
	}
}

func TestParseDefaults(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
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
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{invalid json!!!`)

	_, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestNonPlatformRequire(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
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
	dir := testhelper.TempDir(t, "composer")
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
	dir := testhelper.TempDir(t, "composer")
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

func TestContentHashMatchesComposerEncoding(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{
		"name": "urbanheroes/lely-portal",
		"minimum-stability": "dev",
		"prefer-stable": true,
		"repositories": [
			{
				"type": "composer",
				"url": "https://satis.urban-heroes.nl"
			}
		],
		"require": {
			"php": "^8.4",
			"ext-ctype": "*",
			"twig/twig": "^3.0"
		},
		"require-dev": {
			"phpunit/phpunit": "^9.5"
		},
		"replace": {
			"symfony/polyfill-ctype": "*"
		},
		"conflict": {
			"symfony/symfony": "*"
		},
		"extra": {
			"symfony": {
				"require": "6.4.*"
			}
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got, want := cj.ContentHash(), "d6b6131078f23834264581696f61554e"; got != want {
		t.Fatalf("ContentHash() = %q, want %q", got, want)
	}
}

func TestParseAutoload(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{
		"name": "acme/project",
		"autoload": {
			"psr-4": {
				"App\\": "src/",
				"Database\\": ["database/factories/", "database/seeders/"]
			},
			"classmap": ["lib/"],
			"files": ["helpers.php"]
		},
		"autoload-dev": {
			"psr-4": {
				"Tests\\": "tests/"
			}
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// PSR-4.
	if got := cj.Autoload.PSR4["App\\"]; len(got) != 1 || got[0] != "src/" {
		t.Errorf("Autoload PSR4[App\\] = %v, want [src/]", got)
	}
	if got := cj.Autoload.PSR4["Database\\"]; len(got) != 2 {
		t.Errorf("Autoload PSR4[Database\\] = %v, want 2 entries", got)
	}

	// Classmap.
	if len(cj.Autoload.Classmap) != 1 || cj.Autoload.Classmap[0] != "lib/" {
		t.Errorf("Autoload Classmap = %v, want [lib/]", cj.Autoload.Classmap)
	}

	// Files.
	if len(cj.Autoload.Files) != 1 || cj.Autoload.Files[0] != "helpers.php" {
		t.Errorf("Autoload Files = %v, want [helpers.php]", cj.Autoload.Files)
	}

	// Autoload-dev.
	if got := cj.AutoloadDev.PSR4["Tests\\"]; len(got) != 1 || got[0] != "tests/" {
		t.Errorf("AutoloadDev PSR4[Tests\\] = %v, want [tests/]", got)
	}
}

func TestParseRepositoriesArray(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{
		"name": "test/repos",
		"repositories": [
			{"type": "composer", "url": "https://repo.example.com"},
			{"type": "vcs", "url": "https://github.com/acme/foo"}
		]
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cj.Repositories) != 2 {
		t.Fatalf("len(Repositories) = %d, want 2", len(cj.Repositories))
	}
	if cj.Repositories[0].Type != "composer" {
		t.Errorf("Repositories[0].Type = %q, want composer", cj.Repositories[0].Type)
	}
	if cj.Repositories[1].URL != "https://github.com/acme/foo" {
		t.Errorf("Repositories[1].URL = %q", cj.Repositories[1].URL)
	}
}

func TestParseRepositoriesObject(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{
		"name": "test/repos",
		"repositories": {
			"repo1": {"type": "composer", "url": "https://repo.example.com"},
			"repo2": {"type": "vcs", "url": "https://github.com/acme/foo"}
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cj.Repositories) != 2 {
		t.Fatalf("len(Repositories) = %d, want 2", len(cj.Repositories))
	}
	foundComposer, foundVcs := false, false
	for _, r := range cj.Repositories {
		if r.Type == "composer" {
			foundComposer = true
		}
		if r.Type == "vcs" {
			foundVcs = true
		}
	}
	if !foundComposer || !foundVcs {
		t.Errorf("Repositories = %v, want one composer and one vcs", cj.Repositories)
	}
}

func TestParseRepositoriesEmpty(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{"name": "test/no-repos"}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cj.Repositories) != 0 {
		t.Errorf("len(Repositories) = %d, want 0", len(cj.Repositories))
	}
}

func writeJSON(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "composer.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
}
