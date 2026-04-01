package autoload

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// ScanClassmap scans directories and files for PHP class declarations.
// It returns a map of fully-qualified class name -> relative file path (relative to baseDir).
// excludes is an optional list of path patterns (e.g. "/Tests/") — any file whose
// relative path (with a leading "/") contains the trimmed pattern is skipped.
func ScanClassmap(baseDir string, entries []string, excludes []string) (map[string]string, error) {
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
			if err := scanDir(baseDir, target, excludes, classmap); err != nil {
				return nil, err
			}
		} else {
			if isExcluded(baseDir, target, excludes) {
				continue
			}
			if err := scanFile(baseDir, target, classmap); err != nil {
				return nil, err
			}
		}
	}

	return classmap, nil
}

// isExcluded reports whether path should be skipped based on the exclude patterns.
// The relative path from baseDir is normalized to forward slashes and prefixed with "/"
// before checking if any pattern (trimmed of slashes and prefixed with "/") is a substring.
func isExcluded(baseDir, path string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return false
	}
	// Normalize to forward slashes and prepend "/" for prefix-safe matching.
	normalized := "/" + strings.ReplaceAll(rel, string(filepath.Separator), "/")
	for _, pattern := range excludes {
		needle := "/" + strings.Trim(pattern, "/")
		if strings.Contains(normalized, needle) {
			return true
		}
	}
	return false
}

func scanDir(baseDir, dir string, excludes []string, classmap map[string]string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".php" && ext != ".inc" && ext != ".hh" {
			return nil
		}
		if isExcluded(baseDir, path, excludes) {
			return nil
		}
		return scanFile(baseDir, path, classmap)
	})
}

func scanFile(baseDir, path string, classmap map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	symbols := findPHPDeclarations(string(data))
	if len(symbols) == 0 {
		return nil
	}

	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return err
	}
	for _, fqcn := range symbols {
		classmap[fqcn] = rel
	}

	return nil
}

type phpParser struct {
	src                    []rune
	pos                    int
	namespace              string
	bracketedNamespaceEnds []int
	prevSignificant        string
	declarations           []string
}

func findPHPDeclarations(src string) []string {
	p := phpParser{src: []rune(src)}
	for {
		tok, ok := p.nextToken()
		if !ok {
			return p.declarations
		}

		switch tok {
		case "{":
			for len(p.bracketedNamespaceEnds) > 0 {
				last := len(p.bracketedNamespaceEnds) - 1
				p.bracketedNamespaceEnds[last]++
				if last == 0 {
					break
				}
			}
		case "}":
			if len(p.bracketedNamespaceEnds) > 0 {
				last := len(p.bracketedNamespaceEnds) - 1
				p.bracketedNamespaceEnds[last]--
				if p.bracketedNamespaceEnds[last] == 0 {
					p.bracketedNamespaceEnds = p.bracketedNamespaceEnds[:last]
					if len(p.bracketedNamespaceEnds) == 0 {
						p.namespace = ""
					}
				}
			}
		case "namespace":
			p.parseNamespace()
		case "class", "interface", "trait", "enum":
			p.parseDeclaration(tok)
		}

		if isSignificantToken(tok) {
			p.prevSignificant = tok
		}
	}
}

