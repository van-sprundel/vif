package lockfile_test

//! Testcases against drupal's composer.lock
//! See: https://git.drupalcode.org/project/drupal/-/blob/main/composer.lock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/testhelper"
)

// fixtureFile returns the absolute path to testdata/fixtures/composer.lock
// relative to this file's location in the source tree.
func fixtureFile(t *testing.T) string {
	t.Helper()
	// Walk up from the package directory to the repo root.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = .../internal/lockfile/lockfile_test.go
	// repo root is two dirs up
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	return filepath.Join(repoRoot, "testdata", "fixtures", "composer.lock")
}

func TestParse(t *testing.T) {
	fixture := fixtureFile(t)

	tests := []struct {
		name        string
		path        string
		wantErr     bool
		errContains string
		// if no error expected, these are checked
		wantContentHash  string
		wantPackageCount int
		wantDevCount     int
	}{
		{
			name:             "valid fixture",
			path:             fixture,
			wantContentHash:  "b9cf6fb8dfd7eac46b064de205806e65",
			wantPackageCount: 62,
			wantDevCount:     83,
		},
		{
			name:        "missing file",
			path:        "/nonexistent/path/composer.lock",
			wantErr:     true,
			errContains: "lockfile:",
		},
		{
			name:        "invalid JSON",
			path:        writeTemp(t, "invalid json {{{"),
			wantErr:     true,
			errContains: "lockfile:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lf, err := lockfile.Parse(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.errContains != "" {
					if got := err.Error(); len(got) == 0 {
						t.Errorf("error message is empty")
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if lf.ContentHash != tc.wantContentHash {
				t.Errorf("ContentHash = %q, want %q", lf.ContentHash, tc.wantContentHash)
			}
			if got := len(lf.Packages); got != tc.wantPackageCount {
				t.Errorf("len(Packages) = %d, want %d", got, tc.wantPackageCount)
			}
			if got := len(lf.PackagesDev); got != tc.wantDevCount {
				t.Errorf("len(PackagesDev) = %d, want %d", got, tc.wantDevCount)
			}
		})
	}
}

func TestParsePackageFields(t *testing.T) {
	lf, err := lockfile.Parse(fixtureFile(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		name        string
		pkgName     string
		wantName    string
		wantVersion string
		wantDistURL string
	}{
		{
			name:        "asm89/stack-cors",
			pkgName:     "asm89/stack-cors",
			wantName:    "asm89/stack-cors",
			wantVersion: "v2.4.0",
			wantDistURL: "https://api.github.com/repos/asm89/stack-cors/zipball/33dcc9955bd5c683e1246f0162f48df73fe799f6",
		},
		{
			name:        "composer/installers",
			pkgName:     "composer/installers",
			wantName:    "composer/installers",
			wantVersion: "v2.3.0",
			wantDistURL: "https://api.github.com/repos/composer/installers/zipball/12fb2dfe5e16183de69e784a7b84046c43d97e8e",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := findPackage(t, lf.Packages, tc.pkgName)
			if p.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", p.Name, tc.wantName)
			}
			if p.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", p.Version, tc.wantVersion)
			}
			if p.Dist.URL != tc.wantDistURL {
				t.Errorf("Dist.URL = %q, want %q", p.Dist.URL, tc.wantDistURL)
			}
		})
	}
}

