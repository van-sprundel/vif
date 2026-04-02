//go:build compat

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/van-sprundel/vif/internal/autoload"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/testhelper"
)

// compatFixturesDir is where compat fixture subdirectories live.
const compatFixturesDir = "testdata/fixtures/compat"

// compatSkipFiles are vendor/composer/ files vif doesn't generate yet.
var compatSkipFiles = map[string]bool{
	"vendor/composer/installed.php":         true,
	"vendor/composer/InstalledVersions.php": true,
	"vendor/composer/platform_check.php":    true,
}

// compatAutoloaderFiles are the autoloader files whose content we compare.
var compatAutoloaderFiles = map[string]bool{
	"vendor/composer/autoload_static.php":     true,
	"vendor/composer/autoload_psr4.php":       true,
	"vendor/composer/autoload_namespaces.php": true,
	"vendor/composer/autoload_classmap.php":   true,
	"vendor/composer/autoload_files.php":      true,
}

// compatResult holds a per-fixture compat result for the summary test.
type compatResult struct {
	fixture           string
	composerTotal     int
	vifTotal          int
	missingFiles      []string
	extraFiles        []string
	contentMismatches []string
	hashMismatches    []string
	passed            bool
}

// sharedCompatCache holds a once-initialized shared cache for compat tests.
var sharedCompatCache struct {
	once    sync.Once
	dir     string
	initErr error
}

// getSharedCache returns a shared cache dir, initialising it once per test run.
// Set VIF_TEST_TEMP_DIR or VIF_COMPAT_CACHE_DIR to override the default repo-local cache location.
func getSharedCache(t *testing.T) (string, error) {
	t.Helper()
	sharedCompatCache.once.Do(func() {
		base, err := testhelper.GetTestTempBase()
		if err != nil {
			sharedCompatCache.initErr = err
			return
		}
		dir, err := os.MkdirTemp(base, "vif-compat-cache-*")
		if err != nil {
			sharedCompatCache.initErr = fmt.Errorf("create shared cache dir: %w", err)
			return
		}
		sharedCompatCache.dir = dir
		t.Cleanup(func() { os.RemoveAll(dir) })
	})
	return sharedCompatCache.dir, sharedCompatCache.initErr
}

// compatTempDir creates a per-test working directory on the same filesystem as
// the shared cache so installer hardlinks do not degrade into cross-device copies.
func compatTempDir(t *testing.T, prefix string) string {
	return testhelper.TempDir(t, prefix)
}

// discoverCompatFixtures returns a sorted list of fixture directory names under
// testdata/fixtures/compat/.
func discoverCompatFixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(compatFixturesDir)
	if os.IsNotExist(err) {
		t.Skipf("compat fixtures dir not found: %s (run testdata/fixtures/compat/fetch.sh first)", compatFixturesDir)
	}
	if err != nil {
		t.Fatalf("read fixtures dir: %v", err)
	}

	var fixtures []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// A valid fixture must have both composer.json and composer.lock.
		dir := filepath.Join(compatFixturesDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "composer.lock")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "composer.json")); err != nil {
			continue
		}
		fixtures = append(fixtures, e.Name())
	}
	sort.Strings(fixtures)

	if len(fixtures) == 0 {
		t.Skipf("no valid compat fixtures found in %s (run testdata/fixtures/compat/fetch.sh first)", compatFixturesDir)
	}
	return fixtures
}

// runComposerInstall runs `composer install` in dir with standard compat flags.
// Returns (vendorDir, nil) on success, or ("", error) if composer is not found
// or the install fails.
func runComposerInstall(t *testing.T, dir string) (string, error) {
	t.Helper()
	composerBin, err := exec.LookPath("composer")
	if err != nil {
		return "", fmt.Errorf("composer not in PATH: %w", err)
	}

	cmd := exec.Command(
		composerBin,
		"install",
		"--no-scripts",
		"--no-plugins",
		"--no-interaction",
		"--ignore-platform-reqs",
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COMPOSER_NO_AUDIT=1")

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("composer install failed: %w\noutput:\n%s", err, out)
	}
	return filepath.Join(dir, "vendor"), nil
}

