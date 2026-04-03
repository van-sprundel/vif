package version

import (
	"fmt"
	"strings"
)

// Constraint represents a parsed version constraint that can match against versions.
type Constraint struct {
	// OR groups — a version must match at least one group.
	// Each group is an AND of bounds.
	groups []constraintGroup
	text   string
}

type constraintGroup struct {
	bounds []bound
}

type boundOp int

const (
	opEq boundOp = iota
	opNeq
	opGt
	opGte
	opLt
	opLte
)

type bound struct {
	op      boundOp
	version Version
}

// ParseConstraint parses a Composer constraint string.
// Supports: exact, >, >=, <, <=, !=, ^, ~, *, spaces/commas for AND, || or | for OR.
func ParseConstraint(s string) (Constraint, error) {
	s = strings.TrimSpace(s)

	// Strip per-constraint stability flags like @RC, @beta, @alpha, @stable, @dev.
	// These override minimum-stability for a specific package but don't affect
	// the constraint bounds themselves.
	if at := strings.Index(s, "@"); at >= 0 {
		s = s[:at]
	}

	if s == "" {
		return Constraint{}, fmt.Errorf("constraint: empty string")
	}

	// Composer accepts both || and legacy single | as OR.
	s = strings.ReplaceAll(s, "||", "|")
	orParts := strings.Split(s, "|")
	groups := make([]constraintGroup, 0, len(orParts))

	for _, part := range orParts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		g, err := parseGroup(part)
		if err != nil {
			return Constraint{}, err
		}
		groups = append(groups, g)
	}

	if len(groups) == 0 {
		return Constraint{}, fmt.Errorf("constraint: no valid groups in %q", s)
	}

	return Constraint{groups: groups, text: renderConstraint(groups)}, nil
}

// parseGroup parses a single AND group (space or comma-separated bounds).
func parseGroup(s string) (constraintGroup, error) {
	// Replace commas with spaces for uniform splitting.
	s = strings.ReplaceAll(s, ",", " ")
	tokens := tokenize(s)

	var bounds []bound
	for _, tok := range tokens {
		b, err := parseSingleConstraint(tok)
		if err != nil {
			return constraintGroup{}, err
		}
		bounds = append(bounds, b...)
	}

	return constraintGroup{bounds: bounds}, nil
}

// tokenize splits an AND group into individual constraint tokens.
// Handles operators attached to versions like ">=1.0.0".
func tokenize(s string) []string {
	var tokens []string
	for _, part := range strings.Fields(s) {
		tokens = append(tokens, part)
	}
	return tokens
}

// parseSingleConstraint parses one constraint token and returns one or more bounds.
// ^ and ~ expand to two bounds (range).
func parseSingleConstraint(s string) ([]bound, error) {
	s = strings.TrimSpace(s)

	// Wildcard: *
	if s == "*" {
		// Match everything — use a bound that's always true.
		return []bound{{op: opGte, version: Version{Stability: Dev}}}, nil
	}

	// Caret: ^x.y.z
	if strings.HasPrefix(s, "^") {
		return parseCaret(s[1:])
	}

	// Tilde: ~x.y.z
	if strings.HasPrefix(s, "~") {
		return parseTilde(s[1:])
	}

	// Wildcard in version: x.y.* or x.*
	if strings.Contains(s, "*") {
		return parseWildcard(s)
	}

	// Operators: >=, <=, !=, >, <
	if strings.HasPrefix(s, ">=") {
		v, err := Parse(s[2:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: opGte, version: v}}, nil
	}
	if strings.HasPrefix(s, "<=") {
		v, err := Parse(s[2:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: opLte, version: v}}, nil
	}
	if strings.HasPrefix(s, "!=") {
		v, err := Parse(s[2:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: opNeq, version: v}}, nil
	}
	if strings.HasPrefix(s, ">") {
		v, err := Parse(s[1:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: opGt, version: v}}, nil
	}
	if strings.HasPrefix(s, "<") {
		v, err := Parse(s[1:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: opLt, version: v}}, nil
	}

	// Exact match (including dev-* branches).
	v, err := Parse(s)
	if err != nil {
		return nil, fmt.Errorf("constraint: %w", err)
	}
	return []bound{{op: opEq, version: v}}, nil
}

