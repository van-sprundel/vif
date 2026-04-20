package composer_test

import (
	"os"
	"path/filepath"
	"strings"
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
			"sort-packages": true,
			"platform": {
				"php": "8.4.1",
				"ext-imagick": false
			}
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
	if cj.PlatformOverrides()["php"] != "8.4.1" {
		t.Errorf("PlatformOverrides()[php] = %q, want %q", cj.PlatformOverrides()["php"], "8.4.1")
	}
	if !cj.DisabledPlatformPackages()["ext-imagick"] {
		t.Error("DisabledPlatformPackages should include ext-imagick")
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

func TestParseScriptsArray(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{
		"name": "test/scripts",
		"scripts": {
			"post-install-cmd": ["echo install", "@php bin/console cache:clear"],
			"post-update-cmd": "echo update",
			"custom": ["@post-install-cmd"]
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cj.Scripts["post-install-cmd"]) != 2 {
		t.Errorf("post-install-cmd handlers = %d, want 2", len(cj.Scripts["post-install-cmd"]))
	}
	if cj.Scripts["post-install-cmd"][0] != "echo install" {
		t.Errorf("post-install-cmd[0] = %q", cj.Scripts["post-install-cmd"][0])
	}

	// Single string should be wrapped in array.
	if len(cj.Scripts["post-update-cmd"]) != 1 {
		t.Errorf("post-update-cmd handlers = %d, want 1", len(cj.Scripts["post-update-cmd"]))
	}
	if cj.Scripts["post-update-cmd"][0] != "echo update" {
		t.Errorf("post-update-cmd[0] = %q", cj.Scripts["post-update-cmd"][0])
	}
}

func TestParseScriptsEmpty(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{"name": "test/no-scripts"}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cj.Scripts) != 0 {
		t.Errorf("len(Scripts) = %d, want 0", len(cj.Scripts))
	}
}

func TestParseScriptsObjectMap(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{
		"name": "test/scripts-object",
		"scripts": {
			"post-install-cmd": ["@auto-scripts"],
			"post-update-cmd": ["@auto-scripts"],
			"auto-scripts": {
				"cache:clear": "symfony-cmd",
				"assets:install %PUBLIC_DIR%": "symfony-cmd"
			}
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Object-form scripts are silently skipped.
	if _, ok := cj.Scripts["auto-scripts"]; ok {
		t.Error("auto-scripts should not be stored as a script handler")
	}

	// Regular string/array scripts still parse correctly.
	if len(cj.Scripts["post-install-cmd"]) != 1 {
		t.Errorf("post-install-cmd handlers = %d, want 1", len(cj.Scripts["post-install-cmd"]))
	}
}

func TestParseAutoScripts(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	writeJSON(t, dir, `{
		"name": "test/auto-scripts",
		"extra": {
			"symfony": {
				"require": "6.4.*",
				"auto-scripts": {
					"cache:clear": "symfony-cmd",
					"assets:install %PUBLIC_DIR%": "symfony-cmd"
				}
			}
		}
	}`)

	cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cj.Extra.Symfony.AutoScripts) != 2 {
		t.Fatalf("AutoScripts = %d, want 2", len(cj.Extra.Symfony.AutoScripts))
	}
	if cj.Extra.Symfony.AutoScripts["cache:clear"] != "symfony-cmd" {
		t.Errorf("AutoScripts[cache:clear] = %q", cj.Extra.Symfony.AutoScripts["cache:clear"])
	}
}

func TestBumpAfterUpdateMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
		want string
	}{
		{name: "bool true", cfg: `true`, want: "all"},
		{name: "bool false", cfg: `false`, want: ""},
		{name: "dev", cfg: `"dev"`, want: "dev"},
		{name: "no-dev", cfg: `"no-dev"`, want: "no-dev"},
		{name: "unknown string", cfg: `"weird"`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := testhelper.TempDir(t, "composer")
			writeJSON(t, dir, `{
				"name": "test/bump",
				"config": {
					"bump-after-update": `+tt.cfg+`
				}
			}`)

			cj, err := composer.Parse(filepath.Join(dir, "composer.json"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if got := cj.BumpAfterUpdateMode(); got != tt.want {
				t.Fatalf("BumpAfterUpdateMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWritePreservesTopLevelAndRequireOrder(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	path := filepath.Join(dir, "composer.json")

	original := `{
    "license": "proprietary",
    "minimum-stability": "stable",
    "prefer-stable": true,
    "repositories": [
        {
            "type": "composer",
            "url": "https://satis.example.test"
        }
    ],
    "require": {
        "php": ">=8.4",
        "ext-ctype": "*",
        "twig/twig": "^3.24.0"
    },
    "config": {
        "platform": {
            "php": "8.4.1"
        },
        "allow-plugins": {
            "symfony/flex": true
        },
        "bump-after-update": true,
        "sort-packages": true,
        "audit": {
            "block-insecure": false
        }
    },
    "require-dev": {
        "phpunit/phpunit": "^13.1.1",
        "phpstan/phpstan": "^2.1.46"
    }
}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cj, err := composer.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cj.AddRequire("twig/twig", "^3.25.0")
	cj.AddRequireDev("phpunit/phpunit", "^13.1.7")

	if err := cj.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)

	topLevel := []string{
		`"license"`,
		`"minimum-stability"`,
		`"prefer-stable"`,
		`"repositories"`,
		`"require"`,
		`"config"`,
		`"require-dev"`,
	}
	last := -1
	for _, key := range topLevel {
		idx := strings.Index(got, key)
		if idx == -1 {
			t.Fatalf("missing top-level key %s in output:\n%s", key, got)
		}
		if idx <= last {
			t.Fatalf("top-level key %s moved out of order in output:\n%s", key, got)
		}
		last = idx
	}

	requireSectionStart := strings.Index(got, `"require": {`)
	requireSectionEnd := strings.Index(got[requireSectionStart:], "\n    },")
	requireSection := got[requireSectionStart : requireSectionStart+requireSectionEnd]
	requireKeys := []string{
		`"php"`,
		`"ext-ctype"`,
		`"twig/twig"`,
	}
	last = -1
	for _, key := range requireKeys {
		idx := strings.Index(requireSection, key)
		if idx == -1 {
			t.Fatalf("missing require key %s in output:\n%s", key, requireSection)
		}
		if idx <= last {
			t.Fatalf("require key %s moved out of order in output:\n%s", key, requireSection)
		}
		last = idx
	}

	if !strings.Contains(got, `"twig/twig": "^3.25.0"`) {
		t.Fatalf("updated require constraint missing from output:\n%s", got)
	}
	if !strings.Contains(got, `"phpunit/phpunit": "^13.1.7"`) {
		t.Fatalf("updated require-dev constraint missing from output:\n%s", got)
	}

	configOrder := []string{
		`"platform"`,
		`"allow-plugins"`,
		`"bump-after-update"`,
		`"sort-packages"`,
		`"audit"`,
	}
	configSectionStart := strings.Index(got, `"config": {`)
	configSectionEnd := strings.Index(got[configSectionStart:], "\n    },")
	configSection := got[configSectionStart : configSectionStart+configSectionEnd]
	last = -1
	for _, key := range configOrder {
		idx := strings.Index(configSection, key)
		if idx == -1 {
			t.Fatalf("missing config key %s in output:\n%s", key, configSection)
		}
		if idx <= last {
			t.Fatalf("config key %s moved out of order in output:\n%s", key, configSection)
		}
		last = idx
	}
}

func TestWriteAppendsNewRequireKeyWithoutResortingExistingEntries(t *testing.T) {
	dir := testhelper.TempDir(t, "composer")
	path := filepath.Join(dir, "composer.json")
	if err := os.WriteFile(path, []byte(`{
    "require": {
        "php": ">=8.4",
        "twig/twig": "^3.24.0"
    }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cj, err := composer.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cj.AddRequire("symfony/console", "^8.0")

	if err := cj.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	requireKeys := []string{
		`"php"`,
		`"twig/twig"`,
		`"symfony/console"`,
	}
	last := -1
	for _, key := range requireKeys {
		idx := strings.Index(got, key)
		if idx == -1 {
			t.Fatalf("missing require key %s in output:\n%s", key, got)
		}
		if idx <= last {
			t.Fatalf("require key %s moved out of order in output:\n%s", key, got)
		}
		last = idx
	}
}

func writeJSON(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "composer.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
}
