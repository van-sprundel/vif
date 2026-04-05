package version

import (
	"fmt"
	"sort"
	"strings"
)

// VersionSet represents a set of versions as a union of non-overlapping
// intervals over numeric versions, plus a separate set for dev branches
// (which lack a natural total order among themselves).
type VersionSet struct {
	// Sorted, non-overlapping intervals over non-dev versions.
	intervals []vsInterval
	// Dev branch handling. When complementDev is false, devBranches lists
	// branches that ARE included. When true, devBranches lists branches
	// that are EXCLUDED (all others are included).
	complementDev bool
	devBranches   map[string]struct{}
}

type boundKind byte

const (
	boundUnbounded boundKind = iota
	boundIncluded
	boundExcluded
)

type vsBound struct {
	kind    boundKind
	version Version
}

type vsInterval struct {
	lo vsBound
	hi vsBound
}

// ---------------- Constructors ----------------

// EmptySet returns a VersionSet containing no versions.
func EmptySet() VersionSet {
	return VersionSet{}
}

// AnySet returns a VersionSet containing all versions.
func AnySet() VersionSet {
	return VersionSet{
		intervals:     []vsInterval{{lo: vsBound{kind: boundUnbounded}, hi: vsBound{kind: boundUnbounded}}},
		complementDev: true,
	}
}

// Singleton returns a VersionSet containing exactly one version.
func Singleton(v Version) VersionSet {
	if v.Dev {
		return VersionSet{
			devBranches: map[string]struct{}{v.DevBranch: {}},
		}
	}
	return VersionSet{
		intervals: []vsInterval{{
			lo: vsBound{kind: boundIncluded, version: v},
			hi: vsBound{kind: boundIncluded, version: v},
		}},
	}
}

// ConstraintVersionSet converts a Constraint to a VersionSet.
// Uses internal access to constraint groups and bounds.
func ConstraintVersionSet(c Constraint) VersionSet {
	if len(c.groups) == 0 {
		return EmptySet()
	}

	var result VersionSet
	for i, g := range c.groups {
		groupSet := groupToVersionSet(g)
		if i == 0 {
			result = groupSet
		} else {
			result = result.Union(groupSet)
		}
	}
	return result
}

// groupToVersionSet converts a single constraint group (AND of bounds) to a VersionSet.
func groupToVersionSet(g constraintGroup) VersionSet {
	// Start with "any" and intersect each bound.
	result := AnySet()
	for _, b := range g.bounds {
		result = result.Intersect(boundToVersionSet(b))
	}
	return result
}

// boundToVersionSet converts a single bound to a VersionSet.
func boundToVersionSet(b bound) VersionSet {
	switch b.op {
	case OpEq:
		return Singleton(b.version)
	case OpNeq:
		return Singleton(b.version).Complement()
	case OpGte:
		return VersionSet{
			intervals:     []vsInterval{{lo: vsBound{kind: boundIncluded, version: b.version}, hi: vsBound{kind: boundUnbounded}}},
			complementDev: true, // dev branches are below all numeric, so >=anything excludes them... unless the bound itself is dev-level
		}
	case OpGt:
		return VersionSet{
			intervals:     []vsInterval{{lo: vsBound{kind: boundExcluded, version: b.version}, hi: vsBound{kind: boundUnbounded}}},
			complementDev: true,
		}
	case OpLte:
		return VersionSet{
			intervals:     []vsInterval{{lo: vsBound{kind: boundUnbounded}, hi: vsBound{kind: boundIncluded, version: b.version}}},
			complementDev: true, // include all dev branches (they're below everything)
		}
	case OpLt:
		return VersionSet{
			intervals:     []vsInterval{{lo: vsBound{kind: boundUnbounded}, hi: vsBound{kind: boundExcluded, version: b.version}}},
			complementDev: true,
		}
	}
	return EmptySet()
}

// ---------------- Set Operations ----------------

// Complement returns the set of all versions NOT in vs.
func (vs VersionSet) Complement() VersionSet {
	return VersionSet{
		intervals:     complementIntervals(vs.intervals),
		complementDev: !vs.complementDev,
		devBranches:   copyDevSet(vs.devBranches),
	}
}