// runVifInstall runs the vif install pipeline in dir using a shared cache.
func runVifInstall(t *testing.T, dir, cacheDir string) (string, error) {
	t.Helper()

	lf, err := lockfile.Parse(filepath.Join(dir, "composer.lock"))
	if err != nil {
		return "", fmt.Errorf("parse lockfile: %w", err)
	}

	// Check composer.json for optimize-autoloader config and root package metadata.
	optimized := false
	prependAutoloader := true
	var root *installer.RootPackage
	if cj, err := composer.Parse(filepath.Join(dir, "composer.json")); err == nil {
		optimized = cj.Config.OptimizeAutoloader
		prependAutoloader = cj.Config.PrependAutoloaderOrDefault()
		root = &installer.RootPackage{
			Name:    cj.Name,
			Version: cj.Version,
			Type:    cj.Type,
		}
	}

	allPackages := append(lf.Packages, lf.PackagesDev...)

	c, err := cache.New(cacheDir)
	if err != nil {
		return "", fmt.Errorf("cache.New: %w", err)
	}
	defer c.Close()

	dl := downloader.New(c, 0)
	results, err := dl.Download(context.Background(), allPackages)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	for _, r := range results {
		if r.Err != nil && !r.Skipped {
			return "", fmt.Errorf("download %s: %w", r.Package.Name, r.Err)
		}
	}

	vendorDir := filepath.Join(dir, "vendor")
	inst := installer.New(c)
	if err := inst.Install(lf.Packages, lf.PackagesDev, vendorDir, root); err != nil {
		return "", fmt.Errorf("install: %w", err)
	}

	if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash, optimized, nil, prependAutoloader); err != nil {
		return "", fmt.Errorf("autoload.Generate: %w", err)
	}

	return vendorDir, nil
}

// walkVendor returns sorted relative paths of all files under vendorDir,
// excluding paths in skipFiles (relative to the project root, i.e. "vendor/...").
func walkVendor(t *testing.T, vendorDir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(vendorDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Dir(vendorDir), path)
		if err != nil {
			return fmt.Errorf("rel: %w", err)
		}
		if compatSkipFiles[rel] {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk vendor %s: %v", vendorDir, err)
	}
	sort.Strings(files)
	return files
}

// sha256File returns the hex-encoded SHA256 digest of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// compareVendorDirs compares the two vendor directories and returns a compatResult.
func compareVendorDirs(t *testing.T, fixture, composerVendor, vifVendor string) compatResult {
	t.Helper()

	composerFiles := walkVendor(t, composerVendor)
	vifFiles := walkVendor(t, vifVendor)

	composerSet := make(map[string]bool, len(composerFiles))
	for _, f := range composerFiles {
		composerSet[f] = true
	}
	vifSet := make(map[string]bool, len(vifFiles))
	for _, f := range vifFiles {
		vifSet[f] = true
	}

	var missingFromVif []string
	for _, f := range composerFiles {
		if !vifSet[f] {
			missingFromVif = append(missingFromVif, f)
		}
	}

	var extraInVif []string
	for _, f := range vifFiles {
		if !composerSet[f] {
			extraInVif = append(extraInVif, f)
		}
	}

	// Compare content for autoloader files present in both.
	var contentMismatches []string
	for _, f := range vifFiles {
		if !composerSet[f] {
			continue
		}
		relToVendor := strings.TrimPrefix(f, "vendor/")
		composerPath := filepath.Join(composerVendor, relToVendor)
		vifPath := filepath.Join(vifVendor, relToVendor)

		if compatAutoloaderFiles["vendor/"+relToVendor] {
			diff := diffAutoloaderFile(t, f, composerPath, vifPath)
			if diff != "" {
				contentMismatches = append(contentMismatches, diff)
			}
		}
	}

	// Compare file content hashes for non-autoloader files present in both.
	var hashMismatches []string
	for _, f := range vifFiles {
		if !composerSet[f] {
			continue
		}
		if compatAutoloaderFiles[f] {
			continue
		}
		relToVendor := strings.TrimPrefix(f, "vendor/")
		composerPath := filepath.Join(composerVendor, relToVendor)
		vifPath := filepath.Join(vifVendor, relToVendor)

		ch, err1 := sha256File(composerPath)
		vh, err2 := sha256File(vifPath)
		if err1 != nil || err2 != nil {
			hashMismatches = append(hashMismatches, fmt.Sprintf("%s (read error: composer=%v vif=%v)", f, err1, err2))
			continue
		}
		if ch != vh {
			hashMismatches = append(hashMismatches, f)
		}
	}

	passed := len(missingFromVif) == 0 && len(contentMismatches) == 0

	return compatResult{
		fixture:           fixture,
		composerTotal:     len(composerFiles),
		vifTotal:          len(vifFiles),
		missingFiles:      missingFromVif,
		extraFiles:        extraInVif,
		contentMismatches: contentMismatches,
		hashMismatches:    hashMismatches,
		passed:            passed,
	}
}

