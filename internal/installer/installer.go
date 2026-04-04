package installer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/pkg"
)

// Installer populates vendor/ from the cache via hardlinks (fallback: copy).
type Installer struct {
	cache   *cache.Cache
	workers int
}

// installJob represents a single package installation task.
type installJob struct {
	pkg pkg.Package
	src string
	dst string
}

// RootPackage describes the root project metadata Composer exposes at runtime.
type RootPackage struct {
	Name    string
	Version string
	Type    string
}

// New creates an Installer backed by the given cache.
// Workers defaults to min(numCPU, 8) for filesystem operations.
func New(c *cache.Cache) *Installer {
	return &Installer{
		cache:   c,
		workers: min(runtime.NumCPU(), 8),
	}
}

// SetWorkers overrides the number of parallel workers.
func (inst *Installer) SetWorkers(n int) {
	if n > 0 {
		inst.workers = n
	}
}

// Install links all packages from cache into vendorDir, removes stale packages,
// and writes vendor/composer/installed.json.
// packages are the production dependencies, devPackages are the dev dependencies.
// Returns stats with counts of installs, updates, removals, and skips.
func (inst *Installer) Install(packages, devPackages []pkg.Package, vendorDir string, root *RootPackage) (InstallStats, error) {
	all := append(packages, devPackages...)

	expected := make(map[string]struct{}, len(all))
	for _, p := range all {
		expected[p.Name] = struct{}{}
	}

	installed := readInstalledVersions(vendorDir)

	var toInstall []installJob
	var toRemove []string
	stats := InstallStats{}

	for _, p := range all {
		dst := filepath.Join(vendorDir, p.Name)

		if installedVer, ok := installed[p.Name]; ok && installedVer == p.Version {
			if _, statErr := os.Stat(dst); statErr == nil {
				stats.Skipped++
				continue
			}
		}

		var src string
		if p.Dist.Type == "path" && strings.TrimSpace(p.Dist.URL) != "" {
			src = p.Dist.URL
		} else if pkg.RequiresGitClone(p) {
			key := cache.CacheKey(p.Source.URL + "@" + p.Source.Reference)
			src = inst.cache.ExtractedDir(key)
		} else if pkg.RequiresDownload(p) {
			key := cache.CacheKey(p.Dist.URL)
			src = inst.cache.ExtractedDir(key)
		} else {
			continue
		}

		toInstall = append(toInstall, installJob{
			pkg: p,
			src: src,
			dst: dst,
		})

		if _, existed := installed[p.Name]; existed {
			stats.Updated++
		} else {
			stats.Installed++
		}
	}

	for name := range installed {
		if _, ok := expected[name]; !ok {
			toRemove = append(toRemove, filepath.Join(vendorDir, name))
			stats.Removed++
		}
	}

	for _, dir := range toRemove {
		os.RemoveAll(dir)
	}

	cleanEmptyVendorDirs(vendorDir, expected)

	if err := inst.installParallel(toInstall); err != nil {
		return stats, err
	}

	if err := installBinaries(vendorDir, all); err != nil {
		return stats, fmt.Errorf("install binaries: %w", err)
	}

	if err := writeInstalledMetadata(vendorDir, packages, devPackages, root); err != nil {
		return stats, fmt.Errorf("write installed metadata: %w", err)
	}

	return stats, nil
}

// InstallStats returns counts from the last install operation for reporting.
type InstallStats struct {
	Installed int
	Updated   int
	Removed   int
	Skipped   int
}

// readInstalledVersions reads vendor/composer/installed.json and returns
// a map of package name -> version for currently installed packages.
func readInstalledVersions(vendorDir string) map[string]string {
	result := make(map[string]string)

	data, err := os.ReadFile(filepath.Join(vendorDir, "composer", "installed.json"))
	if err != nil {
		return result
	}

	var ij installedJSON
	if err := json.Unmarshal(data, &ij); err != nil {
		return result
	}

	for _, p := range ij.Packages {
		result[p.Name] = p.Version
	}

	return result
}

// cleanEmptyVendorDirs removes empty org directories under vendor/
// that have no remaining package subdirectories in the expected set.
func cleanEmptyVendorDirs(vendorDir string, expected map[string]struct{}) {
	entries, err := os.ReadDir(vendorDir)
	if err != nil {
		return
	}

	for _, org := range entries {
		if !org.IsDir() || org.Name() == "composer" || org.Name() == "bin" {
			continue
		}

		orgPath := filepath.Join(vendorDir, org.Name())
		pkgEntries, err := os.ReadDir(orgPath)
		if err != nil {
			continue
		}

		hasPackages := false
		for _, p := range pkgEntries {
			if !p.IsDir() {
				continue
			}
			name := org.Name() + "/" + p.Name()
			if _, ok := expected[name]; ok {
				hasPackages = true
				break
			}
		}

		if !hasPackages {
			remaining, _ := os.ReadDir(orgPath)
			if len(remaining) == 0 {
				os.Remove(orgPath)
			}
		}
	}
}

