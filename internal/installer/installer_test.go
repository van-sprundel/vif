package installer_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/testhelper"
)

// setupCache creates a temp cache and populates it with extracted files for each package.
func setupCache(t *testing.T, packages []pkg.Package) (*cache.Cache, string) {
	t.Helper()
	cacheDir := testhelper.TempDir(t, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}

	for _, p := range packages {
		key := cache.CacheKey(p.Dist.URL)
		extractedDir := c.ExtractedDir(key)

		// Create a fake extracted package with some PHP files.
		if err := os.MkdirAll(filepath.Join(extractedDir, "src"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(
			filepath.Join(extractedDir, "src", "Foo.php"),
			[]byte("<?php class Foo {}"),
			0o644,
		); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.WriteFile(
			filepath.Join(extractedDir, "composer.json"),
			[]byte(`{"name":"`+p.Name+`"}`),
			0o644,
		); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		for _, bin := range p.Bin {
			binPath := filepath.Join(extractedDir, filepath.FromSlash(bin))
			if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
				t.Fatalf("MkdirAll bin: %v", err)
			}
			content := "#!/usr/bin/env php\n<?php echo 'bin';\n"
			if strings.HasSuffix(bin, ".sh") || strings.Contains(bin, "sail") || strings.Contains(bin, "sabredav") || strings.Contains(bin, "xhprofile") {
				content = "#!/usr/bin/env sh\necho bin\n"
			}
			if err := os.WriteFile(binPath, []byte(content), 0o755); err != nil {
				t.Fatalf("WriteFile bin: %v", err)
			}
		}

		if err := c.Insert(p.Name, p.Version, p.Dist.URL, p.Dist.Reference, key); err != nil {
			t.Fatalf("cache.Insert: %v", err)
		}
	}

	return c, cacheDir
}

func testPackages() []pkg.Package {
	return []pkg.Package{
		{
			Name:              "vendor/foo",
			Version:           "1.0.0",
			VersionNormalized: "1.0.0.0",
			Type:              "library",
			Dist:              pkg.Dist{URL: "https://example.com/foo.zip", Reference: "abc123"},
			Source:            pkg.Dist{Type: "git", URL: "https://example.com/foo.git", Reference: "abc123"},
			Require:           map[string]string{"php": "^8.2", "vendor/dep": "^2.0"},
			Time:              "2025-01-01T00:00:00+00:00",
		},
		{
			Name:              "vendor/bar",
			Version:           "2.0.0",
			VersionNormalized: "2.0.0.0",
			Type:              "library",
			Dist:              pkg.Dist{URL: "https://example.com/bar.zip", Reference: "def456"},
			Bin:               []string{"bin/bar"},
			Time:              "2025-01-02T00:00:00+00:00",
		},
	}
}

func TestInstallCreatesVendorLayout(t *testing.T) {
	packages := testPackages()
	c, _ := setupCache(t, packages)
	defer c.Close()

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")

	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Verify directory structure.
	for _, p := range packages {
		phpFile := filepath.Join(vendorDir, p.Name, "src", "Foo.php")
		data, err := os.ReadFile(phpFile)
		if err != nil {
			t.Errorf("missing %s: %v", phpFile, err)
			continue
		}
		if string(data) != "<?php class Foo {}" {
			t.Errorf("%s content = %q, want %q", phpFile, data, "<?php class Foo {}")
		}
	}
}

func TestInstallUsesHardlinks(t *testing.T) {
	packages := testPackages()[:1] // just one package
	c, _ := setupCache(t, packages)
	defer c.Close()

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")

	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Check that the vendor file and cache file share the same inode.
	key := cache.CacheKey(packages[0].Dist.URL)
	cacheFile := filepath.Join(c.ExtractedDir(key), "src", "Foo.php")
	vendorFile := filepath.Join(vendorDir, packages[0].Name, "src", "Foo.php")

	cacheStat, err := os.Stat(cacheFile)
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	vendorStat, err := os.Stat(vendorFile)
	if err != nil {
		t.Fatalf("stat vendor file: %v", err)
	}

	cacheIno := cacheStat.Sys().(*syscall.Stat_t).Ino
	vendorIno := vendorStat.Sys().(*syscall.Stat_t).Ino
	if cacheIno != vendorIno {
		t.Errorf("files are not hardlinked: cache inode=%d, vendor inode=%d", cacheIno, vendorIno)
	}
}

func TestInstallRemovesStalePackages(t *testing.T) {
	packages := testPackages()
	c, _ := setupCache(t, packages)
	defer c.Close()

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")

	// First install with both packages.
	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install 1: %v", err)
	}

	// Second install with only the first package — bar should be removed.
	if err := inst.Install(packages[:1], nil, vendorDir, nil); err != nil {
		t.Fatalf("Install 2: %v", err)
	}

	// foo should still exist.
	if _, err := os.Stat(filepath.Join(vendorDir, "vendor/foo")); err != nil {
		t.Errorf("foo should exist: %v", err)
	}

	// bar should be gone.
	if _, err := os.Stat(filepath.Join(vendorDir, "vendor/bar")); !os.IsNotExist(err) {
		t.Errorf("bar should be removed, but stat returned: %v", err)
	}
}