// phpArrayPattern matches lines within a PHP return array(...) block.
// We extract key => value pairs for comparison.
var phpArrayEntryRe = regexp.MustCompile(`^\s+'([^'\\]*(?:\\.[^'\\]*)*)'\s+=>\s+(.+),?\s*$`)
var phpReturnArrayStartRe = regexp.MustCompile(`^\s*return\s+(?:\\?array\s*\(|\[)\s*$`)

// extractPHPArrayEntries parses key => value pairs from a PHP autoloader file.
// This is a best-effort line-by-line parser, not a full PHP parser.
// It handles nested arrays (like PSR-0 letter-indexed entries) by tracking
// brace/paren depth and skipping entries inside nested structures.
func extractPHPArrayEntries(content string) map[string]string {
	entries := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	depth := 0
	inReturn := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Track when we're inside a return array(...) or return [...] block.
		if phpReturnArrayStartRe.MatchString(trimmed) {
			inReturn = true
			depth = 1
			continue
		}
		if !inReturn {
			continue
		}

		// Parse entries at the current depth before applying this line's depth delta.
		if depth == 1 {
			m := phpArrayEntryRe.FindStringSubmatch(line)
			if m != nil {
				key := m[1]
				val := strings.TrimRight(strings.TrimSpace(m[2]), ",")
				// Skip top-level entries whose value is itself an array (e.g. PSR-0
				// letter buckets), we only compare flat key => value mappings.
				if !strings.HasPrefix(val, "array(") && !strings.HasPrefix(val, "[") {
					entries[key] = val
				}
			}
		}

		// Count depth changes from parens and brackets outside single-quoted strings.
		depth += phpNestingDelta(line)
		if depth <= 0 {
			inReturn = false
		}
	}
	return entries
}

func phpNestingDelta(line string) int {
	delta := 0
	inSingleQuote := false
	escaped := false
	for _, c := range line {
		if inSingleQuote {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '\'' {
				inSingleQuote = false
			}
			continue
		}
		if c == '\'' {
			inSingleQuote = true
			continue
		}
		switch c {
		case '(', '[':
			delta++
		case ')', ']':
			delta--
		}
	}
	return delta
}

// diffAutoloaderFile compares the PHP array entries in two autoloader files and
// returns a human-readable diff string (empty string if identical).
func diffAutoloaderFile(t *testing.T, relPath, composerPath, vifPath string) string {
	t.Helper()

	composerData, err := os.ReadFile(composerPath)
	if err != nil {
		return fmt.Sprintf("%s: read composer file: %v", relPath, err)
	}
	vifData, err := os.ReadFile(vifPath)
	if err != nil {
		return fmt.Sprintf("%s: read vif file: %v", relPath, err)
	}

	composerEntries := extractPHPArrayEntries(string(composerData))
	vifEntries := extractPHPArrayEntries(string(vifData))

	var diffs []string

	for k, cv := range composerEntries {
		if shouldSkipAutoloaderEntry(k, cv) {
			continue
		}
		vv, ok := vifEntries[k]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("  missing key %q (composer has: %s)", k, cv))
			continue
		}
		if shouldSkipAutoloaderEntry(k, vv) {
			continue
		}
		// Normalise path separators and trim __DIR__ prefix variants for comparison.
		if normalisePHPPath(cv) != normalisePHPPath(vv) {
			diffs = append(diffs, fmt.Sprintf("  key %q: composer=%s vif=%s", k, cv, vv))
		}
	}
	for k, vv := range vifEntries {
		if shouldSkipAutoloaderEntry(k, vv) {
			continue
		}
		if _, ok := composerEntries[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("  extra key %q (vif has: %s)", k, vv))
		}
	}

	if len(diffs) == 0 {
		return ""
	}
	sort.Strings(diffs)
	return fmt.Sprintf("%s:\n%s", relPath, strings.Join(diffs, "\n"))
}