// Intersect returns the set of versions in both vs and other.
func (vs VersionSet) Intersect(other VersionSet) VersionSet {
	return VersionSet{
		intervals:     intersectIntervals(vs.intervals, other.intervals),
		complementDev: intersectDevComplement(vs.complementDev, other.complementDev),
		devBranches:   intersectDevBranches(vs.complementDev, vs.devBranches, other.complementDev, other.devBranches),
	}
}

// Union returns the set of versions in vs or other (or both).
func (vs VersionSet) Union(other VersionSet) VersionSet {
	return VersionSet{
		intervals:     unionIntervals(vs.intervals, other.intervals),
		complementDev: unionDevComplement(vs.complementDev, other.complementDev),
		devBranches:   unionDevBranches(vs.complementDev, vs.devBranches, other.complementDev, other.devBranches),
	}
}

// IsEmpty returns true if the set contains no versions.
// Note: a set with complementDev=true is conceptually infinite (all dev branches)
// and is never considered empty.
func (vs VersionSet) IsEmpty() bool {
	if len(vs.intervals) > 0 {
		return false
	}
	if vs.complementDev {
		return false // infinite set of dev branches
	}
	return len(vs.devBranches) == 0
}

// IsAny returns true if the set contains all versions.
func (vs VersionSet) IsAny() bool {
	if len(vs.intervals) != 1 {
		return false
	}
	iv := vs.intervals[0]
	if iv.lo.kind != boundUnbounded || iv.hi.kind != boundUnbounded {
		return false
	}
	return vs.complementDev && len(vs.devBranches) == 0
}

// Contains reports whether v is in the set.
func (vs VersionSet) Contains(v Version) bool {
	if v.Dev {
		return devSetContains(vs.complementDev, vs.devBranches, v.DevBranch)
	}
	return intervalsContain(vs.intervals, v)
}

// AllowsAny reports whether vs and other have any version in common.
func (vs VersionSet) AllowsAny(other VersionSet) bool {
	if intervalsOverlap(vs.intervals, other.intervals) {
		return true
	}
	return devSetsOverlap(vs.complementDev, vs.devBranches, other.complementDev, other.devBranches)
}

// IsSubsetOf reports whether every version in vs is also in other.
func (vs VersionSet) IsSubsetOf(other VersionSet) bool {
	if !intervalsSubset(vs.intervals, other.intervals) {
		return false
	}
	return devSubset(vs.complementDev, vs.devBranches, other.complementDev, other.devBranches)
}

// ---------------- Bound comparison helpers ----------------

// compareLo compares two lower bounds. Returns -1, 0, or 1.
// Unbounded is less than any bounded value.
// At the same version, Included < Excluded (Included is "wider" for a lower bound).
func compareLo(a, b vsBound) int {
	if a.kind == boundUnbounded && b.kind == boundUnbounded {
		return 0
	}
	if a.kind == boundUnbounded {
		return -1
	}
	if b.kind == boundUnbounded {
		return 1
	}
	cmp := Compare(a.version, b.version)
	if cmp != 0 {
		return cmp
	}
	// Same version: Included < Excluded for lower bounds.
	if a.kind == b.kind {
		return 0
	}
	if a.kind == boundIncluded {
		return -1
	}
	return 1
}

// compareHi compares two upper bounds. Returns -1, 0, or 1.
// Unbounded is greater than any bounded value.
// At the same version, Excluded < Included (Included is "wider" for an upper bound).
func compareHi(a, b vsBound) int {
	if a.kind == boundUnbounded && b.kind == boundUnbounded {
		return 0
	}
	if a.kind == boundUnbounded {
		return 1
	}
	if b.kind == boundUnbounded {
		return -1
	}
	cmp := Compare(a.version, b.version)
	if cmp != 0 {
		return cmp
	}
	// Same version: Excluded < Included for upper bounds.
	if a.kind == b.kind {
		return 0
	}
	if a.kind == boundIncluded {
		return 1
	}
	return -1
}

// maxLo returns the higher (more restrictive) of two lower bounds.
func maxLo(a, b vsBound) vsBound {
	if compareLo(a, b) >= 0 {
		return a
	}
	return b
}