func TestInstallWritesInstalledJSON(t *testing.T) {
	packages := testPackages()
	devPackages := []pkg.Package{
		{
			Name:              "vendor/dev-tool",
			Version:           "3.0.0",
			VersionNormalized: "3.0.0.0",
			Type:              "library",
			Dist:              pkg.Dist{URL: "https://example.com/dev-tool.zip", Reference: "ghi789"},
		},
	}

	allPackages := append(packages, devPackages...)
	c, _ := setupCache(t, allPackages)
	defer c.Close()

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")

	inst := installer.New(c)
	if err := inst.Install(packages, devPackages, vendorDir, &installer.RootPackage{Name: "acme/demo"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Read and parse installed.json.
	data, err := os.ReadFile(filepath.Join(vendorDir, "composer", "installed.json"))
	if err != nil {
		t.Fatalf("read installed.json: %v", err)
	}

	var installed struct {
		Packages        []json.RawMessage `json:"packages"`
		Dev             bool              `json:"dev"`
		DevPackageNames []string          `json:"dev-package-names"`
	}
	if err := json.Unmarshal(data, &installed); err != nil {
		t.Fatalf("unmarshal installed.json: %v", err)
	}

	// Should have all 3 packages.
	if got := len(installed.Packages); got != 3 {
		t.Errorf("len(packages) = %d, want 3", got)
	}

	// Dev should be true.
	if !installed.Dev {
		t.Error("dev should be true")
	}

	// Dev package names should list the dev package.
	if len(installed.DevPackageNames) != 1 || installed.DevPackageNames[0] != "vendor/dev-tool" {
		t.Errorf("dev-package-names = %v, want [vendor/dev-tool]", installed.DevPackageNames)
	}

	var first map[string]interface{}
	if err := json.Unmarshal(installed.Packages[0], &first); err != nil {
		t.Fatalf("unmarshal first package: %v", err)
	}
	if first["name"] != "vendor/bar" {
		t.Fatalf("packages should be sorted by name, first = %v", first["name"])
	}
	if first["version_normalized"] == "" {
		t.Fatalf("package missing version_normalized: %v", first)
	}

	// Each package entry should have install-path.
	for i, raw := range installed.Packages {
		var entry map[string]interface{}
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("unmarshal package[%d]: %v", i, err)
		}
		if _, ok := entry["install-path"]; !ok {
			t.Errorf("package[%d] missing install-path", i)
		}
	}

	installedPHP, err := os.ReadFile(filepath.Join(vendorDir, "composer", "installed.php"))
	if err != nil {
		t.Fatalf("read installed.php: %v", err)
	}
	content := string(installedPHP)
	if !strings.Contains(content, "'root' => array(") {
		t.Fatalf("installed.php missing root metadata:\n%s", content)
	}
	if !strings.Contains(content, "'acme/demo' => array(") {
		t.Fatalf("installed.php missing root version entry:\n%s", content)
	}
	if !strings.Contains(content, "'vendor/dev-tool' => array(") {
		t.Fatalf("installed.php missing dev package entry:\n%s", content)
	}
	if !strings.Contains(content, "'dev_requirement' => true") {
		t.Fatalf("installed.php missing dev requirement flag:\n%s", content)
	}

	if _, err := os.Stat(filepath.Join(vendorDir, "composer", "InstalledVersions.php")); err != nil {
		t.Fatalf("InstalledVersions.php should exist: %v", err)
	}
}

func TestInstallWritesPHPBinProxy(t *testing.T) {
	packages := []pkg.Package{
		{
			Name:    "vendor/php-tool",
			Version: "1.0.0",
			Type:    "library",
			Dist:    pkg.Dist{URL: "https://example.com/php-tool.zip", Reference: "abc123"},
			Bin:     []string{"bin/php-tool"},
		},
	}
	c, _ := setupCache(t, packages)
	defer c.Close()

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")
	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vendorDir, "bin", "php-tool"))
	if err != nil {
		t.Fatalf("read php proxy: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Proxy PHP file generated by Composer") {
		t.Fatalf("php proxy missing Composer header:\n%s", content)
	}
	if !strings.Contains(content, "$GLOBALS['_composer_autoload_path']") {
		t.Fatalf("php proxy missing autoload path:\n%s", content)
	}
	if !strings.Contains(content, "return include __DIR__ . '/..' . '/vendor' . '/php-tool' . '/bin' . '/php-tool';") {
		t.Fatalf("php proxy missing include target:\n%s", content)
	}
}

