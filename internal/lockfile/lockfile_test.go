package lockfile_test

//! Testcases against drupal's composer.lock
//! See: https://git.drupalcode.org/project/drupal/-/blob/main/composer.lock

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/pkg"
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
	dir := t.TempDir()
	path := filepath.Join(dir, "composer.lock")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeTemp: %v", err)
	}
	return path
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
