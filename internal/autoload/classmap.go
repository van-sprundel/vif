package autoload

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	namespaceRe = regexp.MustCompile(`(?m)^\s*namespace\s+([\w\\]+)`)
	classRe     = regexp.MustCompile(`(?m)^\s*(?:abstract\s+|final\s+)?(?:readonly\s+)?(?:class|interface|trait|enum)\s+(\w+)`)
	// heredocRe matches the start of a heredoc/nowdoc: <<<EOT or <<<'EOT'
	heredocStartRe = regexp.MustCompile(`<<<\s*'?(\w+)'?\s*$`)
)

// ScanClassmap scans directories and files for PHP class declarations.
// It returns a map of fully-qualified class name -> relative file path (relative to baseDir).
func ScanClassmap(baseDir string, entries []string) (map[string]string, error) {
	classmap := make(map[string]string)

	for _, entry := range entries {
		target := filepath.Join(baseDir, entry)

		info, err := os.Stat(target)
		if err != nil {
			// Skip missing entries silently — packages may declare
			// classmap entries that don't exist in all versions.
			continue
		}

		if info.IsDir() {
			if err := scanDir(baseDir, target, classmap); err != nil {
				return nil, err
			}
		} else {
			if err := scanFile(baseDir, target, classmap); err != nil {
				return nil, err
			}
		}
	}

	return classmap, nil
}

func scanDir(baseDir, dir string, classmap map[string]string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".php" && ext != ".inc" {
			return nil
		}
		return scanFile(baseDir, path, classmap)
	})
}

func scanFile(baseDir, path string, classmap map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var namespace string
	var heredocEnd string // non-empty when inside a heredoc/nowdoc block
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		// Track heredoc/nowdoc blocks to skip class declarations inside strings.
		if heredocEnd != "" {
			if strings.TrimSpace(line) == heredocEnd || strings.TrimSpace(line) == heredocEnd+";" {
				heredocEnd = ""
			}
			continue
		}
		if m := heredocStartRe.FindStringSubmatch(line); m != nil {
			heredocEnd = m[1]
			continue
		}

		if m := namespaceRe.FindStringSubmatch(line); m != nil {
			namespace = m[1]
		}

		if m := classRe.FindStringSubmatch(line); m != nil {
			className := m[1]
			var fqcn string
			if namespace != "" {
				fqcn = namespace + "\\" + className
			} else {
				fqcn = className
			}

			rel, err := filepath.Rel(baseDir, path)
			if err != nil {
				return err
			}
			classmap[fqcn] = rel
		}
	}

	return scanner.Err()
}
