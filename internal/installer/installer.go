package installer

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/pkg"
)

// Installer populates vendor/ from the cache via hardlinks (fallback: copy).
type Installer struct {
	cache *cache.Cache
}

// RootPackage describes the root project metadata Composer exposes at runtime.
type RootPackage struct {
	Name    string
	Version string
	Type    string
}

// New creates an Installer backed by the given cache.
func New(c *cache.Cache) *Installer {
	return &Installer{cache: c}
}

// Install links all packages from cache into vendorDir, removes stale packages,
// and writes vendor/composer/installed.json.
// packages are the production dependencies, devPackages are the dev dependencies.
func (inst *Installer) Install(packages, devPackages []pkg.Package, vendorDir string, root *RootPackage) error {
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
		if p.Dist.Type == "path" && strings.TrimSpace(p.Dist.URL) != "" {
			dst := filepath.Join(vendorDir, p.Name)
			if err := linkPackage(p.Dist.URL, dst); err != nil {
				return fmt.Errorf("install path package %s: %w", p.Name, err)
			}
			continue
		}

		if pkg.RequiresGitClone(p) {
			key := cache.CacheKey(p.Source.URL + "@" + p.Source.Reference)
			src := inst.cache.ExtractedDir(key)
			dst := filepath.Join(vendorDir, p.Name)
			if err := linkPackage(src, dst); err != nil {
				return fmt.Errorf("install %s: %w", p.Name, err)
			}
			continue
		}

		// Skip packages that do not have a cacheable/installable source.
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
	if err := writeInstalledMetadata(vendorDir, packages, devPackages, root); err != nil {
		return fmt.Errorf("write installed metadata: %w", err)
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

// binProxyPHP generates a Composer-compatible bin proxy PHP script.
// binPath is the relative path from vendor/ to the target (e.g. "phpunit/phpunit/phpunit").
func binProxyPHP(binPath string) string {
	return "#!/usr/bin/env php\n<?php\n\n" +
		"/**\n" +
		" * Proxy PHP file generated by Composer\n" +
		" *\n" +
		" * This file includes the referenced bin path (../" + binPath + ")\n" +
		" * using a stream wrapper to prevent the shebang from being output on PHP<8\n" +
		" *\n" +
		" * @generated\n" +
		" */\n\n" +
		"namespace Composer;\n\n" +
		"$GLOBALS['_composer_bin_dir'] = __DIR__;\n" +
		"$GLOBALS['_composer_autoload_path'] = __DIR__ . '/..'.'/autoload.php';\n\n" +
		`if (PHP_VERSION_ID < 80000) {
    if (!class_exists('Composer\BinProxyWrapper')) {
        /**
         * @internal
         */
        final class BinProxyWrapper
        {
            private $handle;
            private $position;
            private $realpath;

            public function stream_open($path, $mode, $options, &$opened_path)
            {
                // get rid of phpvfscomposer:// prefix for __FILE__ & __DIR__ resolution
                $opened_path = substr($path, 17);
                $this->realpath = realpath($opened_path) ?: $opened_path;
                $opened_path = $this->realpath;
                $this->handle = fopen($this->realpath, $mode);
                $this->position = 0;

                return (bool) $this->handle;
            }

            public function stream_read($count)
            {
                $data = fread($this->handle, $count);

                if ($this->position === 0) {
                    $data = preg_replace('{^#!.*\r?\n}', '', $data);
                }

                $this->position += strlen($data);

                return $data;
            }

            public function stream_cast($castAs)
            {
                return $this->handle;
            }

            public function stream_close()
            {
                fclose($this->handle);
            }

            public function stream_lock($operation)
            {
                return $operation ? flock($this->handle, $operation) : true;
            }

            public function stream_seek($offset, $whence)
            {
                if (0 === fseek($this->handle, $offset, $whence)) {
                    $this->position = ftell($this->handle);
                    return true;
                }

                return false;
            }

            public function stream_tell()
            {
                return $this->position;
            }

            public function stream_eof()
            {
                return feof($this->handle);
            }

            public function stream_stat()
            {
                return array();
            }

            public function stream_set_option($option, $arg1, $arg2)
            {
                return true;
            }

            public function url_stat($path, $flags)
            {
                $path = substr($path, 17);
                if (file_exists($path)) {
                    return stat($path);
                }

                return false;
            }
        }
    }

    if (
        (function_exists('stream_get_wrappers') && in_array('phpvfscomposer', stream_get_wrappers(), true))
        || (function_exists('stream_wrapper_register') && stream_wrapper_register('phpvfscomposer', 'Composer\BinProxyWrapper'))
    ) {
        return include("phpvfscomposer://" . __DIR__ . '/..'.'/` + binPath + `');
    }
}

return include __DIR__ . '/..'.'/` + binPath + `';
`
}

// installBinaries creates Composer-compatible PHP proxy scripts in vendor/bin/
// for packages that declare bin entries.
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
			// binPath: relative from vendor/ (e.g. "phpunit/phpunit/phpunit")
			binPath := filepath.ToSlash(filepath.Join(p.Name, bin))
			name := filepath.Base(bin)
			proxyFile := filepath.Join(binDir, name)

			// Remove existing link/file.
			os.Remove(proxyFile)

			content := binProxyPHP(binPath)
			if err := os.WriteFile(proxyFile, []byte(content), 0o755); err != nil {
				return fmt.Errorf("write bin proxy %s: %w", proxyFile, err)
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

func writeInstalledMetadata(vendorDir string, packages, devPackages []pkg.Package, root *RootPackage) error {
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

	if err := os.WriteFile(filepath.Join(composerDir, "installed.json"), data, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(composerDir, "installed.php"), []byte(generateInstalledPHP(packages, devPackages, root)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(composerDir, "InstalledVersions.php"), []byte(installedVersionsPHP), 0o644); err != nil {
		return err
	}

	return nil
}

func generateInstalledPHP(packages, devPackages []pkg.Package, root *RootPackage) string {
	rootMeta := normalizeRootPackage(root)

	type versionEntry struct {
		name    string
		version string
		typ     string
		path    string
		dev     bool
	}

	allEntries := make([]versionEntry, 0, len(packages)+len(devPackages)+1)
	allEntries = append(allEntries, versionEntry{
		name:    rootMeta.Name,
		version: rootMeta.Version,
		typ:     rootMeta.Type,
		path:    "__DIR__ . '/../../'",
		dev:     false,
	})
	for _, p := range packages {
		allEntries = append(allEntries, versionEntry{
			name:    p.Name,
			version: p.Version,
			typ:     p.Type,
			path:    installPathExpr(p.Name),
			dev:     false,
		})
	}
	for _, p := range devPackages {
		allEntries = append(allEntries, versionEntry{
			name:    p.Name,
			version: p.Version,
			typ:     p.Type,
			path:    installPathExpr(p.Name),
			dev:     true,
		})
	}

	sort.Slice(allEntries, func(i, j int) bool { return allEntries[i].name < allEntries[j].name })

	var b strings.Builder
	b.WriteString("<?php return array(\n")
	b.WriteString("    'root' => array(\n")
	fmt.Fprintf(&b, "        'name' => %s,\n", phpString(rootMeta.Name))
	fmt.Fprintf(&b, "        'pretty_version' => %s,\n", phpString(prettyVersion(rootMeta.Version)))
	fmt.Fprintf(&b, "        'version' => %s,\n", phpString(normalizedVersion(rootMeta.Version)))
	b.WriteString("        'reference' => null,\n")
	fmt.Fprintf(&b, "        'type' => %s,\n", phpString(rootMeta.Type))
	b.WriteString("        'install_path' => __DIR__ . '/../../',\n")
	b.WriteString("        'aliases' => array(),\n")
	fmt.Fprintf(&b, "        'dev' => %s,\n", phpBool(len(devPackages) > 0 || len(packages) == 0))
	b.WriteString("    ),\n")
	b.WriteString("    'versions' => array(\n")
	for _, entry := range allEntries {
		fmt.Fprintf(&b, "        %s => array(\n", phpString(entry.name))
		fmt.Fprintf(&b, "            'pretty_version' => %s,\n", phpString(prettyVersion(entry.version)))
		fmt.Fprintf(&b, "            'version' => %s,\n", phpString(normalizedVersion(entry.version)))
		b.WriteString("            'reference' => null,\n")
		fmt.Fprintf(&b, "            'type' => %s,\n", phpString(packageType(entry.typ)))
		fmt.Fprintf(&b, "            'install_path' => %s,\n", entry.path)
		b.WriteString("            'aliases' => array(),\n")
		fmt.Fprintf(&b, "            'dev_requirement' => %s,\n", phpBool(entry.dev))
		b.WriteString("        ),\n")
	}
	b.WriteString("    ),\n")
	b.WriteString(");\n")
	return b.String()
}

func normalizeRootPackage(root *RootPackage) RootPackage {
	if root == nil {
		return RootPackage{
			Name:    "__root__",
			Version: "",
			Type:    "library",
		}
	}

	return RootPackage{
		Name:    firstNonEmpty(root.Name, "__root__"),
		Version: root.Version,
		Type:    packageType(root.Type),
	}
}

func installPathExpr(name string) string {
	return "__DIR__ . " + phpString("/../"+name)
}

func packageType(typ string) string {
	if strings.TrimSpace(typ) == "" {
		return "library"
	}
	return typ
}

func prettyVersion(version string) string {
	if strings.TrimSpace(version) == "" {
		return "1.0.0+no-version-set"
	}
	return version
}

func normalizedVersion(version string) string {
	if strings.TrimSpace(version) == "" {
		return "1.0.0.0"
	}
	return version
}

func phpString(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'"
}

func phpBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

const installedVersionsPHP = `<?php

namespace Composer;

use Composer\Semver\VersionParser;

final class InstalledVersions
{
    private static $installed;

    public static function getInstalledPackages()
    {
        return array_keys(self::getInstalled()['versions']);
    }

    public static function getInstalledPackagesByType($type)
    {
        $packages = array();
        foreach (self::getInstalled()['versions'] as $name => $package) {
            if (($package['type'] ?? null) === $type) {
                $packages[] = $name;
            }
        }

        return $packages;
    }

    public static function isInstalled($packageName, $includeDevRequirements = true)
    {
        if (!isset(self::getInstalled()['versions'][$packageName])) {
            return false;
        }

        if ($includeDevRequirements) {
            return true;
        }

        return !((self::getInstalled()['versions'][$packageName]['dev_requirement'] ?? false) === true);
    }

    public static function satisfies(VersionParser $parser, $packageName, $constraint)
    {
        $constraint = $parser->parseConstraints((string) $constraint);
        $provided = $parser->parseConstraints(self::getVersionRanges($packageName));

        return $provided->matches($constraint);
    }

    public static function getVersionRanges($packageName)
    {
        $package = self::getVersionEntry($packageName);
        $ranges = array();

        if (isset($package['pretty_version'])) {
            $ranges[] = $package['pretty_version'];
        }
        if (array_key_exists('aliases', $package)) {
            $ranges = array_merge($ranges, $package['aliases']);
        }
        if (array_key_exists('replaced', $package)) {
            $ranges = array_merge($ranges, $package['replaced']);
        }
        if (array_key_exists('provided', $package)) {
            $ranges = array_merge($ranges, $package['provided']);
        }

        return implode(' || ', $ranges);
    }

    public static function getVersion($packageName)
    {
        return self::getVersionEntry($packageName)['version'] ?? null;
    }

    public static function getPrettyVersion($packageName)
    {
        return self::getVersionEntry($packageName)['pretty_version'] ?? null;
    }

    public static function getReference($packageName)
    {
        return self::getVersionEntry($packageName)['reference'] ?? null;
    }

    public static function getInstallPath($packageName)
    {
        return self::getVersionEntry($packageName)['install_path'] ?? null;
    }

    public static function getRootPackage()
    {
        return self::getInstalled()['root'];
    }

    public static function getRawData()
    {
        return self::getInstalled();
    }

    public static function reload($data)
    {
        self::$installed = $data;
    }

    private static function getInstalled()
    {
        if (null === self::$installed) {
            self::$installed = require __DIR__ . '/installed.php';
        }

        return self::$installed;
    }

    private static function getVersionEntry($packageName)
    {
        if (!isset(self::getInstalled()['versions'][$packageName])) {
            throw new \OutOfBoundsException('Package "' . $packageName . '" is not installed');
        }

        return self::getInstalled()['versions'][$packageName];
    }
}
`