// minHi returns the lower (more restrictive) of two upper bounds.
func minHi(a, b vsBound) vsBound {
	if compareHi(a, b) <= 0 {
		return a
	}
	return b
}

// intervalNonEmpty returns true if [lo, hi] represents a non-empty interval.
func intervalNonEmpty(lo, hi vsBound) bool {
	if lo.kind == boundUnbounded || hi.kind == boundUnbounded {
		return true
	}
	cmp := Compare(lo.version, hi.version)
	if cmp < 0 {
		return true
	}
	if cmp > 0 {
		return false
	}
	return lo.kind == boundIncluded && hi.kind == boundIncluded
}

// flipBound converts Included to Excluded and vice versa (for complement gaps).
func flipBound(b vsBound) vsBound {
	switch b.kind {
	case boundIncluded:
		return vsBound{kind: boundExcluded, version: b.version}
	case boundExcluded:
		return vsBound{kind: boundIncluded, version: b.version}
	}
	return b
}

// hiReachesLo reports whether two intervals touch or overlap:
// first interval ending at hi, second starting at lo.
func hiReachesLo(hi, lo vsBound) bool {
	if hi.kind == boundUnbounded || lo.kind == boundUnbounded {
		return true
	}
	cmp := Compare(hi.version, lo.version)
	if cmp > 0 {
		return true
	}
	if cmp < 0 {
		return false
	}
	// Same version: touch unless both excluded.
	return hi.kind == boundIncluded || lo.kind == boundIncluded
}

// ---------------- Interval operations ----------------

func complementIntervals(intervals []vsInterval) []vsInterval {
	if len(intervals) == 0 {
		return []vsInterval{{lo: vsBound{kind: boundUnbounded}, hi: vsBound{kind: boundUnbounded}}}
	}

	var result []vsInterval

	// Gap before first interval.
	if intervals[0].lo.kind != boundUnbounded {
		result = append(result, vsInterval{
			lo: vsBound{kind: boundUnbounded},
			hi: flipBound(intervals[0].lo),
		})
	}

	// Gaps between intervals.
	for i := 1; i < len(intervals); i++ {
		result = append(result, vsInterval{
			lo: flipBound(intervals[i-1].hi),
			hi: flipBound(intervals[i].lo),
		})
	}

	// Gap after last interval.
	last := intervals[len(intervals)-1]
	if last.hi.kind != boundUnbounded {
		result = append(result, vsInterval{
			lo: flipBound(last.hi),
			hi: vsBound{kind: boundUnbounded},
		})
	}

	return result
}

func intersectIntervals(a, b []vsInterval) []vsInterval {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}

	var result []vsInterval
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo := maxLo(a[i].lo, b[j].lo)
		hi := minHi(a[i].hi, b[j].hi)
		if intervalNonEmpty(lo, hi) {
			result = append(result, vsInterval{lo: lo, hi: hi})
		}
		if compareHi(a[i].hi, b[j].hi) <= 0 {
			i++
		} else {
			j++
		}
	}
	return result
}

func unionIntervals(a, b []vsInterval) []vsInterval {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}

	merged := make([]vsInterval, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	return normalizeIntervals(merged)
}

func normalizeIntervals(intervals []vsInterval) []vsInterval {
	if len(intervals) <= 1 {
		return intervals
	}
	sort.Slice(intervals, func(i, j int) bool {
		return compareLo(intervals[i].lo, intervals[j].lo) < 0
	})
	result := []vsInterval{intervals[0]}
	for _, iv := range intervals[1:] {
		last := &result[len(result)-1]
		if hiReachesLo(last.hi, iv.lo) {
			if compareHi(iv.hi, last.hi) > 0 {
				last.hi = iv.hi
			}
		} else {
			result = append(result, iv)
		}
	}
	return result
}

