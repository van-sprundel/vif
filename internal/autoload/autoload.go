package autoload

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/van-sprundel/vif/internal/pkg"
)

// rootPrefix marks paths that are relative to the project root ($baseDir)
// rather than vendor/. The literal "../" matches the filesystem relationship.
const rootPrefix = "../"

// RootAutoload holds the root package's autoload configuration and name.
type RootAutoload struct {
	Name        string       // root package name (for file hash keys)
	Autoload    pkg.Autoload // from composer.json "autoload"
	AutoloadDev pkg.Autoload // from composer.json "autoload-dev"
}

// Generate creates all autoload PHP files in vendorDir from the given packages.
// The hash is used as the suffix for class names (typically the content-hash from composer.lock).
// If optimized is true, PSR-4/PSR-0 classes are scanned into the classmap (like composer dump-autoload -o).
// If root is non-nil, root package autoload entries are included with $baseDir paths.
func Generate(vendorDir string, packages []pkg.Package, contentHash string, optimized bool, root *RootAutoload) error {
	composerDir := filepath.Join(vendorDir, "composer")
	if err := os.MkdirAll(composerDir, 0o755); err != nil {
		return fmt.Errorf("autoload: mkdir: %w", err)
	}

	hash := contentHash
	if hash == "" {
		hash = "0000000000000000000000000000000000000000"
	}

	// Collect all autoload data from packages.
	psr4 := make(map[string][]string)   // namespace -> list of dirs (relative to vendor/)
	psr0 := make(map[string][]string)   // namespace -> list of dirs
	classmap := make(map[string]string) // FQCN -> relative file path
	var files []fileEntry               // deterministic-keyed file entries
	scanInputs := make([]packageScanInput, 0, len(packages))

	for _, p := range packages {
		pkgDir := p.Name // relative to vendor/
		pkgPath := filepath.Join(vendorDir, pkgDir)
		excludes := p.Autoload.ExcludeFromClassmap

		// PSR-4
		for ns, paths := range p.Autoload.PSR4 {
			for _, path := range paths {
				dir := filepath.Join(pkgDir, path)
				psr4[ns] = append(psr4[ns], dir)
			}
		}

		// PSR-0
		for ns, paths := range p.Autoload.PSR0 {
			for _, path := range paths {
				dir := filepath.Join(pkgDir, path)
				psr0[ns] = append(psr0[ns], dir)
			}
		}

		input := packageScanInput{
			name:     p.Name,
			pkgDir:   pkgDir,
			pkgPath:  pkgPath,
			classmap: append([]string(nil), p.Autoload.Classmap...),
			excludes: append([]string(nil), excludes...),
		}
		if optimized {
			input.psr4 = flattenAutoloadPaths(p.Autoload.PSR4)
			input.psr0 = flattenAutoloadPaths(p.Autoload.PSR0)
			input.psr4Namespaces = p.Autoload.PSR4
			input.psr0Namespaces = p.Autoload.PSR0
		}
		scanInputs = append(scanInputs, input)

		// Files
		for _, f := range p.Autoload.Files {
			key := fileHash(p.Name, f)
			files = append(files, fileEntry{
				key:  key,
				path: filepath.Join(pkgDir, f),
			})
		}
	}

	// Root package autoload entries use rootPrefix ("../") so generators
	// emit $baseDir instead of $vendorDir.
	if root != nil {
		addRootAutoload := func(al pkg.Autoload) {
			for ns, paths := range al.PSR4 {
				for _, path := range paths {
					psr4[ns] = append(psr4[ns], rootPrefix+path)
				}
			}
			for ns, paths := range al.PSR0 {
				for _, path := range paths {
					psr0[ns] = append(psr0[ns], rootPrefix+path)
				}
			}
			for _, f := range al.Files {
				key := fileHash(root.Name, f)
				files = append(files, fileEntry{
					key:  key,
					path: rootPrefix + f,
				})
			}
		}
		addRootAutoload(root.Autoload)
		addRootAutoload(root.AutoloadDev)

		// Root classmap and optimized PSR scanning.
		projectDir := filepath.Dir(vendorDir)
		for _, al := range []pkg.Autoload{root.Autoload, root.AutoloadDev} {
			input := packageScanInput{
				name:    root.Name,
				pkgDir:  rootPrefix,
				pkgPath: projectDir,
				classmap: append([]string(nil), al.Classmap...),
				excludes: append([]string(nil), al.ExcludeFromClassmap...),
			}
			if optimized {
				input.psr4 = flattenAutoloadPaths(al.PSR4)
				input.psr0 = flattenAutoloadPaths(al.PSR0)
				input.psr4Namespaces = al.PSR4
				input.psr0Namespaces = al.PSR0
			}
			if len(input.classmap) > 0 || len(input.psr4) > 0 || len(input.psr0) > 0 {
				scanInputs = append(scanInputs, input)
			}
		}
	}

	debugAutoload := os.Getenv("VIF_AUTOLOAD_DEBUG") != ""
	scannedClassmaps, scanStats, err := scanPackageClassmaps(scanInputs)
	if err != nil {
		return err
	}
	for _, scanned := range scannedClassmaps {
		for fqcn, path := range scanned {
			classmap[fqcn] = path
		}
	}
	if debugAutoload {
		logAutoloadScanStats(scanStats)
	}

	// Sort files for deterministic output.
	sort.Slice(files, func(i, j int) bool { return files[i].key < files[j].key })

	// Collect include-paths.
	var includePaths []string
	for _, p := range packages {
		for _, ip := range p.IncludePath {
			includePaths = append(includePaths, filepath.Join(p.Name, ip))
		}
	}

	// Generate each file.
	writers := map[string]string{
		"autoload_psr4.php":       generatePsr4(psr4),
		"autoload_namespaces.php": generateNamespaces(psr0),
		"autoload_classmap.php":   generateClassmap(classmap),
		"autoload_files.php":      generateFiles(files),
		"autoload_static.php":     generateStatic(hash, psr4, psr0, classmap, files),
		"autoload_real.php":       generateReal(hash, len(files) > 0),
		"ClassLoader.php":         classLoaderPHP,
		"LICENSE":                 composerLicense,
	}

	if len(includePaths) > 0 {
		writers["include_paths.php"] = generateIncludePaths(includePaths)
	}

	for name, content := range writers {
		if err := os.WriteFile(filepath.Join(composerDir, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("autoload: write %s: %w", name, err)
		}
	}

	// Write vendor/autoload.php (one level up from composer/).
	autoload := strings.ReplaceAll(autoloadPHP, "<HASH>", hash)
	if err := os.WriteFile(filepath.Join(vendorDir, "autoload.php"), []byte(autoload), 0o644); err != nil {
		return fmt.Errorf("autoload: write autoload.php: %w", err)
	}

	return nil
}

type fileEntry struct {
	key  string
	path string
}

type packageScanInput struct {
	name           string
	pkgDir         string
	pkgPath        string
	classmap       []string
	psr4           []string            // only populated in optimized mode
	psr0           []string            // only populated in optimized mode
	psr4Namespaces map[string][]string // namespace -> dirs, for filtering in optimized mode
	psr0Namespaces map[string][]string // namespace -> dirs, for filtering in optimized mode
	excludes       []string
}

type packageScanResult struct {
	index    int
	classmap map[string]string
	stats    packageScanStats
	err      error
}

type packageScanStats struct {
	name            string
	classmapEntries int
	psr4Entries     int
	psr0Entries     int
	files           int
	symbols         int
	duration        time.Duration
}

// fileHash returns the md5 hex of "packageName:filePath", matching Composer's behavior.
func fileHash(packageName, filePath string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(packageName+":"+filePath)))
}

