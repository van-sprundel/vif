package resolver

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/version"
)

type pgTerm struct {
	pkg string
	set version.VersionSet
}

type pgIncompatibility struct {
	terms []pgTerm
}

type pgAssignment struct {
	pkg           string
	set           version.VersionSet
	isDecision    bool
	decisionLevel int
	cause         *pgIncompatibility
	index         int
}

type pgDecision struct {
	pkg   string
	cand  candidate
	level int
}

type pgPendingMeta struct {
	dev bool
}

type pgPartialSolution struct {
	assignments []pgAssignment
	allowed     map[string]version.VersionSet
	decided     map[string]version.Version
	level       int
}

func newPGPartialSolution() pgPartialSolution {
	return pgPartialSolution{
		allowed: make(map[string]version.VersionSet),
		decided: make(map[string]version.Version),
	}
}

func (s *pgPartialSolution) allowedSet(pkgName string) version.VersionSet {
	if set, ok := s.allowed[pkgName]; ok {
		return set
	}
	return version.AnySet()
}

func (s *pgPartialSolution) constrain(pkgName string, set version.VersionSet, decision bool, cause *pgIncompatibility) (bool, bool) {
	old := s.allowedSet(pkgName)
	next := old.Intersect(set)
	if next.IsEmpty() {
		return false, true
	}
	if setsEqual(old, next) {
		return false, false
	}
	s.allowed[pkgName] = next
	s.assignments = append(s.assignments, pgAssignment{
		pkg:           pkgName,
		set:           set,
		isDecision:    decision,
		decisionLevel: s.level,
		cause:         cause,
		index:         len(s.assignments),
	})
	if decided, ok := s.decided[pkgName]; ok && !next.Contains(decided) {
		return false, true
	}
	return true, false
}

func setsEqual(a, b version.VersionSet) bool {
	return a.IsSubsetOf(b) && b.IsSubsetOf(a)
}

type pubGrubSolver struct {
	r *resolver
	s *state

	solution pgPartialSolution

	incompatibilities []*pgIncompatibility
	incompatByPkg     map[string][]*pgIncompatibility

	decisions []pgDecision

	pending         map[string]pgPendingMeta
	minStabilityByP map[string]version.Stability

	// conflictPkgs accumulates all packages seen during conflict resolution
	// across the entire solve, for better error messages.
	conflictPkgs map[string]struct{}
}

func (r *resolver) solvePubGrub(s *state, reqs []requirement) bool {
	pg := &pubGrubSolver{
		r:               r,
		s:               s,
		solution:        newPGPartialSolution(),
		incompatByPkg:   make(map[string][]*pgIncompatibility),
		pending:         make(map[string]pgPendingMeta),
		minStabilityByP: make(map[string]version.Stability),
		conflictPkgs:    make(map[string]struct{}),
	}
	pg.seedRootRequirements(reqs)
	return pg.solve()
}

func (pg *pubGrubSolver) solve() bool {
	queue := pg.initialQueue()

	for {
		if !pg.r.checkContext() {
			return false
		}

		if conflict := pg.propagate(&queue); conflict != nil {
			if !pg.resolveConflict(conflict) {
				return false
			}
			queue = pg.initialQueue()
			continue
		}

		if pg.allPendingSatisfied() {
			return true
		}

		decided, missingOnly, unsat := pg.decide(&queue)
		if decided {
			continue
		}

		if missingOnly != "" {
			pg.r.terminalErr = fmt.Errorf("resolver: package %s was not found on Packagist; private/custom repositories are not supported yet", missingOnly)
			return false
		}
		if unsat != "" {
			return false
		}

		pg.r.recordConflict("", version.Constraint{}, "failed to resolve dependencies")
		return false
	}
}

func (pg *pubGrubSolver) seedRootRequirements(reqs []requirement) {
	for _, req := range reqs {
		pg.pending[req.name] = pgPendingMeta{dev: req.dev}
		pg.updateMinStability(req.name, req.constraint.EffectiveStability(pg.r.minimumStability))

		set := version.ConstraintVersionSet(req.constraint)
		pg.addIncompatibility(&pgIncompatibility{
			// single-term incompatibility means this term must be false; i.e.
			// package must satisfy req.constraint.
			terms: []pgTerm{{pkg: req.name, set: set.Complement()}},
		})
	}

	if pg.r.hasRestriction {
		// Restriction is applied by filterCandidates() only when/if a package is
		// actually required. It should not make that package required by itself.
	}

	for name, c := range pg.r.rootConflicts {
		set := version.ConstraintVersionSet(c)
		pg.pending[name] = pgPendingMeta{}
		pg.addIncompatibility(&pgIncompatibility{
			terms: []pgTerm{{pkg: name, set: set}},
		})
	}
}