func intervalsContain(intervals []vsInterval, v Version) bool {
	for _, iv := range intervals {
		if iv.lo.kind != boundUnbounded {
			cmp := Compare(v, iv.lo.version)
			if cmp < 0 || (cmp == 0 && iv.lo.kind == boundExcluded) {
				continue
			}
		}
		if iv.hi.kind != boundUnbounded {
			cmp := Compare(v, iv.hi.version)
			if cmp > 0 || (cmp == 0 && iv.hi.kind == boundExcluded) {
				continue
			}
		}
		return true
	}
	return false
}

func intervalsOverlap(a, b []vsInterval) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo := maxLo(a[i].lo, b[j].lo)
		hi := minHi(a[i].hi, b[j].hi)
		if intervalNonEmpty(lo, hi) {
			return true
		}
		if compareHi(a[i].hi, b[j].hi) <= 0 {
			i++
		} else {
			j++
		}
	}
	return false
}

func intervalsSubset(a, b []vsInterval) bool {
	if len(a) == 0 {
		return true
	}
	if len(b) == 0 {
		return false
	}

	j := 0
	for _, ai := range a {
		// Find a b interval that covers ai entirely.
		for j < len(b) && compareHi(b[j].hi, ai.lo) < 0 {
			j++
		}
		if j >= len(b) {
			return false
		}
		// b[j] must start at or before ai.lo and end at or after ai.hi.
		if compareLo(b[j].lo, ai.lo) > 0 {
			return false
		}
		if compareHi(b[j].hi, ai.hi) < 0 {
			return false
		}
	}
	return true
}

// compareHiToLo compares an upper bound to a lower bound for subset checking.
// Returns negative if hi is "before" lo.
func compareHiToLo(hi, lo vsBound) int {
	if hi.kind == boundUnbounded {
		return 1
	}
	if lo.kind == boundUnbounded {
		return 1
	}
	cmp := Compare(hi.version, lo.version)
	if cmp != 0 {
		return cmp
	}
	// At same version: Excluded hi is before Included lo (gap), else not.
	if hi.kind == boundExcluded {
		return -1
	}
	return 0
}

// ---------------- Dev set operations ----------------

func copyDevSet(m map[string]struct{}) map[string]struct{} {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]struct{}, len(m))
	for k := range m {
		c[k] = struct{}{}
	}
	return c
}

func devSetContains(complementDev bool, branches map[string]struct{}, branch string) bool {
	_, in := branches[branch]
	if complementDev {
		return !in // included unless excluded
	}
	return in // included only if listed
}

func devSetsOverlap(compA bool, brA map[string]struct{}, compB bool, brB map[string]struct{}) bool {
	if !compA && !compB {
		// Both are include-lists: overlap if they share a branch.
		for k := range brA {
			if _, ok := brB[k]; ok {
				return true
			}
		}
		return false
	}
	if compA && compB {
		// Both are exclude-lists: always overlap (infinite sets).
		return true
	}
	// One include-list, one exclude-list.
	incl, excl := brA, brB
	if compA {
		incl, excl = brB, brA
	}
	// Overlap if any included branch is not excluded.
	for k := range incl {
		if _, excluded := excl[k]; !excluded {
			return true
		}
	}
	return false
}

func devSubset(compA bool, brA map[string]struct{}, compB bool, brB map[string]struct{}) bool {
	if !compA && !compB {
		// A ⊆ B: every included in A must be included in B.
		for k := range brA {
			if _, ok := brB[k]; !ok {
				return false
			}
		}
		return true
	}
	if !compA && compB {
		// A is finite, B is "all except brB". A ⊆ B iff no element of A is in brB.
		for k := range brA {
			if _, excluded := brB[k]; excluded {
				return false
			}
		}
		return true
	}
	if compA && !compB {
		// A is infinite ("all except brA"), B is finite. Can't be subset.
		return false
	}
	// Both complement: "all except A" ⊆ "all except B" iff B ⊆ A.
	for k := range brB {
		if _, ok := brA[k]; !ok {
			return false
		}
	}
	return true
}

func intersectDevComplement(compA, compB bool) bool {
	// (false,A)∩(false,B) → false; (false,A)∩(true,B) → false;
	// (true,A)∩(false,B) → false; (true,A)∩(true,B) → true
	return compA && compB
}