// shouldSkipAutoloaderEntry filters known Phase 1 compat differences:
// - root package autoload paths using $baseDir
// - Composer\InstalledVersions classmap/autoload entries
func shouldSkipAutoloaderEntry(key, val string) bool {
	if strings.Contains(val, "$baseDir") {
		return true
	}
	return key == `Composer\\InstalledVersions` || strings.Contains(val, "InstalledVersions")
}

// normalisePHPPath strips the __DIR__ / $vendorDir prefix and OS path separator
// differences so that two logically equal paths compare equal.
func normalisePHPPath(s string) string {
	// Strip leading __DIR__ . '/..' . '/' or $vendorDir . '/' expressions.
	prefixes := []string{
		`__DIR__ . '/..' . '`,
		`$vendorDir . '`,
	}
	for _, p := range prefixes {
		if idx := strings.Index(s, p); idx != -1 {
			s = s[idx+len(p):]
			s = strings.TrimSuffix(s, "'")
		}
	}
	return filepath.ToSlash(strings.TrimSpace(s))
}

// TestCompat iterates over compat fixtures, runs both composer and vif, then
// compares the resulting vendor directories.
func TestCompat(t *testing.T) {
	if _, err := exec.LookPath("composer"); err != nil {
		t.Skip("composer not found in PATH")
	}

	fixtures := discoverCompatFixtures(t)

	cacheDir, err := getSharedCache(t)
	if err != nil {
		t.Fatalf("shared cache: %v", err)
	}

	for _, name := range fixtures {
		name := name
		t.Run(name, func(t *testing.T) {
			fixtureDir := filepath.Join(compatFixturesDir, name)

			// --- Composer side ---
			composerDir := compatTempDir(t, "composer-*")
			copyCompatFile(t, filepath.Join(fixtureDir, "composer.json"), filepath.Join(composerDir, "composer.json"))
			copyCompatFile(t, filepath.Join(fixtureDir, "composer.lock"), filepath.Join(composerDir, "composer.lock"))

			composerVendor, err := runComposerInstall(t, composerDir)
			if err != nil {
				t.Skipf("composer install failed (skipping fixture): %v", err)
			}

			// --- vif side ---
			vifDir := compatTempDir(t, "vif-*")
			copyCompatFile(t, filepath.Join(fixtureDir, "composer.json"), filepath.Join(vifDir, "composer.json"))
			copyCompatFile(t, filepath.Join(fixtureDir, "composer.lock"), filepath.Join(vifDir, "composer.lock"))

			vifVendor, err := runVifInstall(t, vifDir, cacheDir)
			if err != nil {
				t.Fatalf("vif install failed: %v", err)
			}

			// --- Compare ---
			result := compareVendorDirs(t, name, composerVendor, vifVendor)
			printCompatResult(t, result)

			if len(result.missingFiles) > 0 {
				t.Errorf("files present in composer vendor but missing from vif (%d):\n  %s",
					len(result.missingFiles), strings.Join(result.missingFiles, "\n  "))
			}
			if len(result.extraFiles) > 0 {
				t.Logf("files present in vif vendor but not in composer (%d):\n  %s",
					len(result.extraFiles), strings.Join(result.extraFiles, "\n  "))
			}
			if len(result.contentMismatches) > 0 {
				t.Errorf("autoloader content mismatches (%d):\n%s",
					len(result.contentMismatches), strings.Join(result.contentMismatches, "\n"))
			}
			if len(result.hashMismatches) > 0 {
				t.Logf("file content hash mismatches (%d):\n  %s",
					len(result.hashMismatches), strings.Join(result.hashMismatches, "\n  "))
			}
		})
	}
}

