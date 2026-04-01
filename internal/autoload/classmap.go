package autoload

import (
	"os"
	"path/filepath"
	"strings"
)

// ScanClassmap scans directories and files for PHP class declarations.
// It returns a map of fully-qualified class name -> relative file path (relative to baseDir).
// excludes is an optional list of path patterns (e.g. "/Tests/") — any file whose
// relative path (with a leading "/") contains the trimmed pattern is skipped.
func ScanClassmap(baseDir string, entries []string, excludes []string) (map[string]string, error) {
	classmap, _, err := scanClassmapWithStats(baseDir, entries, excludes)
	return classmap, err
}

type classmapScanStats struct {
	Entries int
	Files   int
	Symbols int
}

func scanClassmapWithStats(baseDir string, entries []string, excludes []string) (map[string]string, classmapScanStats, error) {
	classmap := make(map[string]string)
	stats := classmapScanStats{Entries: len(entries)}

	for _, entry := range entries {
		target := filepath.Join(baseDir, entry)

		info, err := os.Stat(target)
		if err != nil {
			// Skip missing entries silently — packages may declare
			// classmap entries that don't exist in all versions.
			continue
		}

		if info.IsDir() {
			if err := scanDir(baseDir, target, excludes, classmap, &stats); err != nil {
				return nil, classmapScanStats{}, err
			}
		} else {
			if isExcluded(baseDir, target, excludes) {
				continue
			}
			if err := scanFile(baseDir, target, classmap, &stats); err != nil {
				return nil, classmapScanStats{}, err
			}
		}
	}

	return classmap, stats, nil
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

func scanDir(baseDir, dir string, excludes []string, classmap map[string]string, stats *classmapScanStats) error {
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
		return scanFile(baseDir, path, classmap, stats)
	})
}

func scanFile(baseDir, path string, classmap map[string]string, stats *classmapScanStats) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if stats != nil {
		stats.Files++
	}

	symbols := findPHPDeclarations(string(data))
	if len(symbols) == 0 {
		return nil
	}
	if stats != nil {
		stats.Symbols += len(symbols)
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
	src                    []byte
	pos                    int
	namespace              string
	bracketedNamespaceEnds []int
	prevSignificant        phpTokenKind
	declarations           []string
}

func findPHPDeclarations(src string) []string {
	p := phpParser{src: []byte(src)}
	for {
		tok := p.nextToken()
		if tok.kind == phpTokenEOF {
			return p.declarations
		}

		switch tok.kind {
		case phpTokenLBrace:
			if len(p.bracketedNamespaceEnds) > 0 {
				last := len(p.bracketedNamespaceEnds) - 1
				p.bracketedNamespaceEnds[last]++
			}
		case phpTokenRBrace:
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
		case phpTokenNamespace:
			p.parseNamespace()
		case phpTokenClass, phpTokenInterface, phpTokenTrait, phpTokenEnum:
			p.parseDeclaration(tok.kind)
		}

		if tok.significant {
			p.prevSignificant = tok.kind
		}
	}
}

