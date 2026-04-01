package autoload

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/van-sprundel/vif/internal/pkg"
)

// Generate creates all autoload PHP files in vendorDir from the given packages.
// The hash is used as the suffix for class names (typically the content-hash from composer.lock).
func Generate(vendorDir string, packages []pkg.Package, contentHash string) error {
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

	for _, p := range packages {
		pkgDir := p.Name // relative to vendor/

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

		// Classmap — always compute pkgPath so PSR-4/PSR-0 scanning can use it too.
		pkgPath := filepath.Join(vendorDir, pkgDir)
		excludes := p.Autoload.ExcludeFromClassmap

		if len(p.Autoload.Classmap) > 0 {
			scanned, err := ScanClassmap(pkgPath, p.Autoload.Classmap, excludes)
			if err != nil {
				return fmt.Errorf("autoload: classmap scan %s: %w", p.Name, err)
			}
			for fqcn, relPath := range scanned {
				classmap[fqcn] = filepath.Join(pkgDir, relPath)
			}
		}

		// PSR-4: scan each mapped directory for class declarations.
		for _, paths := range p.Autoload.PSR4 {
			for _, path := range paths {
				scanned, err := ScanClassmap(pkgPath, []string{path}, excludes)
				if err != nil {
					return fmt.Errorf("autoload: psr-4 classmap scan %s: %w", p.Name, err)
				}
				for fqcn, relPath := range scanned {
					classmap[fqcn] = filepath.Join(pkgDir, relPath)
				}
			}
		}

		// PSR-0: scan each mapped directory for class declarations.
		for _, paths := range p.Autoload.PSR0 {
			for _, path := range paths {
				scanned, err := ScanClassmap(pkgPath, []string{path}, excludes)
				if err != nil {
					return fmt.Errorf("autoload: psr-0 classmap scan %s: %w", p.Name, err)
				}
				for fqcn, relPath := range scanned {
					classmap[fqcn] = filepath.Join(pkgDir, relPath)
				}
			}
		}

		// Files
		for _, f := range p.Autoload.Files {
			key := fileHash(p.Name, f)
			files = append(files, fileEntry{
				key:  key,
				path: filepath.Join(pkgDir, f),
			})
		}
	}

	// Sort files for deterministic output.
	sort.Slice(files, func(i, j int) bool { return files[i].key < files[j].key })

	// Generate each file.
	writers := map[string]string{
		"autoload_psr4.php":       generatePsr4(psr4),
		"autoload_namespaces.php": generateNamespaces(psr0),
		"autoload_classmap.php":   generateClassmap(classmap),
		"autoload_files.php":      generateFiles(files),
		"autoload_static.php":     generateStatic(hash, psr4, psr0, classmap, files),
		"autoload_real.php":       generateReal(hash, len(files) > 0),
		"ClassLoader.php":         classLoaderPHP,
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

// fileHash returns the md5 hex of "packageName:filePath", matching Composer's behavior.
func fileHash(packageName, filePath string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(packageName+":"+filePath)))
}

// --- Simple return-array files ---

func generatePsr4(psr4 map[string][]string) string {
	var b strings.Builder
	keys := sortedKeys(psr4)
	for _, ns := range keys {
		dirs := psr4[ns]
		for _, dir := range dirs {
			fmt.Fprintf(&b, "    %s => array($vendorDir . '/%s'),\n", phpString(ns), dir)
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
			fmt.Fprintf(&b, "    %s => array($vendorDir . '/%s'),\n", phpString(ns), dir)
		}
	}
	return strings.Replace(autoloadNamespacesPHP, "<PSR0_ENTRIES>", b.String(), 1)
}

func generateClassmap(classmap map[string]string) string {
	var b strings.Builder
	keys := sortedMapKeys(classmap)
	for _, fqcn := range keys {
		path := classmap[fqcn]
		fmt.Fprintf(&b, "    %s => $vendorDir . '/%s',\n", phpString(fqcn), path)
	}
	return strings.Replace(autoloadClassmapPHP, "<CLASSMAP_ENTRIES>", b.String(), 1)
}

func generateFiles(files []fileEntry) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "    '%s' => $vendorDir . '/%s',\n", f.key, f.path)
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
			fmt.Fprintf(&b, "        '%s' => __DIR__ . '/..' . '/%s',\n", f.key, f.path)
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
			fmt.Fprintf(&b, "            %d => __DIR__ . '/..' . '/%s',\n", i, dir)
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
				fmt.Fprintf(&b, "                %d => __DIR__ . '/..' . '/%s',\n", i, dir)
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
		fmt.Fprintf(&b, "        %s => __DIR__ . '/..' . '/%s',\n", phpString(fqcn), classmap[fqcn])
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