// TestCompatSummary runs all fixtures and prints a summary table.
func TestCompatSummary(t *testing.T) {
	if _, err := exec.LookPath("composer"); err != nil {
		t.Skip("composer not found in PATH")
	}

	fixtures := discoverCompatFixtures(t)

	cacheDir, err := getSharedCache(t)
	if err != nil {
		t.Fatalf("shared cache: %v", err)
	}

	results := make([]compatResult, 0, len(fixtures))

	for _, name := range fixtures {
		name := name
		fixtureDir := filepath.Join(compatFixturesDir, name)

		composerDir := compatTempDir(t, "composer-*")
		copyCompatFile(t, filepath.Join(fixtureDir, "composer.json"), filepath.Join(composerDir, "composer.json"))
		copyCompatFile(t, filepath.Join(fixtureDir, "composer.lock"), filepath.Join(composerDir, "composer.lock"))

		composerVendor, err := runComposerInstall(t, composerDir)
		if err != nil {
			t.Logf("[%s] SKIP: composer install failed: %v", name, err)
			results = append(results, compatResult{fixture: name, passed: false, missingFiles: []string{"(composer install failed)"}})
			continue
		}

		vifDir := compatTempDir(t, "vif-*")
		copyCompatFile(t, filepath.Join(fixtureDir, "composer.json"), filepath.Join(vifDir, "composer.json"))
		copyCompatFile(t, filepath.Join(fixtureDir, "composer.lock"), filepath.Join(vifDir, "composer.lock"))

		vifVendor, err := runVifInstall(t, vifDir, cacheDir)
		if err != nil {
			t.Logf("[%s] FAIL: vif install failed: %v", name, err)
			results = append(results, compatResult{fixture: name, passed: false, missingFiles: []string{"(vif install failed)"}})
			continue
		}

		r := compareVendorDirs(t, name, composerVendor, vifVendor)
		results = append(results, r)
	}

	// Print summary table.
	t.Log("")
	t.Log("=== Compatibility Summary ===")
	t.Logf("%-20s  %8s  %8s  %8s  %8s  %12s  %10s  %6s",
		"Fixture", "Composer", "Vif", "Missing", "Extra", "Mismatches", "HashDiff", "Result")
	t.Log(strings.Repeat("-", 92))

	passed := 0
	for _, r := range results {
		status := "FAIL"
		if r.passed {
			status = "PASS"
			passed++
		}
		t.Logf("%-20s  %8d  %8d  %8d  %8d  %12d  %10d  %6s",
			r.fixture,
			r.composerTotal,
			r.vifTotal,
			len(r.missingFiles),
			len(r.extraFiles),
			len(r.contentMismatches),
			len(r.hashMismatches),
			status,
		)
	}

	t.Log(strings.Repeat("-", 92))
	pct := 0
	if len(results) > 0 {
		pct = passed * 100 / len(results)
	}
	t.Logf("Compatibility: %d/%d fixtures passed (%d%%)", passed, len(results), pct)
}

// printCompatResult logs a per-fixture summary to the test log.
func printCompatResult(t *testing.T, r compatResult) {
	t.Helper()
	t.Logf("--- Compat summary for %s ---", r.fixture)
	t.Logf("  Composer vendor files : %d", r.composerTotal)
	t.Logf("  Vif vendor files      : %d", r.vifTotal)
	t.Logf("  Missing from vif      : %d", len(r.missingFiles))
	t.Logf("  Extra in vif          : %d", len(r.extraFiles))
	t.Logf("  Autoloader mismatches : %d", len(r.contentMismatches))
	t.Logf("  Hash mismatches       : %d", len(r.hashMismatches))
	if len(r.hashMismatches) > 0 {
		limit := len(r.hashMismatches)
		if limit > 20 {
			limit = 20
		}
		for _, f := range r.hashMismatches[:limit] {
			t.Logf("    %s", f)
		}
		if len(r.hashMismatches) > 20 {
			t.Logf("    ... and %d more", len(r.hashMismatches)-20)
		}
	}
	if r.passed {
		t.Logf("  Result                : PASS")
	} else {
		t.Logf("  Result                : FAIL")
	}
}

// copyCompatFile copies a file from src to dst, failing the test on error.
func copyCompatFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("copyCompatFile read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("copyCompatFile mkdir: %v", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("copyCompatFile write %s: %v", dst, err)
	}
}