func TestAutoloadPSR4Normalization(t *testing.T) {
	lf, err := lockfile.Parse(fixtureFile(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		name      string
		pkgName   string
		dev       bool
		namespace string
		wantPaths []string
	}{
		{
			name:      "string value normalized to slice",
			pkgName:   "asm89/stack-cors",
			namespace: `Asm89\Stack\`,
			wantPaths: []string{"src/"},
		},
		{
			name:      "another package string value normalized to slice",
			pkgName:   "drupal/core",
			namespace: `Drupal\Core\`,
			wantPaths: []string{"lib/Drupal/Core"},
		},
		{
			name:      "dev package string value normalized to slice",
			pkgName:   "behat/mink-browserkit-driver",
			dev:       true,
			namespace: `Behat\Mink\Driver\`,
			wantPaths: []string{"src/"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var psr4 map[string][]string
			if tc.dev {
				psr4 = findPackage(t, lf.PackagesDev, tc.pkgName).Autoload.PSR4
			} else {
				psr4 = findPackage(t, lf.Packages, tc.pkgName).Autoload.PSR4
			}

			paths, ok := psr4[tc.namespace]
			if !ok {
				t.Fatalf("namespace %q not found in PSR4 map; map = %v", tc.namespace, psr4)
			}
			if len(paths) != len(tc.wantPaths) {
				t.Fatalf("paths = %v, want %v", paths, tc.wantPaths)
			}
			for i, want := range tc.wantPaths {
				if paths[i] != want {
					t.Errorf("paths[%d] = %q, want %q", i, paths[i], want)
				}
			}
		})
	}
}

func TestAutoloadPSR0Normalization(t *testing.T) {
	lf, err := lockfile.Parse(fixtureFile(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		name      string
		pkgName   string
		namespace string
		wantPaths []string
	}{
		{
			name:      "pear/archive_tar string value normalized",
			pkgName:   "pear/archive_tar",
			namespace: "Archive_Tar",
			wantPaths: []string{""},
		},
		{
			name:      "pear/console_getopt string value normalized",
			pkgName:   "pear/console_getopt",
			namespace: "Console",
			wantPaths: []string{"./"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pkg := findPackage(t, lf.Packages, tc.pkgName)
			paths, ok := pkg.Autoload.PSR0[tc.namespace]
			if !ok {
				t.Fatalf("PSR-0 key %q not found in %q", tc.namespace, tc.pkgName)
			}
			if len(paths) != len(tc.wantPaths) {
				t.Fatalf("PSR-0 paths = %v, want %v", paths, tc.wantPaths)
			}
			for i, want := range tc.wantPaths {
				if paths[i] != want {
					t.Errorf("paths[%d] = %q, want %q", i, paths[i], want)
				}
			}
		})
	}
}

func TestAutoloadClassmapAndFiles(t *testing.T) {
	lf, err := lockfile.Parse(fixtureFile(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	symfonyString := findPackage(t, lf.Packages, "symfony/string")
	if len(symfonyString.Autoload.Files) != 1 || symfonyString.Autoload.Files[0] != "Resources/functions.php" {
		t.Errorf("symfony/string files = %v, want [Resources/functions.php]", symfonyString.Autoload.Files)
	}

	drupalCore := findPackage(t, lf.Packages, "drupal/core")
	if len(drupalCore.Autoload.Classmap) == 0 {
		t.Fatal("drupal/core classmap is empty")
	}
	if !contains(drupalCore.Autoload.Classmap, "lib/Drupal.php") {
		t.Errorf("drupal/core classmap does not contain lib/Drupal.php; classmap = %v", drupalCore.Autoload.Classmap)
	}
}

func findPackage(t *testing.T, packages []pkg.Package, name string) pkg.Package {
	t.Helper()
	for _, p := range packages {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("package %q not found", name)
	return pkg.Package{}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// writeTemp writes content to a temp file and returns its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := testhelper.TempDir(t, "lockfile")
	path := filepath.Join(dir, "composer.lock")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeTemp: %v", err)
	}
	return path
}

func TestGenerate(t *testing.T) {
	dir := testhelper.TempDir(t, "lockfile")
	path := filepath.Join(dir, "composer.lock")

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^1.0"},
		RequireDev:       map[string]string{"acme/test": "^2.0"},
		MinimumStability: "stable",
		PreferStable:     true,
	}

	resolved := []resolver.ResolvedPackage{
		{
			Name:    "acme/foo",
			Version: "1.5.0",
			Entry: packagist.VersionEntry{
				Name:    "acme/foo",
				Version: "1.5.0",
				Type:    "library",
				Require: map[string]string{"acme/bar": "^1.0"},
				Dist: packagist.DistEntry{
					URL:       "https://example.com/acme/foo/1.5.0.zip",
					Type:      "zip",
					Reference: "abc123",
				},
			},
			Dev: false,
		},
		{
			Name:    "acme/bar",
			Version: "1.2.0",
			Entry: packagist.VersionEntry{
				Name:    "acme/bar",
				Version: "1.2.0",
				Type:    "library",
				Dist: packagist.DistEntry{
					URL:       "https://example.com/acme/bar/1.2.0.zip",
					Type:      "zip",
					Reference: "def456",
				},
			},
			Dev: false,
		},
		{
			Name:    "acme/test",
			Version: "2.0.0",
			Entry: packagist.VersionEntry{
				Name:    "acme/test",
				Version: "2.0.0",
				Type:    "library",
				Dist: packagist.DistEntry{
					URL:       "https://example.com/acme/test/2.0.0.zip",
					Type:      "zip",
					Reference: "ghi789",
				},
			},
			Dev: true,
		},
	}

	if err := lockfile.Generate(path, resolved, cj); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Read back and verify.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var lf map[string]json.RawMessage
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Check content-hash.
	var hash string
	json.Unmarshal(lf["content-hash"], &hash)
	if len(hash) != 32 {
		t.Errorf("content-hash length = %d, want 32", len(hash))
	}

	// Check packages count.
	var packages []json.RawMessage
	json.Unmarshal(lf["packages"], &packages)
	if len(packages) != 2 {
		t.Errorf("packages count = %d, want 2", len(packages))
	}

	// Check packages-dev count.
	var packagesDev []json.RawMessage
	json.Unmarshal(lf["packages-dev"], &packagesDev)
	if len(packagesDev) != 1 {
		t.Errorf("packages-dev count = %d, want 1", len(packagesDev))
	}

	// Check minimum-stability.
	var minStab string
	json.Unmarshal(lf["minimum-stability"], &minStab)
	if minStab != "stable" {
		t.Errorf("minimum-stability = %q, want stable", minStab)
	}

	// Check prefer-stable.
	var preferStable bool
	json.Unmarshal(lf["prefer-stable"], &preferStable)
	if !preferStable {
		t.Error("prefer-stable should be true")
	}

	// Check _readme exists.
	if _, ok := lf["_readme"]; !ok {
		t.Error("missing _readme field")
	}

	// Packages should be sorted alphabetically.
	var pkgEntries []struct{ Name string }
	json.Unmarshal(lf["packages"], &pkgEntries)
	if len(pkgEntries) == 2 && pkgEntries[0].Name != "acme/bar" {
		t.Errorf("packages not sorted: first = %q, want acme/bar", pkgEntries[0].Name)
	}

	var platform map[string]string
	json.Unmarshal(lf["platform"], &platform)
	if platform["php"] != "" {
		t.Errorf("platform[php] = %q, want empty for non-platform-only input", platform["php"])
	}
}

func TestGeneratePreservesExistingRawPackageAndPluginAPIVersion(t *testing.T) {
	dir := testhelper.TempDir(t, "lockfile")
	path := filepath.Join(dir, "composer.lock")

	existing := `{
    "_readme": ["test"],
    "content-hash": "oldhash",
    "packages": [
        {
            "name": "acme/foo",
            "version": "1.5.0",
            "source": {
                "type": "git",
                "url": "https://example.com/acme/foo.git",
                "reference": "abc123"
            },
            "dist": {
                "type": "zip",
                "url": "https://example.com/acme/foo/1.5.0.zip",
                "reference": "abc123",
                "shasum": ""
            },
            "notification-url": "https://example.com/downloads/"
        }
    ],
    "packages-dev": [],
    "aliases": [],
    "minimum-stability": "dev",
    "stability-flags": {},
    "prefer-stable": true,
    "prefer-lowest": false,
    "platform": {
        "php": "^8.4"
    },
    "platform-dev": {
        "ext-xdebug": "*"
    },
    "plugin-api-version": "2.9.0"
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"php": "^8.4", "acme/foo": "^1.0"},
		RequireDev:       map[string]string{"ext-xdebug": "*"},
		MinimumStability: "dev",
		PreferStable:     true,
	}

	resolved := []resolver.ResolvedPackage{
		{
			Name:    "acme/foo",
			Version: "1.5.0",
			Entry: packagist.VersionEntry{
				Name:    "acme/foo",
				Version: "1.5.0",
				Type:    "library",
				Dist: packagist.DistEntry{
					URL:       "https://example.com/acme/foo/1.5.0.zip",
					Type:      "zip",
					Reference: "abc123",
				},
			},
		},
	}

	if err := lockfile.Generate(path, resolved, cj); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var out struct {
		Packages         []map[string]json.RawMessage `json:"packages"`
		Platform         map[string]string            `json:"platform"`
		PlatformDev      map[string]string            `json:"platform-dev"`
		PluginAPIVersion string                       `json:"plugin-api-version"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(out.Packages) != 1 {
		t.Fatalf("len(packages) = %d, want 1", len(out.Packages))
	}
	if _, ok := out.Packages[0]["source"]; !ok {
		t.Fatalf("expected preserved source field in package entry")
	}
	if _, ok := out.Packages[0]["notification-url"]; !ok {
		t.Fatalf("expected preserved notification-url field in package entry")
	}
	if out.Platform["php"] != "^8.4" {
		t.Fatalf("platform[php] = %q, want ^8.4", out.Platform["php"])
	}
	if out.PlatformDev["ext-xdebug"] != "*" {
		t.Fatalf("platform-dev[ext-xdebug] = %q, want *", out.PlatformDev["ext-xdebug"])
	}
	if out.PluginAPIVersion != "2.9.0" {
		t.Fatalf("plugin-api-version = %q, want 2.9.0", out.PluginAPIVersion)
	}
}

func TestGenerateEmpty(t *testing.T) {
	dir := testhelper.TempDir(t, "lockfile")
	path := filepath.Join(dir, "composer.lock")

	cj := &composer.ComposerJSON{
		Name:             "test/empty",
		MinimumStability: "stable",
	}

	if err := lockfile.Generate(path, nil, cj); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var lf map[string]json.RawMessage
	json.Unmarshal(data, &lf)

	if string(lf["packages"]) != "[]" {
		t.Errorf("packages = %s, want []", lf["packages"])
	}
	if string(lf["packages-dev"]) != "[]" {
		t.Errorf("packages-dev = %s, want []", lf["packages-dev"])
	}
}

func TestGenerateNoHTMLEscape(t *testing.T) {
	dir := testhelper.TempDir(t, "lockfile")
	path := filepath.Join(dir, "composer.lock")

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"php": ">=8.1"},
		MinimumStability: "stable",
		PreferStable:     true,
	}

	resolved := []resolver.ResolvedPackage{
		{
			Name:    "acme/foo",
			Version: "1.0.0",
			Entry: packagist.VersionEntry{
				Name:     "acme/foo",
				Version:  "1.0.0",
				Type:     "library",
				Require:  map[string]string{"php": ">=8.1", "ext-json": "*"},
				Conflict: map[string]string{"acme/bar": "<2.0", "acme/baz": ">=3.0 <4.0"},
				Dist: packagist.DistEntry{
					URL:       "https://example.com/acme/foo/1.0.0.zip",
					Type:      "zip",
					Reference: "abc123",
				},
			},
		},
	}

	if err := lockfile.Generate(path, resolved, cj); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	raw := string(data)
	if strings.Contains(raw, `\u003c`) {
		t.Error("lockfile contains \\u003c (escaped <), should be raw <")
	}
	if strings.Contains(raw, `\u003e`) {
		t.Error("lockfile contains \\u003e (escaped >), should be raw >")
	}
	if !strings.Contains(raw, `>=8.1`) {
		t.Error("lockfile missing >=8.1 constraint")
	}
	if !strings.Contains(raw, `<2.0`) {
		t.Error("lockfile missing <2.0 constraint")
	}
	if !strings.Contains(raw, `>=3.0 <4.0`) {
		t.Error("lockfile missing >=3.0 <4.0 constraint")
	}
}

func BenchmarkParse(b *testing.B) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	path := filepath.Join(repoRoot, "testdata", "fixtures", "composer.lock")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := lockfile.Parse(path)
		if err != nil {
			b.Fatal(err)
		}
	}
}