// matchesPsr4 checks whether a class FQCN matches its file path according to
// PSR-4 naming rules. Returns true if the class should be included in the classmap.
func matchesPsr4(fqcn, filePath, pkgDir string, psr4 map[string][]string) bool {
	// Sort namespace prefixes by length (longest first) so the most specific match wins.
	prefixes := make([]string, 0, len(psr4))
	for ns := range psr4 {
		prefixes = append(prefixes, ns)
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })

	inPsr4Dir := false
	for _, nsPrefix := range prefixes {
		dirs := psr4[nsPrefix]
		for _, dir := range dirs {
			// Use trailing separator so "src/Server/" doesn't match "src/Server.php".
			psr4Dir := filepath.Join(pkgDir, dir) + string(filepath.Separator)
			if !strings.HasPrefix(filePath, psr4Dir) {
				continue
			}
			inPsr4Dir = true
			if !strings.HasPrefix(fqcn, nsPrefix) {
				// Namespace doesn't match, but another prefix might map to the same dir.
				continue
			}
			// Check that the relative class name maps to the relative file path.
			relClass := strings.TrimPrefix(fqcn, nsPrefix)
			expectedRelPath := strings.ReplaceAll(relClass, `\`, string(filepath.Separator)) + ".php"
			expectedPath := filepath.Join(psr4Dir, expectedRelPath)
			if filePath == expectedPath {
				return true
			}
		}
	}
	if inPsr4Dir {
		// File is in a PSR-4 directory but no namespace/path combo matched.
		return false
	}
	return true
}

// matchesPsr0 checks whether a class FQCN matches its file path according to
// PSR-0 naming rules. Returns true if the class should be included in the classmap.
func matchesPsr0(fqcn, filePath, pkgDir string, psr0 map[string][]string) bool {
	prefixes := make([]string, 0, len(psr0))
	for ns := range psr0 {
		prefixes = append(prefixes, ns)
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })

	inPsr0Dir := false
	for _, nsPrefix := range prefixes {
		dirs := psr0[nsPrefix]
		for _, dir := range dirs {
			psr0Dir := filepath.Join(pkgDir, dir) + string(filepath.Separator)
			if !strings.HasPrefix(filePath, psr0Dir) {
				continue
			}
			inPsr0Dir = true
			// For PSR-0, the prefix can be a namespace (with \) or a class prefix (with _).
			// Check if FQCN starts with the prefix.
			nsTrimmed := strings.TrimRight(nsPrefix, `\_`)
			if nsTrimmed != "" && !strings.HasPrefix(fqcn, nsTrimmed) {
				continue
			}
			// PSR-0 path: replace \ with /, then _ in class name with /.
			// For namespaced classes: only underscores after last \ become separators.
			classPath := fqcn
			if idx := strings.LastIndex(classPath, `\`); idx >= 0 {
				ns := strings.ReplaceAll(classPath[:idx], `\`, string(filepath.Separator))
				className := strings.ReplaceAll(classPath[idx+1:], "_", string(filepath.Separator))
				classPath = ns + string(filepath.Separator) + className
			} else {
				// No namespace — all underscores become separators.
				classPath = strings.ReplaceAll(classPath, "_", string(filepath.Separator))
			}
			expectedPath := filepath.Join(psr0Dir, classPath+".php")
			if filePath == expectedPath {
				return true
			}
		}
	}
	if inPsr0Dir {
		return false
	}
	return true
}

func flattenAutoloadPaths(m map[string][]string) []string {
	if len(m) == 0 {
		return nil
	}
	var paths []string
	for _, entries := range m {
		paths = append(paths, entries...)
	}
	return paths
}

func scanPackageClassmaps(inputs []packageScanInput) ([]map[string]string, []packageScanStats, error) {
	results := make([]map[string]string, len(inputs))
	stats := make([]packageScanStats, len(inputs))
	if len(inputs) == 0 {
		return results, stats, nil
	}
	debugAutoload := os.Getenv("VIF_AUTOLOAD_DEBUG") != ""
	completed := 0
	var completedFiles int
	var completedSymbols int
	var completedDuration time.Duration

	ch := make(chan packageScanResult, len(inputs))
	var wg sync.WaitGroup
	for i, input := range inputs {
		wg.Add(1)
		go func(index int, input packageScanInput) {
			defer wg.Done()
			scanned, stats, err := scanPackageClassmap(input)
			ch <- packageScanResult{
				index:    index,
				classmap: scanned,
				stats:    stats,
				err:      err,
			}
		}(i, input)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()

	for result := range ch {
		if result.err != nil {
			return nil, nil, result.err
		}
		results[result.index] = result.classmap
		stats[result.index] = result.stats
		completed++
		completedFiles += result.stats.files
		completedSymbols += result.stats.symbols
		completedDuration += result.stats.duration
		if debugAutoload && (result.stats.duration >= 2*time.Second || result.stats.files >= 1000 || completed%25 == 0) {
			fmt.Fprintf(os.Stderr, "autoload scan progress: completed=%d/%d package=%s files=%d symbols=%d classmap=%d psr4=%d psr0=%d package_time=%s cumulative_cpu=%s\n",
				completed, len(inputs), result.stats.name, result.stats.files, result.stats.symbols, result.stats.classmapEntries, result.stats.psr4Entries, result.stats.psr0Entries,
				result.stats.duration.Round(time.Millisecond), completedDuration.Round(time.Millisecond))
		}
	}

	return results, stats, nil
}

func logAutoloadScanStats(stats []packageScanStats) {
	total := packageScanStats{name: "TOTAL"}
	filtered := stats[:0]
	for _, stat := range stats {
		if stat.name == "" {
			continue
		}
		total.classmapEntries += stat.classmapEntries
		total.psr4Entries += stat.psr4Entries
		total.psr0Entries += stat.psr0Entries
		total.files += stat.files
		total.symbols += stat.symbols
		total.duration += stat.duration
		if stat.files > 0 {
			filtered = append(filtered, stat)
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].duration == filtered[j].duration {
			return filtered[i].files > filtered[j].files
		}
		return filtered[i].duration > filtered[j].duration
	})

	fmt.Fprintf(os.Stderr, "autoload scan total: packages=%d classmap_entries=%d psr4_entries=%d psr0_entries=%d files=%d symbols=%d cpu_time=%s\n",
		len(filtered), total.classmapEntries, total.psr4Entries, total.psr0Entries, total.files, total.symbols, total.duration.Round(time.Millisecond))
	limit := 15
	if len(filtered) < limit {
		limit = len(filtered)
	}
	for i := 0; i < limit; i++ {
		stat := filtered[i]
		fmt.Fprintf(os.Stderr, "autoload scan top[%d]: package=%s files=%d symbols=%d classmap=%d psr4=%d psr0=%d time=%s\n",
			i+1, stat.name, stat.files, stat.symbols, stat.classmapEntries, stat.psr4Entries, stat.psr0Entries, stat.duration.Round(time.Millisecond))
	}
}

func scanPackageClassmap(input packageScanInput) (map[string]string, packageScanStats, error) {
	classmap := make(map[string]string)
	stats := packageScanStats{
		name:            input.name,
		classmapEntries: len(input.classmap),
		psr4Entries:     len(input.psr4),
		psr0Entries:     len(input.psr0),
	}
	started := time.Now()

	scanAndAdd := func(label string, entries []string) error {
		if len(entries) == 0 {
			return nil
		}
		scanned, scanStats, err := scanClassmapWithStats(input.pkgPath, entries, input.excludes)
		if err != nil {
			return fmt.Errorf("autoload: %s scan %s: %w", label, input.name, err)
		}
		stats.files += scanStats.Files
		stats.symbols += scanStats.Symbols
		for fqcn, relPath := range scanned {
			classmap[fqcn] = filepath.Join(input.pkgDir, relPath)
		}
		return nil
	}

	if err := scanAndAdd("classmap", input.classmap); err != nil {
		return nil, packageScanStats{}, err
	}

	// PSR-4/PSR-0 scanning only in optimized mode.
	// For PSR-4, apply namespace-to-filepath filtering: only include classes
	// whose FQCN maps to the file they were found in per PSR-4 rules.
	if len(input.psr4) > 0 {
		scanned, scanStats, err := scanClassmapWithStats(input.pkgPath, input.psr4, input.excludes)
		if err != nil {
			return nil, packageScanStats{}, err
		}
		stats.files += scanStats.Files
		stats.symbols += scanStats.Symbols
		for fqcn, relPath := range scanned {
			fullPath := filepath.Join(input.pkgDir, relPath)
			if matchesPsr4(fqcn, fullPath, input.pkgDir, input.psr4Namespaces) {
				classmap[fqcn] = fullPath
			}
		}
	}
	// PSR-0 scanning with namespace-to-filepath filtering.
	if len(input.psr0) > 0 {
		scanned, scanStats, err := scanClassmapWithStats(input.pkgPath, input.psr0, input.excludes)
		if err != nil {
			return nil, packageScanStats{}, err
		}
		stats.files += scanStats.Files
		stats.symbols += scanStats.Symbols
		for fqcn, relPath := range scanned {
			fullPath := filepath.Join(input.pkgDir, relPath)
			if matchesPsr0(fqcn, fullPath, input.pkgDir, input.psr0Namespaces) {
				classmap[fqcn] = fullPath
			}
		}
	}
	stats.duration = time.Since(started)

	return classmap, stats, nil
}

// phpPathExpr returns the PHP expression for a path in return-array files.
// Root paths (prefixed with "../") use $baseDir, vendor paths use $vendorDir.
func phpPathExpr(path string) string {
	if strings.HasPrefix(path, rootPrefix) {
		return "$baseDir . '/" + strings.TrimPrefix(path, rootPrefix) + "'"
	}
	return "$vendorDir . '/" + path + "'"
}

// phpStaticPathExpr returns the PHP expression for a path in autoload_static.php.
// Root paths use __DIR__ . '/../..', vendor paths use __DIR__ . '/..'.
func phpStaticPathExpr(path string) string {
	if strings.HasPrefix(path, rootPrefix) {
		return "__DIR__ . '/../..' . '/" + strings.TrimPrefix(path, rootPrefix) + "'"
	}
	return "__DIR__ . '/..' . '/" + path + "'"
}

// --- Simple return-array files ---

func generateIncludePaths(paths []string) string {
	var b strings.Builder
	b.WriteString("<?php\n\n")
	b.WriteString("// include_paths.php @generated by vif\n\n")
	b.WriteString("$vendorDir = dirname(__DIR__);\n$baseDir = dirname($vendorDir);\n\n")
	b.WriteString("return array(\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "    $vendorDir . '/%s',\n", p)
	}
	b.WriteString(");\n")
	return b.String()
}

func generatePsr4(psr4 map[string][]string) string {
	var b strings.Builder
	keys := sortedKeys(psr4)
	for _, ns := range keys {
		dirs := psr4[ns]
		for _, dir := range dirs {
			fmt.Fprintf(&b, "    %s => array(%s),\n", phpString(ns), phpPathExpr(dir))
		}
	}
	return strings.Replace(autoloadPsr4PHP, "<PSR4_ENTRIES>", b.String(), 1)
}

func generateNamespaces(psr0 map[string][]string) string {
	var b strings.Builder
	keys := sortedKeys(psr0)
	for _, ns := range keys {
		dirs := psr0[ns]
		for _, dir := range dirs {
			fmt.Fprintf(&b, "    %s => array(%s),\n", phpString(ns), phpPathExpr(dir))
		}
	}
	return strings.Replace(autoloadNamespacesPHP, "<PSR0_ENTRIES>", b.String(), 1)
}

func generateClassmap(classmap map[string]string) string {
	var b strings.Builder
	keys := sortedMapKeys(classmap)
	for _, fqcn := range keys {
		path := classmap[fqcn]
		fmt.Fprintf(&b, "    %s => %s,\n", phpString(fqcn), phpPathExpr(path))
	}
	return strings.Replace(autoloadClassmapPHP, "<CLASSMAP_ENTRIES>", b.String(), 1)
}

func generateFiles(files []fileEntry) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "    '%s' => %s,\n", f.key, phpPathExpr(f.path))
	}
	return strings.Replace(autoloadFilesPHP, "<FILES_ENTRIES>", b.String(), 1)
}

// --- autoload_static.php ---

func generateStatic(hash string, psr4 map[string][]string, psr0 map[string][]string, classmap map[string]string, files []fileEntry) string {
	s := autoloadStaticPHP

	// Files
	if len(files) > 0 {
		var b strings.Builder
		b.WriteString("    public static $files = array(\n")
		for _, f := range files {
			fmt.Fprintf(&b, "        '%s' => %s,\n", f.key, phpStaticPathExpr(f.path))
		}
		b.WriteString("    );\n")
		s = strings.Replace(s, "<STATIC_FILES>", b.String(), 1)
	} else {
		s = strings.Replace(s, "<STATIC_FILES>\n", "", 1)
	}

	// PSR-4: prefixLengthsPsr4 and prefixDirsPsr4
	if len(psr4) > 0 {
		s = strings.Replace(s, "<STATIC_PREFIX_LENGTHS_PSR4>", buildPrefixLengths(psr4), 1)
		s = strings.Replace(s, "<STATIC_PREFIX_DIRS_PSR4>", buildPrefixDirs(psr4), 1)
	} else {
		s = strings.Replace(s, "<STATIC_PREFIX_LENGTHS_PSR4>\n", "", 1)
		s = strings.Replace(s, "<STATIC_PREFIX_DIRS_PSR4>\n", "", 1)
	}

	// PSR-0: prefixesPsr0
	if len(psr0) > 0 {
		s = strings.Replace(s, "<STATIC_PREFIXES_PSR0>", buildPrefixesPsr0(psr0), 1)
	} else {
		s = strings.Replace(s, "<STATIC_PREFIXES_PSR0>\n", "", 1)
	}

	// Classmap
	if len(classmap) > 0 {
		s = strings.Replace(s, "<STATIC_CLASSMAP>", buildStaticClassmap(classmap), 1)
	} else {
		s = strings.Replace(s, "<STATIC_CLASSMAP>\n", "", 1)
	}

	// Initializer body
	var init strings.Builder
	if len(psr4) > 0 {
		fmt.Fprintf(&init, "            $loader->prefixLengthsPsr4 = ComposerStaticInit%s::$prefixLengthsPsr4;\n", hash)
		fmt.Fprintf(&init, "            $loader->prefixDirsPsr4 = ComposerStaticInit%s::$prefixDirsPsr4;\n", hash)
	}
	if len(psr0) > 0 {
		fmt.Fprintf(&init, "            $loader->prefixesPsr0 = ComposerStaticInit%s::$prefixesPsr0;\n", hash)
	}
	if len(classmap) > 0 {
		fmt.Fprintf(&init, "            $loader->classMap = ComposerStaticInit%s::$classMap;\n", hash)
	}
	s = strings.Replace(s, "<INITIALIZER_BODY>", init.String(), 1)

	s = strings.ReplaceAll(s, "<HASH>", hash)
	return s
}

func buildPrefixLengths(psr4 map[string][]string) string {
	// Group by first character.
	byChar := make(map[byte]map[string]int)
	for ns := range psr4 {
		if len(ns) == 0 {
			continue
		}
		c := ns[0]
		if byChar[c] == nil {
			byChar[c] = make(map[string]int)
		}
		byChar[c][ns] = len(ns)
	}

	var chars []byte
	for c := range byChar {
		chars = append(chars, c)
	}
	sort.Slice(chars, func(i, j int) bool { return chars[i] < chars[j] })

	var b strings.Builder
	b.WriteString("    public static $prefixLengthsPsr4 = array(\n")
	for _, c := range chars {
		fmt.Fprintf(&b, "        '%c' => array(\n", c)
		namespaces := sortedMapKeys(byChar[c])
		for _, ns := range namespaces {
			fmt.Fprintf(&b, "            %s => %d,\n", phpString(ns), byChar[c][ns])
		}
		b.WriteString("        ),\n")
	}
	b.WriteString("    );\n")
	return b.String()
}

func buildPrefixDirs(psr4 map[string][]string) string {
	var b strings.Builder
	b.WriteString("    public static $prefixDirsPsr4 = array(\n")
	keys := sortedKeys(psr4)
	for _, ns := range keys {
		fmt.Fprintf(&b, "        %s => array(\n", phpString(ns))
		for i, dir := range psr4[ns] {
			fmt.Fprintf(&b, "            %d => %s,\n", i, phpStaticPathExpr(dir))
		}
		b.WriteString("        ),\n")
	}
	b.WriteString("    );\n")
	return b.String()
}

func buildPrefixesPsr0(psr0 map[string][]string) string {
	// Group by first character.
	byChar := make(map[byte]map[string][]string)
	for ns, dirs := range psr0 {
		if len(ns) == 0 {
			continue
		}
		c := ns[0]
		if byChar[c] == nil {
			byChar[c] = make(map[string][]string)
		}
		byChar[c][ns] = dirs
	}

	var chars []byte
	for c := range byChar {
		chars = append(chars, c)
	}
	sort.Slice(chars, func(i, j int) bool { return chars[i] < chars[j] })

	var b strings.Builder
	b.WriteString("    public static $prefixesPsr0 = array(\n")
	for _, c := range chars {
		fmt.Fprintf(&b, "        '%c' => array(\n", c)
		namespaces := sortedKeys(byChar[c])
		for _, ns := range namespaces {
			fmt.Fprintf(&b, "            %s => array(\n", phpString(ns))
			for i, dir := range byChar[c][ns] {
				fmt.Fprintf(&b, "                %d => %s,\n", i, phpStaticPathExpr(dir))
			}
			b.WriteString("            ),\n")
		}
		b.WriteString("        ),\n")
	}
	b.WriteString("    );\n")
	return b.String()
}

func buildStaticClassmap(classmap map[string]string) string {
	var b strings.Builder
	b.WriteString("    public static $classMap = array(\n")
	keys := sortedMapKeys(classmap)
	for _, fqcn := range keys {
		fmt.Fprintf(&b, "        %s => %s,\n", phpString(fqcn), phpStaticPathExpr(classmap[fqcn]))
	}
	b.WriteString("    );\n")
	return b.String()
}

// --- autoload_real.php ---

func generateReal(hash string, hasFiles bool) string {
	s := autoloadRealPHP
	if hasFiles {
		s = strings.Replace(s, "<FILES_REQUIRE>", filesRequireBlock, 1)
	} else {
		s = strings.Replace(s, "<FILES_REQUIRE>", "", 1)
	}
	s = strings.ReplaceAll(s, "<HASH>", hash)
	return s
}

// --- helpers ---

func phpString(s string) string {
	// PHP string with single quotes, escaping backslashes and single quotes.
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'"
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