// parseCaret implements ^x.y.z:
// ^1.2.3 => >=1.2.3 <2.0.0
// ^0.3.0 => >=0.3.0 <0.4.0
// ^0.0.3 => >=0.0.3 <0.0.4
func parseCaret(s string) ([]bound, error) {
	v, err := Parse(s)
	if err != nil {
		return nil, fmt.Errorf("constraint: caret: %w", err)
	}

	upper := Version{Stability: Stable}
	if v.Major != 0 {
		upper.Major = v.Major + 1
	} else if v.Minor != 0 {
		upper.Minor = v.Minor + 1
	} else {
		upper.Patch = v.Patch + 1
	}

	// Lower bound uses the parsed version with stable floor.
	lower := v
	lower.Stability = v.Stability
	lower.StabilityNum = v.StabilityNum

	return []bound{
		{op: opGte, version: lower},
		{op: opLt, version: upper},
	}, nil
}

// parseTilde implements ~x.y.z:
// ~1.2.3 => >=1.2.3 <1.3.0
// ~1.2   => >=1.2.0 <2.0.0
func parseTilde(s string) ([]bound, error) {
	v, err := Parse(s)
	if err != nil {
		return nil, fmt.Errorf("constraint: tilde: %w", err)
	}

	upper := Version{Stability: Stable}
	// Count how many segments were in the original.
	segments := strings.Count(strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V"), ".") + 1
	if segments >= 3 {
		// ~1.2.3 => <1.3.0
		upper.Major = v.Major
		upper.Minor = v.Minor + 1
	} else {
		// ~1.2 => <2.0.0
		upper.Major = v.Major + 1
	}

	return []bound{
		{op: opGte, version: v},
		{op: opLt, version: upper},
	}, nil
}

// parseWildcard implements x.y.* and x.*:
// 1.0.* => >=1.0.0 <1.1.0
// 1.*   => >=1.0.0 <2.0.0
func parseWildcard(s string) ([]bound, error) {
	parts := strings.Split(s, ".")
	lower := Version{Stability: Stable}
	upper := Version{Stability: Stable}

	switch {
	case len(parts) == 3 && parts[2] == "*":
		// x.y.*
		major, err := parseIntPart(parts[0])
		if err != nil {
			return nil, fmt.Errorf("constraint: wildcard: %w", err)
		}
		minor, err := parseIntPart(parts[1])
		if err != nil {
			return nil, fmt.Errorf("constraint: wildcard: %w", err)
		}
		lower.Major = major
		lower.Minor = minor
		upper.Major = major
		upper.Minor = minor + 1
	case len(parts) == 2 && parts[1] == "*":
		// x.*
		major, err := parseIntPart(parts[0])
		if err != nil {
			return nil, fmt.Errorf("constraint: wildcard: %w", err)
		}
		lower.Major = major
		upper.Major = major + 1
	default:
		return nil, fmt.Errorf("constraint: unsupported wildcard %q", s)
	}

	return []bound{
		{op: opGte, version: lower},
		{op: opLt, version: upper},
	}, nil
}

func parseIntPart(s string) (int, error) {
	v, err := fmt.Sscanf(s, "%d", new(int))
	if err != nil || v == 0 {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n, nil
}

// Matches reports whether v satisfies the constraint.
func (c Constraint) Matches(v Version) bool {
	for _, g := range c.groups {
		if g.matches(v) {
			return true
		}
	}
	return false
}

func (g constraintGroup) matches(v Version) bool {
	for _, b := range g.bounds {
		if !b.matches(v) {
			return false
		}
	}
	return true
}

func (b bound) matches(v Version) bool {
	cmp := Compare(v, b.version)
	switch b.op {
	case opEq:
		// For dev branches, compare branch names.
		if b.version.Dev && v.Dev {
			return v.DevBranch == b.version.DevBranch
		}
		return cmp == 0
	case opNeq:
		return cmp != 0
	case opGt:
		return cmp > 0
	case opGte:
		return cmp >= 0
	case opLt:
		return cmp < 0
	case opLte:
		return cmp <= 0
	}
	return false
}

// String returns a normalized string representation of the constraint.
func (c Constraint) String() string {
	if c.text != "" {
		return c.text
	}
	return renderConstraint(c.groups)
}

func renderConstraint(groups []constraintGroup) string {
	parts := make([]string, len(groups))
	for i, g := range groups {
		parts[i] = g.String()
	}
	return strings.Join(parts, " || ")
}

func (g constraintGroup) String() string {
	parts := make([]string, len(g.bounds))
	for i, b := range g.bounds {
		parts[i] = b.String()
	}
	return strings.Join(parts, " ")
}

func (b bound) String() string {
	var prefix string
	switch b.op {
	case opEq:
		return b.version.String()
	case opNeq:
		prefix = "!="
	case opGt:
		prefix = ">"
	case opGte:
		prefix = ">="
	case opLt:
		prefix = "<"
	case opLte:
		prefix = "<="
	}
	return prefix + b.version.String()
}