func (p *phpParser) parseNamespace() {
	var parts []string
	for {
		tok := p.nextToken()
		if tok.kind == phpTokenEOF {
			break
		}
		switch tok.kind {
		case phpTokenLBrace:
			p.namespace = strings.Join(parts, "")
			p.bracketedNamespaceEnds = append(p.bracketedNamespaceEnds, 1)
			return
		case phpTokenSemicolon:
			p.namespace = strings.Join(parts, "")
			p.bracketedNamespaceEnds = nil
			return
		default:
			if tok.kind == phpTokenBackslash {
				parts = append(parts, `\`)
			}
			if tok.kind == phpTokenIdentifier {
				parts = append(parts, tok.text)
			}
		}
	}
}

func (p *phpParser) parseDeclaration(kind phpTokenKind) {
	if kind == phpTokenClass && p.prevSignificant == phpTokenNew {
		return
	}

	name := p.nextIdentifier()
	if name == "" {
		return
	}

	if p.namespace != "" {
		name = p.namespace + `\` + name
	}
	p.declarations = append(p.declarations, name)
}

func (p *phpParser) nextIdentifier() string {
	for {
		tok := p.nextToken()
		if tok.kind == phpTokenEOF {
			return ""
		}
		if tok.kind == phpTokenIdentifier {
			return tok.text
		}
		switch tok.kind {
		case phpTokenLBrace, phpTokenRBrace, phpTokenLParen, phpTokenRParen, phpTokenSemicolon:
			return ""
		}
	}
}

func (p *phpParser) nextToken() phpToken {
	for p.pos < len(p.src) {
		r := p.src[p.pos]

		if isSpace(r) {
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
			return classifyIdentifier(p.src[start:p.pos])
		}

		p.pos++
		switch r {
		case '\\':
			return phpToken{kind: phpTokenBackslash}
		case '{':
			return phpToken{kind: phpTokenLBrace}
		case '}':
			return phpToken{kind: phpTokenRBrace}
		case '(':
			return phpToken{kind: phpTokenLParen}
		case ')':
			return phpToken{kind: phpTokenRParen}
		case ';':
			return phpToken{kind: phpTokenSemicolon}
		case ':', '=', ',', '[', ']':
			return phpToken{kind: phpTokenOther}
		default:
			return phpToken{kind: phpTokenOther, significant: true}
		}
	}

	return phpToken{kind: phpTokenEOF}
}

func (p *phpParser) match(s string) bool {
	if p.pos+len(s) > len(p.src) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if p.src[p.pos+i] != s[i] {
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

func (p *phpParser) skipQuoted(quote byte) {
	p.pos++
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '\\':
			p.pos++
			if p.pos < len(p.src) {
				p.pos++
			}
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
	for p.pos < len(p.src) && isInlineSpace(p.src[p.pos]) {
		p.pos++
	}

	quote := byte(0)
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
		line := trimASCIISpace(p.src[lineStart:p.pos])
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

type phpTokenKind uint8

const (
	phpTokenEOF phpTokenKind = iota
	phpTokenIdentifier
	phpTokenNamespace
	phpTokenClass
	phpTokenInterface
	phpTokenTrait
	phpTokenEnum
	phpTokenNew
	phpTokenBackslash
	phpTokenLBrace
	phpTokenRBrace
	phpTokenLParen
	phpTokenRParen
	phpTokenSemicolon
	phpTokenOther
)

type phpToken struct {
	kind        phpTokenKind
	text        string
	significant bool
}

func classifyIdentifier(raw []byte) phpToken {
	switch {
	case equalFoldASCII(raw, "namespace"):
		return phpToken{kind: phpTokenNamespace, significant: true}
	case equalFoldASCII(raw, "class"):
		return phpToken{kind: phpTokenClass, significant: true}
	case equalFoldASCII(raw, "interface"):
		return phpToken{kind: phpTokenInterface, significant: true}
	case equalFoldASCII(raw, "trait"):
		return phpToken{kind: phpTokenTrait, significant: true}
	case equalFoldASCII(raw, "enum"):
		return phpToken{kind: phpTokenEnum, significant: true}
	case equalFoldASCII(raw, "new"):
		return phpToken{kind: phpTokenNew, significant: true}
	default:
		return phpToken{kind: phpTokenIdentifier, text: string(raw), significant: true}
	}
}

func equalFoldASCII(got []byte, want string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		b := got[i]
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if b != want[i] {
			return false
		}
	}
	return true
}

func isIdentifierStart(b byte) bool {
	return b == '_' || ('A' <= b && b <= 'Z') || ('a' <= b && b <= 'z') || b >= 0x80
}

func isIdentifierPart(b byte) bool {
	return isIdentifierStart(b) || ('0' <= b && b <= '9')
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func isInlineSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r'
}

func trimASCIISpace(b []byte) string {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return string(b[start:end])
}
