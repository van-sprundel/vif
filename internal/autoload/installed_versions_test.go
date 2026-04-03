package autoload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/testhelper"
)

func TestInstalledPHPBasic(t *testing.T) {
	vendorDir := testhelper.TempDir(t, "vendor")
	packages := []pkg.Package{
		{
			Name:              "symfony/console",
			Version:           "v7.0.0",
			VersionNormalized: "7.0.0.0",
			Type:              "library",
			Dist: pkg.Dist{
				Reference: "abc123def",
			},
		},
		{
			Name:              "psr/log",
			Version:           "3.0.0",
			VersionNormalized: "3.0.0.0",
			Type:              "library",
		},
	}

	cfg := &InstalledVersionsConfig{
		RootName:    "my/project",
		RootVersion: "dev-main",
		RootType:    "project",
		DevMode:     true,
		DevPackageNames: map[string]bool{
			"psr/log": true,
		},
	}

	if err := Generate(vendorDir, packages, "testhash", false, nil, true, PlatformCheckFull, cfg); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vendorDir, "composer", "installed.php"))
	if err != nil {
		t.Fatalf("read installed.php: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `'root' => array(`) {
		t.Errorf("installed.php should contain root section, got:\n%s", content)
	}
	if !strings.Contains(content, `'my/project'`) {
		t.Error("installed.php should contain root package name")
	}
	if !strings.Contains(content, `'dev-main'`) {
		t.Error("installed.php should contain root pretty_version")
	}
	if !strings.Contains(content, `'versions' => array(`) {
		t.Errorf("installed.php should contain versions section, got:\n%s", content)
	}
	if !strings.Contains(content, `'symfony/console' => array(`) {
		t.Error("installed.php should contain symfony/console")
	}
	if !strings.Contains(content, `'psr/log' => array(`) {
		t.Error("installed.php should contain psr/log")
	}
	if !strings.Contains(content, `'7.0.0.0'`) {
		t.Error("installed.php should contain normalized version")
	}
	if !strings.Contains(content, `'abc123def'`) {
		t.Error("installed.php should contain reference")
	}
	if !strings.Contains(content, `'dev_requirement' => false`) {
		t.Error("symfony/console should not be marked as dev")
	}
	if !strings.Contains(content, `'dev_requirement' => true`) {
		t.Error("psr/log should be marked as dev")
	}
	if !strings.Contains(content, `'dev' => true`) {
		t.Error("root should have dev=true")
	}
}

func TestInstalledPHPMetapackage(t *testing.T) {
	vendorDir := testhelper.TempDir(t, "vendor")
	packages := []pkg.Package{
		{
			Name:              "acme/meta",
			Version:           "1.0.0",
			VersionNormalized: "1.0.0.0",
			Type:              "metapackage",
		},
	}

	cfg := &InstalledVersionsConfig{
		RootName:        "test/app",
		RootType:        "project",
		DevMode:         false,
		DevPackageNames: map[string]bool{},
	}

	if err := Generate(vendorDir, packages, "h1", false, nil, true, PlatformCheckDisabled, cfg); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vendorDir, "composer", "installed.php"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "'install_path' => null") {
		t.Errorf("metapackage should have null install_path, got:\n%s", content)
	}
}

func TestInstalledPHPProvideReplace(t *testing.T) {
	vendorDir := testhelper.TempDir(t, "vendor")
	packages := []pkg.Package{
		{
			Name:              "acme/polyfill",
			Version:           "1.0.0",
			VersionNormalized: "1.0.0.0",
			Type:              "library",
			Provide: map[string]string{
				"ext-curl": "*",
			},
			Replace: map[string]string{
				"acme/old": "1.0.0",
			},
		},
	}

	cfg := &InstalledVersionsConfig{
		DevPackageNames: map[string]bool{},
	}

	if err := Generate(vendorDir, packages, "h2", false, nil, true, PlatformCheckDisabled, cfg); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vendorDir, "composer", "installed.php"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "'provided' => array(\n                '*'") {
		t.Errorf("should have provided section, got:\n%s", content)
	}
	if !strings.Contains(content, "'replaced' => array(\n                '1.0.0'") {
		t.Errorf("should have replaced section, got:\n%s", content)
	}
}

func TestInstalledVersionsFileWritten(t *testing.T) {
	vendorDir := testhelper.TempDir(t, "vendor")

	cfg := &InstalledVersionsConfig{
		RootName:        "test/app",
		DevPackageNames: map[string]bool{},
	}

	if err := Generate(vendorDir, nil, "h3", false, nil, true, PlatformCheckDisabled, cfg); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vendorDir, "composer", "InstalledVersions.php"))
	if err != nil {
		t.Fatalf("read InstalledVersions.php: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "class InstalledVersions") {
		t.Error("InstalledVersions.php should contain the class")
	}
	if !strings.Contains(content, "namespace Composer;") {
		t.Error("InstalledVersions.php should be in Composer namespace")
	}
	if !strings.Contains(content, "getInstalledPackages") {
		t.Error("InstalledVersions.php should have getInstalledPackages method")
	}
}

func TestInstalledVersionsNotWrittenWithoutConfig(t *testing.T) {
	vendorDir := testhelper.TempDir(t, "vendor")

	if err := Generate(vendorDir, nil, "h4", false, nil, true, PlatformCheckDisabled, nil); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if _, err := os.Stat(filepath.Join(vendorDir, "composer", "InstalledVersions.php")); !os.IsNotExist(err) {
		t.Error("InstalledVersions.php should not exist when ivCfg is nil")
	}
	if _, err := os.Stat(filepath.Join(vendorDir, "composer", "installed.php")); !os.IsNotExist(err) {
		t.Error("installed.php should not exist when ivCfg is nil")
	}
}

func TestInstalledPHPSortedPackageNames(t *testing.T) {
	packages := []pkg.Package{
		{Name: "zebra/z", VersionNormalized: "1.0.0.0"},
		{Name: "alpha/a", VersionNormalized: "1.0.0.0"},
		{Name: "mid/m", VersionNormalized: "1.0.0.0"},
	}

	cfg := &InstalledVersionsConfig{
		DevPackageNames: map[string]bool{},
	}

	content := generateInstalledPHP(packages, cfg)

	idxA := strings.Index(content, "'alpha/a'")
	idxM := strings.Index(content, "'mid/m'")
	idxZ := strings.Index(content, "'zebra/z'")

	if idxA >= idxM || idxM >= idxZ {
		t.Errorf("packages should be sorted alphabetically: alpha(%d) < mid(%d) < zebra(%d)", idxA, idxM, idxZ)
	}
}
