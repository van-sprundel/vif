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

	// StabilityOverride is the per-constraint stability flag (e.g., @dev, @alpha).
	// -1 means no override (use minimum-stability); otherwise it's a Stability value.
	StabilityOverride int
}

type constraintGroup struct {
	bounds []bound
}

type boundOp int

const (
	OpEq boundOp = iota
	OpNeq
	OpGt
	OpGte
	OpLt
	OpLte
)

type bound struct {
	op      boundOp
	version Version
}

// BoundOp is the exported bound operator type.
type BoundOp = boundOp

// ExportedBound is an exported, immutable representation of a bound.
type ExportedBound struct {
	Op  BoundOp
	Ver Version
}

// ExportedGroup is an exported, immutable representation of an AND-group.
type ExportedGroup struct {
	Bounds []ExportedBound
}

// NoStabilityOverride indicates no per-constraint stability flag was specified.
const NoStabilityOverride = -1

// ParseConstraint parses a Composer constraint string.
// Supports: exact, >, >=, <, <=, !=, ^, ~, *, spaces/commas for AND, || or | for OR.
func ParseConstraint(s string) (Constraint, error) {
	s = strings.TrimSpace(s)

	// Extract per-constraint stability flags like @RC, @beta, @alpha, @stable, @dev.
	// These override minimum-stability for a specific package but don't affect
	// the constraint bounds themselves.
	stabilityOverride := NoStabilityOverride
	if at := strings.Index(s, "@"); at >= 0 {
		flag := strings.ToLower(s[at+1:])
		switch flag {
		case "dev":
			stabilityOverride = int(Dev)
		case "alpha":
			stabilityOverride = int(Alpha)
		case "beta":
			stabilityOverride = int(Beta)
		case "rc":
			stabilityOverride = int(RC)
		case "stable":
			stabilityOverride = int(Stable)
		}
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

	return Constraint{groups: groups, text: renderConstraint(groups), StabilityOverride: stabilityOverride}, nil
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
		return []bound{{op: OpGte, version: Version{Stability: Dev}}}, nil
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
		if !versionHasStabilitySuffix(s[2:]) {
			v.Stability = Dev
			v.StabilityNum = 0
		}
		return []bound{{op: OpGte, version: v}}, nil
	}
	if strings.HasPrefix(s, "<=") {
		v, err := Parse(s[2:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: OpLte, version: v}}, nil
	}
	if strings.HasPrefix(s, "!=") {
		v, err := Parse(s[2:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: OpNeq, version: v}}, nil
	}
	if strings.HasPrefix(s, ">") {
		v, err := Parse(s[1:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		return []bound{{op: OpGt, version: v}}, nil
	}
	if strings.HasPrefix(s, "<") {
		v, err := Parse(s[1:])
		if err != nil {
			return nil, fmt.Errorf("constraint: %w", err)
		}
		if !versionHasStabilitySuffix(s[1:]) {
			v.Stability = Dev
			v.StabilityNum = 0
		}
		return []bound{{op: OpLt, version: v}}, nil
	}

	// Exact match (including dev-* branches).
	v, err := Parse(s)
	if err != nil {
		return nil, fmt.Errorf("constraint: %w", err)
	}
	return []bound{{op: OpEq, version: v}}, nil
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

	segments := strings.Count(strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V"), ".") + 1

	upper := Version{Stability: Dev}
	if v.Major != 0 {
		upper.Major = v.Major + 1
	} else if segments == 1 {
		upper.Major = 1
	} else if v.Minor != 0 {
		upper.Minor = v.Minor + 1
	} else if segments == 2 {
		upper.Minor = 1
	} else {
		upper.Patch = v.Patch + 1
	}

	lower := v
	if !versionHasStabilitySuffix(s) {
		lower.Stability = Dev
		lower.StabilityNum = 0
	}

	return []bound{
		{op: OpGte, version: lower},
		{op: OpLt, version: upper},
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

	upper := Version{Stability: Dev}
	segments := strings.Count(strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V"), ".") + 1
	if segments >= 3 {
		upper.Major = v.Major
		upper.Minor = v.Minor + 1
	} else {
		upper.Major = v.Major + 1
	}

	lower := v
	if !versionHasStabilitySuffix(s) {
		lower.Stability = Dev
		lower.StabilityNum = 0
	}

	return []bound{
		{op: OpGte, version: lower},
		{op: OpLt, version: upper},
	}, nil
}

// parseWildcard implements x.y.* and x.*:
// 1.0.* => >=1.0.0 <1.1.0
// 1.*   => >=1.0.0 <2.0.0
func parseWildcard(s string) ([]bound, error) {
	parts := strings.Split(s, ".")
	lower := Version{Stability: Dev}
	upper := Version{Stability: Dev}

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
		{op: OpGte, version: lower},
		{op: OpLt, version: upper},
	}, nil
}

func versionHasStabilitySuffix(s string) bool {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	return m[5] != ""
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

// EffectiveStability returns the stability to use for filtering candidates.
// Priority: explicit @flag > implicit from dev-branch > fallback minimum-stability.
func (c Constraint) EffectiveStability(minimumStability Stability) Stability {
	// Explicit override takes precedence.
	if c.StabilityOverride != NoStabilityOverride {
		return Stability(c.StabilityOverride)
	}

	// Check for implicit dev stability from dev-* branch constraints.
	for _, g := range c.groups {
		for _, b := range g.bounds {
			if b.op == OpEq && b.version.Dev {
				return Dev
			}
		}
	}

	return minimumStability
}

// LowerBound returns the highest >= or > lower bound across all OR groups.
// Returns (Version, ">="|">", true) if found, or a zero value and false.
func (c Constraint) LowerBound() (Version, string, bool) {
	var best *bound
	for _, g := range c.groups {
		for i := range g.bounds {
			b := &g.bounds[i]
			if b.op != OpGte && b.op != OpGt {
				continue
			}
			if best == nil {
				best = b
				continue
			}
			cmp := Compare(b.version, best.version)
			if cmp > 0 || (cmp == 0 && b.op == OpGte && best.op == OpGt) {
				best = b
			}
		}
	}
	if best == nil {
		return Version{}, "", false
	}
	op := ">="
	if best.op == OpGt {
		op = ">"
	}
	return best.version, op, true
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
	case OpEq:
		// For dev branches, compare branch names.
		if b.version.Dev && v.Dev {
			return v.DevBranch == b.version.DevBranch
		}
		return cmp == 0
	case OpNeq:
		return cmp != 0
	case OpGt:
		return cmp > 0
	case OpGte:
		return cmp >= 0
	case OpLt:
		return cmp < 0
	case OpLte:
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

// ExportGroups exports the parsed OR/AND structure as plain immutable data.
func (c Constraint) ExportGroups() []ExportedGroup {
	if len(c.groups) == 0 {
		return nil
	}
	out := make([]ExportedGroup, 0, len(c.groups))
	for _, g := range c.groups {
		eg := ExportedGroup{Bounds: make([]ExportedBound, 0, len(g.bounds))}
		for _, b := range g.bounds {
			eg.Bounds = append(eg.Bounds, ExportedBound{
				Op:  b.op,
				Ver: b.version,
			})
		}
		out = append(out, eg)
	}
	return out
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
	case OpEq:
		return b.version.String()
	case OpNeq:
		prefix = "!="
	case OpGt:
		prefix = ">"
	case OpGte:
		prefix = ">="
	case OpLt:
		prefix = "<"
	case OpLte:
		prefix = "<="
	}
	return prefix + b.version.String()
}