func intersectDevBranches(compA bool, brA map[string]struct{}, compB bool, brB map[string]struct{}) map[string]struct{} {
	if !compA && !compB {
		// Both include-lists: intersection of sets.
		return setIntersect(brA, brB)
	}
	if compA && compB {
		// Both exclude-lists: union of excludes.
		return setUnion(brA, brB)
	}
	// One include-list, one exclude-list: keep includes not in excludes.
	incl, excl := brA, brB
	if compA {
		incl, excl = brB, brA
	}
	return setDifference(incl, excl)
}

func unionDevComplement(compA, compB bool) bool {
	// (false,A)∪(false,B) → false; (false,A)∪(true,B) → true;
	// (true,A)∪(false,B) → true; (true,A)∪(true,B) → true
	return compA || compB
}

func unionDevBranches(compA bool, brA map[string]struct{}, compB bool, brB map[string]struct{}) map[string]struct{} {
	if !compA && !compB {
		// Both include-lists: union of sets.
		return setUnion(brA, brB)
	}
	if compA && compB {
		// Both exclude-lists: intersection of excludes (only exclude what both exclude).
		return setIntersect(brA, brB)
	}
	// One include-list, one exclude-list: result is complement, excludes minus includes.
	excl, incl := brA, brB
	if !compA {
		excl, incl = brB, brA
	}
	return setDifference(excl, incl)
}

// ---------------- Set helpers ----------------

func setIntersect(a, b map[string]struct{}) map[string]struct{} {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	result := make(map[string]struct{})
	for k := range a {
		if _, ok := b[k]; ok {
			result[k] = struct{}{}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func setUnion(a, b map[string]struct{}) map[string]struct{} {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		result[k] = struct{}{}
	}
	for k := range b {
		result[k] = struct{}{}
	}
	return result
}

func setDifference(a, b map[string]struct{}) map[string]struct{} {
	if len(a) == 0 {
		return nil
	}
	result := make(map[string]struct{})
	for k := range a {
		if _, ok := b[k]; !ok {
			result[k] = struct{}{}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// ---------------- String ----------------

func (vs VersionSet) String() string {
	if vs.IsEmpty() {
		return "(empty)"
	}
	if vs.IsAny() {
		return "(any)"
	}

	var parts []string
	for _, iv := range vs.intervals {
		parts = append(parts, intervalString(iv))
	}
	if vs.complementDev {
		if len(vs.devBranches) > 0 {
			branches := make([]string, 0, len(vs.devBranches))
			for b := range vs.devBranches {
				branches = append(branches, "dev-"+b)
			}
			sort.Strings(branches)
			parts = append(parts, fmt.Sprintf("all-dev-except(%s)", strings.Join(branches, ", ")))
		} else {
			parts = append(parts, "all-dev")
		}
	} else if len(vs.devBranches) > 0 {
		branches := make([]string, 0, len(vs.devBranches))
		for b := range vs.devBranches {
			branches = append(branches, "dev-"+b)
		}
		sort.Strings(branches)
		parts = append(parts, strings.Join(branches, ", "))
	}

	return strings.Join(parts, " | ")
}

func intervalString(iv vsInterval) string {
	if iv.lo.kind == boundUnbounded && iv.hi.kind == boundUnbounded {
		return "(any-numeric)"
	}
	// Singleton: [v, v]
	if iv.lo.kind == boundIncluded && iv.hi.kind == boundIncluded &&
		Compare(iv.lo.version, iv.hi.version) == 0 {
		return iv.lo.version.String()
	}

	var b strings.Builder
	switch iv.lo.kind {
	case boundUnbounded:
		b.WriteString("(-∞")
	case boundIncluded:
		fmt.Fprintf(&b, "[%s", iv.lo.version)
	case boundExcluded:
		fmt.Fprintf(&b, "(%s", iv.lo.version)
	}
	b.WriteString(", ")
	switch iv.hi.kind {
	case boundUnbounded:
		b.WriteString("+∞)")
	case boundIncluded:
		fmt.Fprintf(&b, "%s]", iv.hi.version)
	case boundExcluded:
		fmt.Fprintf(&b, "%s)", iv.hi.version)
	}
	return b.String()
}