func (p *phpParser) parseNamespace() {
	var parts []string
	for {
		tok, ok := p.nextToken()
		if !ok {
			break
		}
		switch tok {
		case "{":
			p.namespace = strings.Join(parts, "")
			p.bracketedNamespaceEnds = append(p.bracketedNamespaceEnds, 1)
			return
		case ";":
			p.namespace = strings.Join(parts, "")
			p.bracketedNamespaceEnds = nil
			return
		default:
			if tok == `\` || isIdentifier(tok) {
				parts = append(parts, tok)
			}
		}
	}
}

func (p *phpParser) parseDeclaration(kind string) {
	if kind == "class" && p.prevSignificant == "new" {
		return
	}

	name, ok := p.nextIdentifier()
	if !ok {
		return
	}

	if p.namespace != "" {
		name = p.namespace + `\` + name
	}
	p.declarations = append(p.declarations, name)
}

func (p *phpParser) nextIdentifier() (string, bool) {
	for {
		tok, ok := p.nextToken()
		if !ok {
			return "", false
		}
		if isIdentifier(tok) {
			return tok, true
		}
		switch tok {
		case "{", "}", "(", ")", ";":
			return "", false
		}
	}
}

func (p *phpParser) nextToken() (string, bool) {
	for p.pos < len(p.src) {
		r := p.src[p.pos]

		if unicode.IsSpace(r) {
			p.pos++
			continue
		}

		if r == '<' && p.match("<?") {
			p.pos += 2
			if p.match("php") {
				p.pos += 3
			}
			continue
		}

		if p.match("//") || p.match("#") {
			p.skipLineComment()
			continue
		}
		if p.match("/*") {
			p.skipBlockComment()
			continue
		}
		if p.match("<<<") {
			p.skipHeredoc()
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			p.skipQuoted(r)
			continue
		}

		if isIdentifierStart(r) {
			start := p.pos
			p.pos++
			for p.pos < len(p.src) && isIdentifierPart(p.src[p.pos]) {
				p.pos++
			}
			return string(p.src[start:p.pos]), true
		}

		p.pos++
		return string(r), true
	}

	return "", false
}

func (p *phpParser) match(s string) bool {
	if p.pos+len(s) > len(p.src) {
		return false
	}
	for i, want := range s {
		if p.src[p.pos+i] != want {
			return false
		}
	}
	return true
}

func (p *phpParser) skipLineComment() {
	for p.pos < len(p.src) && p.src[p.pos] != '\n' {
		p.pos++
	}
}

func (p *phpParser) skipBlockComment() {
	p.pos += 2
	for p.pos < len(p.src) {
		if p.match("*/") {
			p.pos += 2
			return
		}
		p.pos++
	}
}

func (p *phpParser) skipQuoted(quote rune) {
	p.pos++
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
		case quote:
			p.pos++
			return
		default:
			p.pos++
		}
	}
}

func (p *phpParser) skipHeredoc() {
	p.pos += 3
	for p.pos < len(p.src) && unicode.IsSpace(p.src[p.pos]) && p.src[p.pos] != '\n' {
		p.pos++
	}

	quote := rune(0)
	if p.pos < len(p.src) && (p.src[p.pos] == '\'' || p.src[p.pos] == '"') {
		quote = p.src[p.pos]
		p.pos++
	}

	start := p.pos
	for p.pos < len(p.src) && isIdentifierPart(p.src[p.pos]) {
		p.pos++
	}
	label := string(p.src[start:p.pos])
	if label == "" {
		return
	}

	if quote != 0 && p.pos < len(p.src) && p.src[p.pos] == quote {
		p.pos++
	}

	for p.pos < len(p.src) && p.src[p.pos] != '\n' {
		p.pos++
	}
	if p.pos < len(p.src) {
		p.pos++
	}

	for p.pos < len(p.src) {
		lineStart := p.pos
		for p.pos < len(p.src) && p.src[p.pos] != '\n' {
			p.pos++
		}
		line := strings.TrimSpace(string(p.src[lineStart:p.pos]))
		if line == label || line == label+";" {
			if p.pos < len(p.src) {
				p.pos++
			}
			return
		}
		if p.pos < len(p.src) {
			p.pos++
		}
	}
}

func isIdentifierStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentifierPart(r rune) bool {
	return isIdentifierStart(r) || unicode.IsDigit(r)
}

func isIdentifier(tok string) bool {
	if tok == "" {
		return false
	}
	return slices.IndexFunc([]rune(tok), func(r rune) bool {
		return !isIdentifierPart(r)
	}) == -1
}

func isSignificantToken(tok string) bool {
	switch tok {
	case "", `\`, "{", "}", "(", ")", ";", ",", ":", "=":
		return false
	default:
		return true
	}
}