// installParallel links packages from cache to vendor using a worker pool.
func (inst *Installer) installParallel(jobs []installJob) error {
	if len(jobs) == 0 {
		return nil
	}

	work := make(chan int, len(jobs))
	errs := make([]error, len(jobs))

	var wg sync.WaitGroup
	for range inst.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				job := jobs[idx]
				if err := linkPackage(job.src, job.dst); err != nil {
					errs[idx] = fmt.Errorf("install %s: %w", job.pkg.Name, err)
				}
			}
		}()
	}

	for i := range jobs {
		work <- i
	}
	close(work)
	wg.Wait()

	// Return first error encountered.
	for _, err := range errs {
		if err != nil {
			return err
		}
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

var phpProxyRe = regexp.MustCompile(`(?s)^(#!.*\r?\n)?[\r\n\t ]*<\?php`)

// installBinaries creates Composer-compatible proxy scripts in vendor/bin/
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
			targetPath := filepath.Join(vendorDir, filepath.FromSlash(p.Name), filepath.FromSlash(bin))
			name := filepath.Base(bin)
			proxyFile := filepath.Join(binDir, name)
			if info, err := os.Stat(targetPath); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return fmt.Errorf("stat binary %s: %w", targetPath, err)
			} else if info.IsDir() {
				continue
			}

			// Remove existing link/file.
			os.Remove(proxyFile)

			content, err := generateBinProxy(vendorDir, proxyFile, targetPath)
			if err != nil {
				return fmt.Errorf("generate bin proxy %s: %w", proxyFile, err)
			}
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
	Name              string            `json:"name"`
	Version           string            `json:"version"`
	VersionNormalized string            `json:"version_normalized"`
	Source            pkg.Dist          `json:"source,omitempty"`
	Dist              pkg.Dist          `json:"dist,omitempty"`
	Require           map[string]string `json:"require,omitempty"`
	RequireDev        map[string]string `json:"require-dev,omitempty"`
	Provide           map[string]string `json:"provide,omitempty"`
	Replace           map[string]string `json:"replace,omitempty"`
	Conflict          map[string]string `json:"conflict,omitempty"`
	Time              string            `json:"time,omitempty"`
	Type              string            `json:"type,omitempty"`
	InstallationSrc   string            `json:"installation-source,omitempty"`
	Autoload          pkg.Autoload      `json:"autoload,omitempty"`
	AutoloadDev       pkg.Autoload      `json:"autoload-dev,omitempty"`
	IncludePath       []string          `json:"include-path,omitempty"`
	Bin               []string          `json:"bin,omitempty"`
	InstallPath       string            `json:"install-path"`
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
	sort.Strings(installed.DevPackageNames)

	installed.Packages = make([]installedPackage, 0, len(all))
	for _, p := range all {
		installPath := "../" + p.Name
		versionNormalized := p.VersionNormalized
		if strings.TrimSpace(versionNormalized) == "" {
			versionNormalized = normalizedVersion(p.Version)
		}

		installed.Packages = append(installed.Packages, installedPackage{
			Name:              p.Name,
			Version:           p.Version,
			VersionNormalized: versionNormalized,
			Source:            p.Source,
			Dist:              p.Dist,
			Require:           p.Require,
			RequireDev:        p.RequireDev,
			Provide:           p.Provide,
			Replace:           p.Replace,
			Conflict:          p.Conflict,
			Time:              p.Time,
			Type:              p.Type,
			InstallationSrc:   installationSource(p),
			Autoload:          p.Autoload,
			AutoloadDev:       p.AutoloadDev,
			IncludePath:       p.IncludePath,
			Bin:               p.Bin,
			InstallPath:       installPath,
		})
	}
	sort.Slice(installed.Packages, func(i, j int) bool {
		return installed.Packages[i].Name < installed.Packages[j].Name
	})

	data, err := marshalJSONIndent(installed, "", "    ")
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

func generateBinProxy(vendorDir, proxyFile, targetPath string) (string, error) {
	rel, err := filepath.Rel(filepath.Dir(proxyFile), targetPath)
	if err != nil {
		return "", fmt.Errorf("relative target path: %w", err)
	}
	rel = filepath.ToSlash(rel)

	contents, err := os.ReadFile(targetPath)
	if err != nil {
		return "", fmt.Errorf("read target: %w", err)
	}

	if phpProxyRe.Match(contents) {
		return binProxyPHP(filepath.ToSlash(vendorDir), filepath.ToSlash(targetPath), rel, contents), nil
	}

	return binProxyShell(rel), nil
}