func TestInstallWritesShellBinProxy(t *testing.T) {
	packages := []pkg.Package{
		{
			Name:    "vendor/shell-tool",
			Version: "1.0.0",
			Type:    "library",
			Dist:    pkg.Dist{URL: "https://example.com/shell-tool.zip", Reference: "abc123"},
			Bin:     []string{"bin/shell-tool.sh"},
		},
	}
	c, _ := setupCache(t, packages)
	defer c.Close()

	key := cache.CacheKey(packages[0].Dist.URL)
	binPath := filepath.Join(c.ExtractedDir(key), "bin", "shell-tool.sh")
	if err := os.WriteFile(binPath, []byte("#!/usr/bin/env sh\necho shell\n"), 0o755); err != nil {
		t.Fatalf("rewrite shell bin: %v", err)
	}

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")
	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vendorDir, "bin", "shell-tool.sh"))
	if err != nil {
		t.Fatalf("read shell proxy: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "#!/usr/bin/env sh") {
		t.Fatalf("shell proxy missing shebang:\n%s", content)
	}
	if !strings.Contains(content, `exec "${dir}/shell-tool.sh" "$@"`) {
		t.Fatalf("shell proxy missing exec target:\n%s", content)
	}
	if !strings.Contains(content, `export COMPOSER_RUNTIME_BIN_DIR=`) {
		t.Fatalf("shell proxy missing COMPOSER_RUNTIME_BIN_DIR:\n%s", content)
	}
}

func TestInstallEmptyPackageList(t *testing.T) {
	cacheDir := testhelper.TempDir(t, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")

	inst := installer.New(c)
	if err := inst.Install(nil, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// installed.json should still be written.
	if _, err := os.Stat(filepath.Join(vendorDir, "composer", "installed.json")); err != nil {
		t.Errorf("installed.json should exist: %v", err)
	}
}

func TestInstallSkipsSourceOnlyPackage(t *testing.T) {
	cacheDir := testhelper.TempDir(t, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")
	packages := []pkg.Package{
		{
			Name:    "vendor/source-only",
			Version: "1.0.0",
			Type:    "library",
			Dist:    pkg.Dist{Type: "zip"},
		},
	}

	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(vendorDir, "vendor", "source-only")); !os.IsNotExist(err) {
		t.Fatalf("source-only package should not be installed, stat err = %v", err)
	}
}

func TestInstallPathPackageFromLocalDirectory(t *testing.T) {
	cacheDir := testhelper.TempDir(t, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	projectDir := testhelper.TempDir(t, "project")
	localPkgDir := filepath.Join(projectDir, "packages", "asset")
	if err := os.MkdirAll(filepath.Join(localPkgDir, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localPkgDir, "src", "Asset.php"), []byte("<?php class Asset {}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	vendorDir := filepath.Join(testhelper.TempDir(t, "vendor"), "vendor")
	packages := []pkg.Package{
		{
			Name:    "urbanheroes-sf/asset",
			Version: "1.0.0",
			Type:    "library",
			Dist:    pkg.Dist{Type: "path", URL: localPkgDir},
		},
	}

	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(vendorDir, "urbanheroes-sf", "asset", "src", "Asset.php")); err != nil {
		t.Fatalf("path package file missing after install: %v", err)
	}
}

func BenchmarkInstall(b *testing.B) {
	n := 50
	packages := make([]pkg.Package, n)
	for i := range n {
		packages[i] = pkg.Package{
			Name:    fmt.Sprintf("vendor/bench%d", i),
			Version: "1.0.0",
			Type:    "library",
			Dist:    pkg.Dist{URL: fmt.Sprintf("https://example.com/bench%d.zip", i), Reference: "ref"},
		}
	}

	cacheDir := testhelper.TempDir(b, "cache")
	c, err := cache.New(cacheDir)
	if err != nil {
		b.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	// Set up cache with fake extracted files.
	for _, p := range packages {
		key := cache.CacheKey(p.Dist.URL)
		extractedDir := filepath.Join(cacheDir, "files", key, "extracted")
		if err := os.MkdirAll(filepath.Join(extractedDir, "src"), 0o755); err != nil {
			b.Fatalf("MkdirAll: %v", err)
		}
		for j := range 5 {
			if err := os.WriteFile(
				filepath.Join(extractedDir, "src", fmt.Sprintf("File%d.php", j)),
				[]byte("<?php // bench"),
				0o644,
			); err != nil {
				b.Fatalf("WriteFile: %v", err)
			}
		}
		_ = c.Insert(p.Name, p.Version, p.Dist.URL, p.Dist.Reference, key)
	}

	inst := installer.New(c)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vendorDir := filepath.Join(testhelper.TempDir(b, "vendor"), "vendor")
		if err := inst.Install(packages, nil, vendorDir, nil); err != nil {
			b.Fatalf("Install: %v", err)
		}
	}
}
