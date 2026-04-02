package installer

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/pkg"
)

// Installer populates vendor/ from the cache via hardlinks (fallback: copy).
type Installer struct {
	cache *cache.Cache
}

// New creates an Installer backed by the given cache.
func New(c *cache.Cache) *Installer {
	return &Installer{cache: c}
}

// Install links all packages from cache into vendorDir, removes stale packages,
// and writes vendor/composer/installed.json.
// packages are the production dependencies, devPackages are the dev dependencies.
func (inst *Installer) Install(packages, devPackages []pkg.Package, vendorDir string) error {
	all := append(packages, devPackages...)

	// Build set of expected package names for stale detection.
	expected := make(map[string]struct{}, len(all))
	for _, p := range all {
		expected[p.Name] = struct{}{}
	}

	// Remove stale packages.
	if err := removeStale(vendorDir, expected); err != nil {
		return fmt.Errorf("remove stale: %w", err)
	}

	// Link each package from cache to vendor.
	for _, p := range all {
		// Skip packages that do not have a cacheable dist archive yet.
		if !pkg.RequiresDownload(p) {
			continue
		}

		key := cache.CacheKey(p.Dist.URL)
		src := inst.cache.ExtractedDir(key)
		dst := filepath.Join(vendorDir, p.Name)

		if err := linkPackage(src, dst); err != nil {
			return fmt.Errorf("install %s: %w", p.Name, err)
		}
	}

	// Create vendor/bin/ proxies.
	if err := installBinaries(vendorDir, all); err != nil {
		return fmt.Errorf("install binaries: %w", err)
	}

	// Write installed.json.
	if err := writeInstalledJSON(vendorDir, packages, devPackages); err != nil {
		return fmt.Errorf("write installed.json: %w", err)
	}

	return nil
}

// linkPackage hardlinks all files from src to dst. Falls back to copy on
// cross-device link errors.
func linkPackage(src, dst string) error {
	// Remove existing destination to get a clean state.
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clean %q: %w", dst, err)
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("rel path: %w", err)
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		// Try hardlink first.
		if err := os.Link(path, target); err != nil {
			// Cross-device fallback: copy.
			if copyErr := copyFile(path, target); copyErr != nil {
				return fmt.Errorf("link/copy %q: link=%w, copy=%w", rel, err, copyErr)
			}
		}
		return nil
	})
}

// copyFile copies a regular file from src to dst, preserving permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// removeStale removes package directories from vendor/ that are not in the expected set.
// It walks one or two levels deep (vendor/<org>/<name>) to find package directories.
func removeStale(vendorDir string, expected map[string]struct{}) error {
	// Walk vendor/ looking for package dirs (two levels: vendor/<org>/<pkg>).
	entries, err := os.ReadDir(vendorDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, org := range entries {
		if !org.IsDir() || org.Name() == "composer" {
			continue
		}

		orgPath := filepath.Join(vendorDir, org.Name())
		pkgEntries, err := os.ReadDir(orgPath)
		if err != nil {
			return err
		}

		for _, p := range pkgEntries {
			if !p.IsDir() {
				continue
			}
			name := org.Name() + "/" + p.Name()
			if _, ok := expected[name]; !ok {
				if err := os.RemoveAll(filepath.Join(orgPath, p.Name())); err != nil {
					return fmt.Errorf("remove stale %s: %w", name, err)
				}
			}
		}

		// Remove empty org directory.
		remaining, _ := os.ReadDir(orgPath)
		if len(remaining) == 0 {
			os.Remove(orgPath)
		}
	}

	return nil
}

// installBinaries creates symlinks in vendor/bin/ for packages that declare bin entries.
func installBinaries(vendorDir string, packages []pkg.Package) error {
	binDir := filepath.Join(vendorDir, "bin")

	// Collect all bin entries.
	var hasBins bool
	for _, p := range packages {
		if len(p.Bin) > 0 {
			hasBins = true
			break
		}
	}
	if !hasBins {
		return nil
	}

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("mkdir bin: %w", err)
	}

	for _, p := range packages {
		for _, bin := range p.Bin {
			// Target: relative from vendor/bin/ to vendor/<package>/<bin>
			target := filepath.Join("..", p.Name, bin)
			name := filepath.Base(bin)
			link := filepath.Join(binDir, name)

			// Remove existing link/file.
			os.Remove(link)

			if err := os.Symlink(target, link); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", link, target, err)
			}
		}
	}

	return nil
}

// installedJSON is the Composer v2 installed.json format.
type installedJSON struct {
	Packages        []installedPackage `json:"packages"`
	Dev             bool               `json:"dev"`
	DevPackageNames []string           `json:"dev-package-names"`
}

// installedPackage is a single entry in installed.json.
type installedPackage struct {
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Type        string       `json:"type"`
	Dist        pkg.Dist     `json:"dist"`
	Autoload    pkg.Autoload `json:"autoload"`
	AutoloadDev pkg.Autoload `json:"autoload-dev,omitempty"`
	InstallPath string       `json:"install-path"`
}

func writeInstalledJSON(vendorDir string, packages, devPackages []pkg.Package) error {
	composerDir := filepath.Join(vendorDir, "composer")
	if err := os.MkdirAll(composerDir, 0o755); err != nil {
		return fmt.Errorf("mkdir composer: %w", err)
	}

	all := append(packages, devPackages...)
	installed := installedJSON{
		Dev:             len(devPackages) > 0 || len(packages) == 0,
		DevPackageNames: make([]string, 0, len(devPackages)),
	}

	for _, p := range devPackages {
		installed.DevPackageNames = append(installed.DevPackageNames, p.Name)
	}

	installed.Packages = make([]installedPackage, 0, len(all))
	for _, p := range all {
		// install-path is relative from vendor/composer/ to vendor/<name>/
		installPath := "../" + strings.Replace(p.Name, "/", "/", 1)

		installed.Packages = append(installed.Packages, installedPackage{
			Name:        p.Name,
			Version:     p.Version,
			Type:        p.Type,
			Dist:        p.Dist,
			Autoload:    p.Autoload,
			AutoloadDev: p.AutoloadDev,
			InstallPath: installPath,
		})
	}

	data, err := json.MarshalIndent(installed, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	return os.WriteFile(filepath.Join(composerDir, "installed.json"), data, 0o644)
}
