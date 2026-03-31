package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/van-sprundel/vif/internal/autoload"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/pkg"
)

// testProject defines a small but realistic set of packages for E2E testing.
type testProject struct {
	packages    []testPkg
	devPackages []testPkg
}

type testPkg struct {
	name      string
	version   string
	namespace string // PSR-4 namespace
	srcDir    string // PSR-4 source dir
	files     map[string]string
	autoFiles []string // files autoload entries
}

func newTestProject() testProject {
	return testProject{
		packages: []testPkg{
			{
				name:      "acme/http",
				version:   "2.1.0",
				namespace: `Acme\Http\`,
				srcDir:    "src/",
				files: map[string]string{
					"src/Client.php":   "<?php\nnamespace Acme\\Http;\nclass Client {}",
					"src/Request.php":  "<?php\nnamespace Acme\\Http;\nclass Request {}",
					"src/Response.php": "<?php\nnamespace Acme\\Http;\nclass Response {}",
					"composer.json":    `{"name":"acme/http"}`,
				},
			},
			{
				name:      "acme/logger",
				version:   "1.0.0",
				namespace: `Acme\Logger\`,
				srcDir:    "src/",
				files: map[string]string{
					"src/Logger.php":          "<?php\nnamespace Acme\\Logger;\nclass Logger {}",
					"src/NullLogger.php":      "<?php\nnamespace Acme\\Logger;\nclass NullLogger {}",
					"src/helpers.php":         "<?php\nfunction acme_log() {}",
					"composer.json":           `{"name":"acme/logger"}`,
				},
				autoFiles: []string{"src/helpers.php"},
			},
			{
				name:      "acme/config",
				version:   "3.0.5",
				namespace: `Acme\Config\`,
				srcDir:    "src/",
				files: map[string]string{
					"src/Config.php":    "<?php\nnamespace Acme\\Config;\nclass Config {}",
					"src/Loader.php":    "<?php\nnamespace Acme\\Config;\nclass Loader {}",
					"composer.json":     `{"name":"acme/config"}`,
				},
			},
		},
		devPackages: []testPkg{
			{
				name:      "acme/test-utils",
				version:   "1.2.0",
				namespace: `Acme\TestUtils\`,
				srcDir:    "src/",
				files: map[string]string{
					"src/TestCase.php": "<?php\nnamespace Acme\\TestUtils;\nclass TestCase {}",
					"composer.json":    `{"name":"acme/test-utils"}`,
				},
			},
			{
				name:      "acme/mock",
				version:   "0.5.0",
				namespace: `Acme\Mock\`,
				srcDir:    "src/",
				files: map[string]string{
					"src/Mock.php":   "<?php\nnamespace Acme\\Mock;\nclass Mock {}",
					"composer.json":  `{"name":"acme/mock"}`,
				},
			},
		},
	}
}

// serve is defined below with testing.TB signature for use in both tests and benchmarks.

// writeLockfile writes a composer.lock file using the test server's URL.
func (tp testProject) writeLockfile(t *testing.T, dir, serverURL string) {
	t.Helper()

	toPkg := func(tp testPkg) pkg.Package {
		p := pkg.Package{
			Name:    tp.name,
			Version: tp.version,
			Type:    "library",
			Dist: pkg.Dist{
				Type: "zip",
				URL:  serverURL + "/" + tp.name + ".zip",
			},
			Autoload: pkg.Autoload{
				PSR4:  map[string][]string{tp.namespace: {tp.srcDir}},
				Files: tp.autoFiles,
			},
		}
		return p
	}

	lf := lockfile.LockFile{
		ContentHash: "e2etesthash",
	}
	for _, p := range tp.packages {
		lf.Packages = append(lf.Packages, toPkg(p))
	}
	for _, p := range tp.devPackages {
		lf.PackagesDev = append(lf.PackagesDev, toPkg(p))
	}

	data, err := json.MarshalIndent(lf, "", "    ")
	if err != nil {
		t.Fatalf("marshal lockfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "composer.lock"), data, 0o644); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
}

func TestE2EInstall(t *testing.T) {
	project := newTestProject()
	srv := project.serve(t)
	defer srv.Close()

	projectDir := t.TempDir()
	project.writeLockfile(t, projectDir, srv.URL)

	// Parse lockfile.
	lf, err := lockfile.Parse(filepath.Join(projectDir, "composer.lock"))
	if err != nil {
		t.Fatalf("parse lockfile: %v", err)
	}

	allPackages := append(lf.Packages, lf.PackagesDev...)

	// Init cache.
	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	// Download.
	dl := downloader.New(c, 4)
	results, err := dl.Download(context.Background(), allPackages)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("download %s: %v", r.Package.Name, r.Err)
		}
		if r.FromCache {
			t.Errorf("first install should not hit cache for %s", r.Package.Name)
		}
	}

	// Install.
	vendorDir := filepath.Join(projectDir, "vendor")
	inst := installer.New(c)
	if err := inst.Install(lf.Packages, lf.PackagesDev, vendorDir); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Generate autoloader.
	if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash); err != nil {
		t.Fatalf("autoload: %v", err)
	}

	// --- Verify vendor/ layout ---
	t.Run("vendor layout", func(t *testing.T) {
		for _, p := range append(project.packages, project.devPackages...) {
			for relPath, wantContent := range p.files {
				got, err := os.ReadFile(filepath.Join(vendorDir, p.name, relPath))
				if err != nil {
					t.Errorf("%s/%s missing: %v", p.name, relPath, err)
					continue
				}
				if string(got) != wantContent {
					t.Errorf("%s/%s content mismatch", p.name, relPath)
				}
			}
		}
	})

	// --- Verify autoloader files ---
	t.Run("autoloader files exist", func(t *testing.T) {
		requiredFiles := []string{
			"autoload.php",
			"composer/autoload_real.php",
			"composer/autoload_static.php",
			"composer/autoload_psr4.php",
			"composer/autoload_namespaces.php",
			"composer/autoload_classmap.php",
			"composer/autoload_files.php",
			"composer/ClassLoader.php",
			"composer/installed.json",
		}
		for _, f := range requiredFiles {
			if _, err := os.Stat(filepath.Join(vendorDir, f)); err != nil {
				t.Errorf("missing %s: %v", f, err)
			}
		}
	})

	// --- Verify autoload.php references correct hash ---
	t.Run("autoload hash", func(t *testing.T) {
		data, _ := os.ReadFile(filepath.Join(vendorDir, "autoload.php"))
		if !strings.Contains(string(data), "ComposerAutoloaderInite2etesthash") {
			t.Error("autoload.php should reference the content hash")
		}
	})

	// --- Verify PSR-4 entries ---
	t.Run("psr4 entries", func(t *testing.T) {
		data, _ := os.ReadFile(filepath.Join(vendorDir, "composer", "autoload_psr4.php"))
		content := string(data)
		for _, p := range append(project.packages, project.devPackages...) {
			escaped := strings.ReplaceAll(p.namespace, `\`, `\\`)
			if !strings.Contains(content, escaped) {
				t.Errorf("autoload_psr4.php missing namespace %s", p.namespace)
			}
		}
	})

	// --- Verify files autoload ---
	t.Run("files autoload", func(t *testing.T) {
		data, _ := os.ReadFile(filepath.Join(vendorDir, "composer", "autoload_files.php"))
		content := string(data)
		if !strings.Contains(content, "acme/logger/src/helpers.php") {
			t.Errorf("autoload_files.php should reference acme/logger helpers, got:\n%s", content)
		}
	})

	// --- Verify installed.json ---
	t.Run("installed.json", func(t *testing.T) {
		data, _ := os.ReadFile(filepath.Join(vendorDir, "composer", "installed.json"))
		var installed struct {
			Packages        []json.RawMessage `json:"packages"`
			Dev             bool              `json:"dev"`
			DevPackageNames []string          `json:"dev-package-names"`
		}
		if err := json.Unmarshal(data, &installed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		wantTotal := len(project.packages) + len(project.devPackages)
		if len(installed.Packages) != wantTotal {
			t.Errorf("installed.json has %d packages, want %d", len(installed.Packages), wantTotal)
		}
		if len(installed.DevPackageNames) != len(project.devPackages) {
			t.Errorf("dev-package-names has %d entries, want %d", len(installed.DevPackageNames), len(project.devPackages))
		}
	})

	// --- Verify warm install hits cache ---
	t.Run("warm install from cache", func(t *testing.T) {
		results2, err := dl.Download(context.Background(), allPackages)
		if err != nil {
			t.Fatalf("warm download: %v", err)
		}
		for _, r := range results2 {
			if r.Err != nil {
				t.Errorf("warm download %s: %v", r.Package.Name, r.Err)
			}
			if !r.FromCache {
				t.Errorf("%s should be from cache on second install", r.Package.Name)
			}
		}
	})
}

// --- Benchmarks ---

func BenchmarkE2EColdInstall(b *testing.B) {
	project := newTestProject()
	srv := project.serve(b)
	defer srv.Close()

	for i := 0; i < b.N; i++ {
		runFullInstall(b, project, srv.URL, true)
	}
}

func BenchmarkE2EWarmInstall(b *testing.B) {
	project := newTestProject()
	srv := project.serve(b)
	defer srv.Close()

	// Prime the cache once.
	cacheDir := runFullInstall(b, project, srv.URL, false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runFullInstallWithCache(b, project, srv.URL, cacheDir)
	}
}

// runFullInstall runs the complete install pipeline and returns the cache dir.
func runFullInstall(tb testing.TB, project testProject, serverURL string, cleanup bool) string {
	tb.Helper()

	projectDir := tb.TempDir()
	project.writeLockfileB(tb, projectDir, serverURL)

	cacheDir := tb.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		tb.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	lf, err := lockfile.Parse(filepath.Join(projectDir, "composer.lock"))
	if err != nil {
		tb.Fatalf("parse: %v", err)
	}

	allPackages := append(lf.Packages, lf.PackagesDev...)

	dl := downloader.New(c, 4)
	if _, err := dl.Download(context.Background(), allPackages); err != nil {
		tb.Fatalf("download: %v", err)
	}

	vendorDir := filepath.Join(projectDir, "vendor")
	inst := installer.New(c)
	if err := inst.Install(lf.Packages, lf.PackagesDev, vendorDir); err != nil {
		tb.Fatalf("install: %v", err)
	}

	if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash); err != nil {
		tb.Fatalf("autoload: %v", err)
	}

	return cacheDir
}

// runFullInstallWithCache runs the install pipeline reusing an existing cache dir.
func runFullInstallWithCache(tb testing.TB, project testProject, serverURL, cacheDir string) {
	tb.Helper()

	projectDir := tb.TempDir()
	project.writeLockfileB(tb, projectDir, serverURL)

	c, err := cache.New(cacheDir)
	if err != nil {
		tb.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	lf, err := lockfile.Parse(filepath.Join(projectDir, "composer.lock"))
	if err != nil {
		tb.Fatalf("parse: %v", err)
	}

	allPackages := append(lf.Packages, lf.PackagesDev...)

	dl := downloader.New(c, 4)
	if _, err := dl.Download(context.Background(), allPackages); err != nil {
		tb.Fatalf("download: %v", err)
	}

	vendorDir := filepath.Join(projectDir, "vendor")
	inst := installer.New(c)
	if err := inst.Install(lf.Packages, lf.PackagesDev, vendorDir); err != nil {
		tb.Fatalf("install: %v", err)
	}

	if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash); err != nil {
		tb.Fatalf("autoload: %v", err)
	}
}

// writeLockfileB is the benchmark-compatible variant.
func (tp testProject) writeLockfileB(tb testing.TB, dir, serverURL string) {
	tb.Helper()

	toPkg := func(tp testPkg) pkg.Package {
		return pkg.Package{
			Name:    tp.name,
			Version: tp.version,
			Type:    "library",
			Dist: pkg.Dist{
				Type: "zip",
				URL:  serverURL + "/" + tp.name + ".zip",
			},
			Autoload: pkg.Autoload{
				PSR4:  map[string][]string{tp.namespace: {tp.srcDir}},
				Files: tp.autoFiles,
			},
		}
	}

	lf := lockfile.LockFile{ContentHash: "e2etesthash"}
	for _, p := range tp.packages {
		lf.Packages = append(lf.Packages, toPkg(p))
	}
	for _, p := range tp.devPackages {
		lf.PackagesDev = append(lf.PackagesDev, toPkg(p))
	}

	data, err := json.MarshalIndent(lf, "", "    ")
	if err != nil {
		tb.Fatalf("marshal lockfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "composer.lock"), data, 0o644); err != nil {
		tb.Fatalf("write lockfile: %v", err)
	}
}

func makeZipE2E(tb testing.TB, files map[string]string) []byte {
	tb.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			tb.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			tb.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// --- Real-world comparison benchmarks against Composer ---
// These use the Drupal fixture lockfile and hit real package registries.
// They are slow and require network access, so they're behind a build tag check.

// benchFixturePath returns the path to a fixture file in testdata/fixtures/.
func benchFixturePath(tb testing.TB, name string) string {
	tb.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "fixtures", name)
}

// BenchmarkComposerInstallWarm benchmarks `composer install` with a warm cache
// against the bench fixture lockfile (22 real packages).
func BenchmarkComposerInstallWarm(b *testing.B) {
	composerBin, err := exec.LookPath("composer")
	if err != nil {
		b.Skip("composer not found in PATH")
	}

	fixture := benchFixturePath(b, "bench.lock")
	composerJSON := benchFixturePath(b, "bench.json")
	if _, err := os.Stat(fixture); err != nil {
		b.Skipf("fixture not found: %v", err)
	}

	// Prime composer cache with one run.
	primedDir := b.TempDir()
	copyFile2(b, fixture, filepath.Join(primedDir, "composer.lock"))
	copyFile2(b, composerJSON, filepath.Join(primedDir, "composer.json"))
	prime := exec.Command(composerBin, "install", "--no-scripts", "--no-plugins", "--no-interaction", "--ignore-platform-reqs")
	prime.Dir = primedDir
	prime.Env = append(os.Environ(), "COMPOSER_NO_AUDIT=1")
	if out, err := prime.CombinedOutput(); err != nil {
		b.Skipf("composer prime failed (platform issue?): %v\n%s", err, out)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		copyFile2(b, fixture, filepath.Join(dir, "composer.lock"))
		copyFile2(b, composerJSON, filepath.Join(dir, "composer.json"))

		cmd := exec.Command(composerBin, "install", "--no-scripts", "--no-plugins", "--no-interaction", "--ignore-platform-reqs")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "COMPOSER_NO_AUDIT=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("composer install failed: %v\n%s", err, out)
		}
	}
}

// BenchmarkVifInstallWarm benchmarks vif install with a warm cache
// against the bench fixture lockfile (22 real packages).
func BenchmarkVifInstallWarm(b *testing.B) {
	fixture := benchFixturePath(b, "bench.lock")
	if _, err := os.Stat(fixture); err != nil {
		b.Skipf("fixture not found: %v", err)
	}

	// Prime vif cache with one run.
	cacheDir := b.TempDir()
	primeVifInstall(b, fixture, cacheDir)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		projectDir := b.TempDir()
		copyFile2(b, fixture, filepath.Join(projectDir, "composer.lock"))

		c, err := cache.New(cacheDir)
		if err != nil {
			b.Fatalf("cache.New: %v", err)
		}

		lf, err := lockfile.Parse(filepath.Join(projectDir, "composer.lock"))
		if err != nil {
			b.Fatalf("parse: %v", err)
		}

		allPackages := append(lf.Packages, lf.PackagesDev...)

		dl := downloader.New(c, 0)
		results, err := dl.Download(context.Background(), allPackages)
		if err != nil {
			b.Fatalf("download: %v", err)
		}
		for _, r := range results {
			if r.Err != nil && !r.Skipped {
				b.Fatalf("download %s: %v", r.Package.Name, r.Err)
			}
		}

		vendorDir := filepath.Join(projectDir, "vendor")
		inst := installer.New(c)
		if err := inst.Install(lf.Packages, lf.PackagesDev, vendorDir); err != nil {
			b.Fatalf("install: %v", err)
		}

		if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash); err != nil {
			b.Fatalf("autoload: %v", err)
		}
		c.Close()
	}
}

func primeVifInstall(tb testing.TB, fixturePath, cacheDir string) {
	tb.Helper()
	projectDir := tb.TempDir()
	copyFile2(tb, fixturePath, filepath.Join(projectDir, "composer.lock"))

	c, err := cache.New(cacheDir)
	if err != nil {
		tb.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	lf, err := lockfile.Parse(filepath.Join(projectDir, "composer.lock"))
	if err != nil {
		tb.Fatalf("parse: %v", err)
	}

	allPackages := append(lf.Packages, lf.PackagesDev...)

	dl := downloader.New(c, 0)
	results, err := dl.Download(context.Background(), allPackages)
	if err != nil {
		tb.Fatalf("download: %v", err)
	}
	for _, r := range results {
		if r.Err != nil && !r.Skipped {
			tb.Fatalf("prime download %s: %v", r.Package.Name, r.Err)
		}
	}
}

func copyFile2(tb testing.TB, src, dst string) {
	tb.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		tb.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		tb.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		tb.Fatalf("write %s: %v", dst, err)
	}
}

// writeMinimalComposerJSON writes a minimal composer.json that satisfies `composer install`.
func writeMinimalComposerJSON(tb testing.TB, dir string) {
	tb.Helper()
	content := []byte(`{"name":"test/project","type":"project","require":{}}`)
	if err := os.WriteFile(filepath.Join(dir, "composer.json"), content, 0o644); err != nil {
		tb.Fatalf("write composer.json: %v", err)
	}
}

// serve for benchmarks.
func (tp testProject) serve(tb testing.TB) *httptest.Server {
	tb.Helper()
	zips := make(map[string][]byte)

	allPkgs := make([]testPkg, 0, len(tp.packages)+len(tp.devPackages))
	allPkgs = append(allPkgs, tp.packages...)
	allPkgs = append(allPkgs, tp.devPackages...)

	for _, p := range allPkgs {
		zips["/"+p.name+".zip"] = makeZipE2E(tb, p.files)
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := zips[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
}