func binProxyPHP(vendorDir, targetPath, rel string, contents []byte) string {
	matches := phpProxyRe.FindSubmatch(contents)
	shebang := "#!/usr/bin/env php"
	streamHint := ""
	streamProxyCode := ""
	binPathExported := phpPathExpr(rel)
	globalsCode := "$GLOBALS['_composer_bin_dir'] = __DIR__;\n" +
		"$GLOBALS['_composer_autoload_path'] = __DIR__ . '/..' . '/autoload.php';\n"
	phpunitHack1 := ""
	phpunitHack2 := ""

	if len(matches) > 1 && len(matches[1]) > 0 {
		shebang = strings.TrimSpace(string(matches[1]))
	}

	if filepath.ToSlash(targetPath) == filepath.ToSlash(filepath.Join(vendorDir, "phpunit", "phpunit", "phpunit")) {
		globalsCode += "$GLOBALS['__PHPUNIT_ISOLATION_EXCLUDE_LIST'] = $GLOBALS['__PHPUNIT_ISOLATION_BLACKLIST'] = array(realpath(" + binPathExported + "));\n"
		phpunitHack1 = "'phpvfscomposer://'."
		phpunitHack2 = `
                $data = str_replace('__DIR__', var_export(dirname($this->realpath), true), $data);
                $data = str_replace('__FILE__', var_export($this->realpath, true), $data);`
	}

	if len(matches) == 0 || strings.TrimSpace(string(matches[0])) != "<?php" {
		streamHint = " using a stream wrapper to prevent the shebang from being output on PHP<8\n *"
		streamProxyCode = `if (PHP_VERSION_ID < 80000) {
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
                $opened_path = ` + phpunitHack1 + `$this->realpath;
                $this->handle = fopen($this->realpath, $mode);
                $this->position = 0;

                return (bool) $this->handle;
            }

            public function stream_read($count)
            {
                $data = fread($this->handle, $count);

                if ($this->position === 0) {
                    $data = preg_replace('{^#!.*\r?\n}', '', $data);
                }` + phpunitHack2 + `

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
        return include("phpvfscomposer://" . ` + binPathExported + `);
    }
}

`
	}

	return shebang + `
<?php

/**
 * Proxy PHP file generated by Composer
 *
 * This file includes the referenced bin path (` + rel + `)
 *` + streamHint + `
 * @generated
 */

namespace Composer;

` + globalsCode + `
` + streamProxyCode + `return include ` + binPathExported + `;
`
}

func binProxyShell(rel string) string {
	binDir := filepath.ToSlash(filepath.Dir(rel))
	binFile := filepath.Base(rel)

	return `#!/usr/bin/env sh

# Support bash to support ` + "`source`" + ` with fallback on $0 if this does not run with bash
# https://stackoverflow.com/a/35006505/6512
selfArg="$BASH_SOURCE"
if [ -z "$selfArg" ]; then
    selfArg="$0"
fi

self=$(realpath "$selfArg" 2> /dev/null)
if [ -z "$self" ]; then
    self="$selfArg"
fi

dir=$(cd "${self%[/\\]*}" > /dev/null; cd ` + shellQuote(binDir) + ` && pwd)

if [ -d /proc/cygdrive ]; then
    case $(which php) in
        $(readlink -n /proc/cygdrive)/*)
            # We are in Cygwin using Windows php, so the path must be translated
            dir=$(cygpath -m "$dir");
            ;;
    esac
fi

export COMPOSER_RUNTIME_BIN_DIR="$(cd "${self%[/\\]*}" > /dev/null; pwd)"

# If bash is sourcing this file, we have to source the target as well
bashSource="$BASH_SOURCE"
if [ -n "$bashSource" ]; then
    if [ "$bashSource" != "$0" ]; then
        source "${dir}/` + binFile + `" "$@"
        return
    fi
fi

exec "${dir}/` + binFile + `" "$@"
`
}

func phpPathExpr(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 {
		return "__DIR__"
	}

	var b strings.Builder
	b.WriteString("__DIR__")
	for _, part := range parts {
		b.WriteString(" . ")
		b.WriteString(phpString("/" + part))
	}
	return b.String()
}

func shellQuote(s string) string {
	if s == "." || s == "" {
		return "."
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func installationSource(p pkg.Package) string {
	if pkg.RequiresGitClone(p) {
		return "source"
	}
	if strings.TrimSpace(p.Dist.Type) != "" || strings.TrimSpace(p.Dist.URL) != "" {
		return "dist"
	}
	return ""
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

func marshalJSONIndent(v interface{}, prefix, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
