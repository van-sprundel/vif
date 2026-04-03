package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/testhelper"
)

func TestApplyLocalPathPackagesSetsDistForMatchingPackage(t *testing.T) {
	projectDir := testhelper.TempDir(t, "project")
	localRepoDir := filepath.Join(projectDir, "packages", "asset")
	if err := os.MkdirAll(localRepoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRepoDir, "composer.json"), []byte(`{"name":"urbanheroes-sf/asset"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	packages := []pkg.Package{
		{Name: "urbanheroes-sf/asset", Type: "library", Dist: pkg.Dist{}},
		{Name: "acme/other", Type: "library", Dist: pkg.Dist{Type: "zip", URL: "https://example.com/other.zip"}},
	}

	repositories := []composer.Repository{
		{Type: "path", URL: "packages/*"},
	}

	got, err := applyLocalPathPackages(packages, repositories, projectDir)
	if err != nil {
		t.Fatalf("applyLocalPathPackages: %v", err)
	}

	if got[0].Dist.Type != "path" {
		t.Fatalf("Dist.Type = %q, want path", got[0].Dist.Type)
	}
	if got[0].Dist.URL != localRepoDir {
		t.Fatalf("Dist.URL = %q, want %q", got[0].Dist.URL, localRepoDir)
	}
	if got[1].Dist.URL != "https://example.com/other.zip" {
		t.Fatalf("unrelated package dist changed: %q", got[1].Dist.URL)
	}
}

func TestPromoteSourceToDistSetsDistFromPathSource(t *testing.T) {
	projectDir := testhelper.TempDir(t, "project")

	packages := []pkg.Package{
		{
			Name: "urbanheroes-sf/asset",
			Type: "library",
			Dist: pkg.Dist{},
			Source: pkg.Dist{
				Type:      "path",
				URL:       "packages/asset",
				Reference: "abc123",
			},
		},
		{
			Name: "acme/other",
			Type: "library",
			Dist: pkg.Dist{Type: "zip", URL: "https://example.com/other.zip"},
		},
		{
			Name:   "acme/empty",
			Type:   "library",
			Dist:   pkg.Dist{},
			Source: pkg.Dist{Type: "git", URL: "https://github.com/acme/empty.git"},
		},
	}

	got := promoteSourceToDist(packages, projectDir)

	// Package with path source should get dist promoted.
	if got[0].Dist.Type != "path" {
		t.Fatalf("Dist.Type = %q, want path", got[0].Dist.Type)
	}
	wantURL := filepath.Join(projectDir, "packages/asset")
	if got[0].Dist.URL != wantURL {
		t.Fatalf("Dist.URL = %q, want %q", got[0].Dist.URL, wantURL)
	}

	// Package with existing dist should be unchanged.
	if got[1].Dist.URL != "https://example.com/other.zip" {
		t.Fatalf("unrelated package dist changed: %q", got[1].Dist.URL)
	}

	// Package with non-path source should NOT be promoted.
	if got[2].Dist.Type != "" {
		t.Fatalf("non-path source promoted: Dist.Type = %q", got[2].Dist.Type)
	}
}
