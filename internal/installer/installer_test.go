package installer_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/pkg"
)

// setupCache creates a temp cache and populates it with extracted files for each package.
func setupCache(t *testing.T, packages []pkg.Package) (*cache.Cache, string) {
	t.Helper()
	cacheDir := t.TempDir()
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

		if err := c.Insert(p.Name, p.Version, p.Dist.URL, p.Dist.Reference, key); err != nil {
			t.Fatalf("cache.Insert: %v", err)
		}
	}

	return c, cacheDir
}

func testPackages() []pkg.Package {
	return []pkg.Package{
		{
			Name:    "vendor/foo",
			Version: "1.0.0",
			Type:    "library",
			Dist:    pkg.Dist{URL: "https://example.com/foo.zip", Reference: "abc123"},
		},
		{
			Name:    "vendor/bar",
			Version: "2.0.0",
			Type:    "library",
			Dist:    pkg.Dist{URL: "https://example.com/bar.zip", Reference: "def456"},
		},
	}
}

func TestInstallCreatesVendorLayout(t *testing.T) {
	packages := testPackages()
	c, _ := setupCache(t, packages)
	defer c.Close()

	vendorDir := filepath.Join(t.TempDir(), "vendor")

	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir); err != nil {
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

	vendorDir := filepath.Join(t.TempDir(), "vendor")

	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir); err != nil {
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

	vendorDir := filepath.Join(t.TempDir(), "vendor")

	// First install with both packages.
	inst := installer.New(c)
	if err := inst.Install(packages, nil, vendorDir); err != nil {
		t.Fatalf("Install 1: %v", err)
	}

	// Second install with only the first package — bar should be removed.
	if err := inst.Install(packages[:1], nil, vendorDir); err != nil {
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
			Name:    "vendor/dev-tool",
			Version: "3.0.0",
			Type:    "library",
			Dist:    pkg.Dist{URL: "https://example.com/dev-tool.zip", Reference: "ghi789"},
		},
	}

	allPackages := append(packages, devPackages...)
	c, _ := setupCache(t, allPackages)
	defer c.Close()

	vendorDir := filepath.Join(t.TempDir(), "vendor")

	inst := installer.New(c)
	if err := inst.Install(packages, devPackages, vendorDir); err != nil {
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
}

func TestInstallEmptyPackageList(t *testing.T) {
	cacheDir := t.TempDir()
	c, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer c.Close()

	vendorDir := filepath.Join(t.TempDir(), "vendor")

	inst := installer.New(c)
	if err := inst.Install(nil, nil, vendorDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// installed.json should still be written.
	if _, err := os.Stat(filepath.Join(vendorDir, "composer", "installed.json")); err != nil {
		t.Errorf("installed.json should exist: %v", err)
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

	cacheDir := b.TempDir()
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
		vendorDir := filepath.Join(b.TempDir(), "vendor")
		if err := inst.Install(packages, nil, vendorDir); err != nil {
			b.Fatalf("Install: %v", err)
		}
	}
}