func (pg *pubGrubSolver) initialQueue() []string {
	keys := make([]string, 0, len(pg.pending))
	for name := range pg.pending {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

func (pg *pubGrubSolver) addIncompatibility(inc *pgIncompatibility) {
	if len(inc.terms) == 0 {
		return
	}
	pg.incompatibilities = append(pg.incompatibilities, inc)
	for _, term := range inc.terms {
		if pkg.IsPlatformPackage(term.pkg) {
			continue
		}
		pg.incompatByPkg[term.pkg] = append(pg.incompatByPkg[term.pkg], inc)
	}
}

func (pg *pubGrubSolver) updateMinStability(pkgName string, s version.Stability) {
	current, ok := pg.minStabilityByP[pkgName]
	if !ok || s < current {
		pg.minStabilityByP[pkgName] = s
	}
}

func (pg *pubGrubSolver) termState(term pgTerm) int {
	// 1 = satisfied, 0 = undecided, -1 = unsatisfied
	if decided, ok := pg.solution.decided[term.pkg]; ok {
		if term.set.Contains(decided) {
			return 1
		}
		return -1
	}
	allowed := pg.solution.allowedSet(term.pkg)
	if allowed.IsSubsetOf(term.set) {
		return 1
	}
	if !allowed.AllowsAny(term.set) {
		return -1
	}
	return 0
}

func (pg *pubGrubSolver) propagate(queue *[]string) *pgIncompatibility {
	for len(*queue) > 0 {
		pkgName := (*queue)[0]
		*queue = (*queue)[1:]

		for _, inc := range pg.incompatByPkg[pkgName] {
			unsatisfied := 0
			undecidedIndex := -1
			for i, term := range inc.terms {
				switch pg.termState(term) {
				case -1:
					unsatisfied++
				case 0:
					if undecidedIndex == -1 {
						undecidedIndex = i
					} else {
						undecidedIndex = -2
					}
				}
			}
			if unsatisfied > 0 {
				continue
			}
			if undecidedIndex == -1 {
				// All terms satisfied: conflict.
				return inc
			}
			if undecidedIndex >= 0 {
				// Unit propagation: derive negation of the remaining undecided term.
				remaining := inc.terms[undecidedIndex]
				derived := remaining.set.Complement()
				changed, conflict := pg.solution.constrain(remaining.pkg, derived, false, inc)
				if conflict {
					return inc
				}
				if changed {
					*queue = append(*queue, remaining.pkg)
				}
			}
		}
	}
	return nil
}

func (pg *pubGrubSolver) resolveConflict(conflict *pgIncompatibility) bool {
	current := conflict

	// Track all packages seen during conflict resolution for error messages.
	for _, t := range current.terms {
		pg.conflictPkgs[t.pkg] = struct{}{}
	}

	for {
		if len(current.terms) == 0 {
			pg.reportConflict(pg.conflictPkgs)
			return false
		}

		// Find the most recent satisfier across all terms in the incompatibility.
		var mostRecent pgAssignment
		var mostRecentTermPkg string
		found := false

		for _, t := range current.terms {
			sat, ok := pg.findSatisfier(t)
			if !ok {
				continue
			}
			if !found || sat.index > mostRecent.index {
				mostRecent = sat
				mostRecentTermPkg = t.pkg
				found = true
			}
		}

		if !found {
			pg.reportConflict(pg.conflictPkgs)
			return false
		}

		if mostRecent.isDecision {
			pg.backtrackTo(mostRecent.decisionLevel - 1)
			pg.addIncompatibility(current)
			return true
		}

		if mostRecent.cause == nil {
			pg.reportConflict(pg.conflictPkgs)
			return false
		}

		// Track packages from the cause before resolving.
		for _, t := range mostRecent.cause.terms {
			pg.conflictPkgs[t.pkg] = struct{}{}
		}

		current = resolvePGIncompatibilities(current, mostRecent.cause, mostRecentTermPkg)

		for _, t := range current.terms {
			pg.conflictPkgs[t.pkg] = struct{}{}
		}
	}
}

func (pg *pubGrubSolver) reportConflict(allPkgs map[string]struct{}) {
	pkgs := make([]string, 0, len(allPkgs))
	for p := range allPkgs {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	msg := fmt.Sprintf("no version of %s could satisfy all constraints", strings.Join(pkgs, ", "))
	for _, p := range pkgs {
		pg.r.recordConflict(p, version.Constraint{}, msg)
	}
}

// resolvePGIncompatibilities combines two incompatibilities by resolving out
// the shared package. For any other package appearing in both, intersect the
// version sets. This is the PubGrub resolution rule — analogous to resolution
// in propositional logic.
func resolvePGIncompatibilities(a, b *pgIncompatibility, pkg string) *pgIncompatibility {
	terms := make(map[string]pgTerm)

	for _, t := range a.terms {
		if t.pkg == pkg {
			continue
		}
		terms[t.pkg] = t
	}

	for _, t := range b.terms {
		if t.pkg == pkg {
			continue
		}
		if existing, ok := terms[t.pkg]; ok {
			terms[t.pkg] = pgTerm{pkg: t.pkg, set: existing.set.Intersect(t.set)}
		} else {
			terms[t.pkg] = t
		}
	}

	result := make([]pgTerm, 0, len(terms))
	for _, t := range terms {
		result = append(result, t)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].pkg < result[j].pkg })

	return &pgIncompatibility{terms: result}
}

// findSatisfier returns the earliest assignment that, combined with all
// previous assignments for the same package, causes the given term to be
// satisfied (allowed set ⊆ term set).
func (pg *pubGrubSolver) findSatisfier(t pgTerm) (pgAssignment, bool) {
	cumulative := version.AnySet()

	for _, a := range pg.solution.assignments {
		if a.pkg != t.pkg {
			continue
		}
		cumulative = cumulative.Intersect(a.set)
		if cumulative.IsSubsetOf(t.set) {
			return a, true
		}
	}

	return pgAssignment{}, false
}

// backtrackTo removes all assignments and decisions above the given level,
// then rebuilds the solution state from the remaining assignments.
func (pg *pubGrubSolver) backtrackTo(level int) {
	// Truncate assignments above the target level.
	cutoff := 0
	for i, a := range pg.solution.assignments {
		if a.decisionLevel > level {
			cutoff = i
			goto truncate
		}
	}
	cutoff = len(pg.solution.assignments)
truncate:
	pg.solution.assignments = pg.solution.assignments[:cutoff]
	pg.solution.level = level

	// Remove decisions above the target level.
	kept := pg.decisions[:0]
	for _, d := range pg.decisions {
		if d.level <= level {
			kept = append(kept, d)
		}
	}
	pg.decisions = kept

	// Rebuild allowed and decided from remaining assignments.
	pg.solution.allowed = make(map[string]version.VersionSet)
	pg.solution.decided = make(map[string]version.Version)
	for _, a := range pg.solution.assignments {
		if cur, ok := pg.solution.allowed[a.pkg]; ok {
			pg.solution.allowed[a.pkg] = cur.Intersect(a.set)
		} else {
			pg.solution.allowed[a.pkg] = a.set
		}
	}
	for _, d := range pg.decisions {
		pg.solution.decided[d.pkg] = d.cand.version
	}

	// Rebuild s.resolved and s.providers from remaining decisions.
	pg.s.resolved = make(map[string]resolvedEntry)
	pg.s.providers = make(map[string]provider)
	for _, d := range pg.decisions {
		pg.s.resolved[d.pkg] = resolvedEntry{
			entry:     d.cand.entry,
			version:   d.cand.version,
			conflicts: d.cand.conflicts,
			dev:       pg.pending[d.pkg].dev,
		}
		pg.s.registerProviders(d.cand.entry)
	}
}

func (pg *pubGrubSolver) allPendingSatisfied() bool {
	for name := range pg.pending {
		if !pg.requirementSatisfied(name) {
			return false
		}
	}
	return true
}

func (pg *pubGrubSolver) requirementSatisfied(name string) bool {
	if pkg.IsPlatformPackage(name) {
		return true
	}

	allowed := pg.solution.allowedSet(name)
	if allowed.IsEmpty() {
		return false
	}

	if resolved, ok := pg.s.resolved[name]; ok {
		return allowed.Contains(resolved.version)
	}

	if prov, ok := pg.s.providers[name]; ok {
		resolved, hasReal := pg.s.resolved[prov.realName]
		if hasReal {
			pv, ok := pg.r.providedVersion(resolved.entry, name)
			if ok {
				if pv.any {
					return true
				}
				if pv.hasValue {
					return allowed.Contains(pv.value)
				}
			}
		}
	}

	if rs, ok := pg.r.rootSatisfiers[name]; ok {
		if rs.any {
			return true
		}
		if rs.hasValue {
			return allowed.Contains(rs.value)
		}
	}

	return false
}

func (pg *pubGrubSolver) decide(queue *[]string) (bool, string, string) {
	names := make([]string, 0, len(pg.pending))
	for name := range pg.pending {
		names = append(names, name)
	}
	sort.Strings(names)

	missingOnly := ""
	unsat := ""
	for _, name := range names {
		if pg.requirementSatisfied(name) {
			continue
		}
		if pkg.IsPlatformPackage(name) {
			continue
		}

		minStability := pg.r.minimumStability
		if s, ok := pg.minStabilityByP[name]; ok {
			minStability = s
		}
		candidates, err := pg.r.getCandidates(name, minStability)
		if err != nil {
			if errors.Is(err, packagist.ErrPackageNotFound) {
				missingOnly = name
				continue
			}
			pg.r.recordConflict(name, version.Constraint{}, fmt.Sprintf("could not fetch package %s: %v", name, err))
			unsat = name
			continue
		}
		if len(candidates) == 0 {
			pg.r.recordConflict(name, version.Constraint{}, fmt.Sprintf("no versions of %s found matching stability %s", name, stabilityString(minStability)))
			unsat = name
			continue
		}

		allowed := pg.solution.allowedSet(name)
		for _, c := range candidates {
			if !allowed.Contains(c.version) {
				continue
			}

			level := len(pg.decisions) + 1
			pg.solution.level = level
			_, conflict := pg.solution.constrain(name, version.Singleton(c.version), true, nil)
			if conflict {
				continue
			}
			pg.solution.decided[name] = c.version
			pg.decisions = append(pg.decisions, pgDecision{pkg: name, cand: c, level: level})
			pg.s.resolved[name] = resolvedEntry{
				entry:     c.entry,
				version:   c.version,
				conflicts: c.conflicts,
				dev:       pg.pending[name].dev,
			}
			pg.s.registerProviders(c.entry)
			*queue = append(*queue, name)

			pg.emitCandidateIncompatibilities(name, c)
			return true, "", ""
		}
		// No candidate in the allowed set works. In PubGrub, we add a
		// single-term incompatibility saying the allowed set is impossible,
		// then let propagation + resolveConflict handle backjumping.
		pg.addIncompatibility(&pgIncompatibility{
			terms: []pgTerm{{pkg: name, set: allowed}},
		})
		*queue = append(*queue, name)
		return true, "", ""
	}

	return false, missingOnly, unsat
}

func (pg *pubGrubSolver) emitCandidateIncompatibilities(name string, c candidate) {
	selected := pgTerm{pkg: name, set: version.Singleton(c.version)}

	for _, dep := range c.dependencies {
		if pkg.IsPlatformPackage(dep.name) {
			continue
		}
		depSet := version.ConstraintVersionSet(dep.constraint)
		pg.addIncompatibility(&pgIncompatibility{
			terms: []pgTerm{
				selected,
				{pkg: dep.name, set: depSet.Complement()},
			},
		})
		parent := pg.pending[name]
		pg.pending[dep.name] = pgPendingMeta{dev: parent.dev}
		pg.updateMinStability(dep.name, dep.constraint.EffectiveStability(pg.r.minimumStability))
	}

	for _, dep := range c.conflicts {
		if pkg.IsPlatformPackage(dep.name) {
			continue
		}
		conflictSet := version.ConstraintVersionSet(dep.constraint)
		pg.addIncompatibility(&pgIncompatibility{
			terms: []pgTerm{
				selected,
				{pkg: dep.name, set: conflictSet},
			},
		})
		pg.pending[dep.name] = pgPendingMeta{}
	}

	addProvided := func(virtual, raw string) {
		if pkg.IsPlatformPackage(virtual) {
			return
		}
		if raw == "" || raw == "*" {
			return
		}
		c, err := version.ParseConstraint(raw)
		if err != nil {
			return
		}
		providedSet := version.ConstraintVersionSet(c)
		pg.addIncompatibility(&pgIncompatibility{
			terms: []pgTerm{
				selected,
				{pkg: virtual, set: providedSet.Complement()},
			},
		})
		pg.pending[virtual] = pgPendingMeta{}
	}

	for virtual, raw := range c.entry.Provide {
		addProvided(virtual, raw)
	}
	for virtual, raw := range c.entry.Replace {
		addProvided(virtual, raw)
	}
}
