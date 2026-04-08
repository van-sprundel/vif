package resolver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/version"
)

// ResolvedPackage is a package selected by the resolver.
type ResolvedPackage struct {
	Name    string
	Version string
	Entry   packagist.VersionEntry
	Dev     bool // true if only required via require-dev
}

const requirementScoreCandidateScanLimit = 64

// Options controls solver behavior for update-like flows.
type Options struct {
	// Fixed maps package name -> version that must be selected.
	// When set, candidates are filtered to that exact version.
	Fixed map[string]string
	// Locked maps package name -> currently locked version. When set, the
	// resolver prefers the locked version over newer candidates when it still
	// satisfies all constraints.
	Locked map[string]string
	// LockedEntries holds full version entries for locked packages. Used to
	// preserve VCS-only packages that have no Packagist releases.
	LockedEntries map[string]packagist.VersionEntry
	// RestrictedPackages constrains a specific package set to Restriction.
	RestrictedPackages map[string]struct{}
	Restriction        string
	// NoDev skips require-dev dependencies from resolution entirely.
	NoDev bool
	// LookupDone is called after each metadata lookup during prefetch.
	LookupDone func(name string, duration time.Duration, err error)
	// SolveProgress is called as the backtracking solver advances.
	SolveProgress func(name string)
}

// Resolve resolves all dependencies from a composer.json using the given Packagist client.
// Returns a flat list of all resolved packages (including transitive dependencies).
func Resolve(ctx context.Context, cj *composer.ComposerJSON, client packagist.Fetcher) ([]ResolvedPackage, error) {
	return ResolveWithProgress(ctx, cj, client, nil)
}

// ResolveWithProgress resolves dependencies and reports each unique package lookup.
func ResolveWithProgress(ctx context.Context, cj *composer.ComposerJSON, client packagist.Fetcher, progress func(string)) ([]ResolvedPackage, error) {
	return ResolveWithOptions(ctx, cj, client, Options{}, progress)
}

// ResolveWithOptions resolves dependencies with optional locked-version preferences.
func ResolveWithOptions(ctx context.Context, cj *composer.ComposerJSON, client packagist.Fetcher, opts Options, progress func(string)) ([]ResolvedPackage, error) {
	minimumStability := parseStability(cj.MinimumStability)

	// Collect root package names for the parallel prefetch pass.
	var rootNames []string
	for name := range cj.NonPlatformRequire() {
		rootNames = append(rootNames, name)
	}
	if !opts.NoDev {
		for name := range cj.NonPlatformRequireDev() {
			rootNames = append(rootNames, name)
		}
	}

	// Shared version cache with mutex for concurrent prefetch + solve access.
	versionCache := make(map[string]candidateCacheEntry, len(rootNames)*4)
	var versionCacheMu sync.RWMutex

	// Start prefetch in background — it populates the cache incrementally as
	// each package's metadata arrives. The solver starts immediately and uses
	// cache hits from prefetch; on cache miss it fetches on demand. This means
	// the solver can detect conflicts early without waiting for the entire
	// dependency graph to be fetched.
	prefetchCtx, prefetchCancel := context.WithCancel(ctx)
	var prefetchWg sync.WaitGroup
	prefetchWg.Add(1)
	go func() {
		defer prefetchWg.Done()
		prefetchInto(prefetchCtx, client, rootNames, versionCache, &versionCacheMu,
			cj.PreferStable, minimumStability, opts.LockedEntries, progress, opts.LookupDone)
	}()

	r := &resolver{
		ctx:              ctx,
		client:           client,
		minimumStability: minimumStability,
		preferStable:     cj.PreferStable,
		fixed:            opts.Fixed,
		locked:           opts.Locked,
		lockedEntries:    opts.LockedEntries,
		rootProvide:      cj.Provide,
		rootReplace:      cj.Replace,
		rootConflict:     cj.Conflict,
		rootSatisfiers:   buildRootSatisfiers(cj.Provide, cj.Replace),
		rootConflicts:    buildRootConflicts(cj.Conflict),
		versionCache:     versionCache,
		versionCacheMu:   &versionCacheMu,
		// progress is nil during solve to avoid double-reporting; prefetch
		// already reports each package lookup via the progress callback.
		progress: nil,
		// solveProgress reports selected requirements during the backtracking phase.
		solveProgress:        opts.SolveProgress,
		providedVersionCache: make(map[string]providedVersion),
	}
	if opts.Restriction != "" && len(opts.RestrictedPackages) > 0 {
		c, err := version.ParseConstraint(opts.Restriction)
		if err != nil {
			prefetchCancel()
			prefetchWg.Wait()
			return nil, fmt.Errorf("resolver: parse restriction %q: %w", opts.Restriction, err)
		}
		r.restriction = c
		r.hasRestriction = true
		r.restrictedPackages = opts.RestrictedPackages
	}

	// Collect root requirements (prod, and dev unless --no-dev).
	var reqs []requirement
	for name, constraint := range cj.NonPlatformRequire() {
		c, err := version.ParseConstraint(constraint)
		if err != nil {
			prefetchCancel()
			prefetchWg.Wait()
			return nil, fmt.Errorf("resolver: parse constraint %q for %s: %w", constraint, name, err)
		}
		reqs = append(reqs, requirement{name: name, constraint: c, dev: false, root: true})
	}
	if !opts.NoDev {
		for name, constraint := range cj.NonPlatformRequireDev() {
			c, err := version.ParseConstraint(constraint)
			if err != nil {
				prefetchCancel()
				prefetchWg.Wait()
				return nil, fmt.Errorf("resolver: parse constraint %q for %s: %w", constraint, name, err)
			}
			reqs = append(reqs, requirement{name: name, constraint: c, dev: true, root: true})
		}
	}

	// Sort requirements for deterministic resolution order.
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].name < reqs[j].name })

	state := newState()
	if !r.solvePubGrub(state, reqs) {
		prefetchCancel()
		prefetchWg.Wait()
		return nil, r.buildError(state)
	}

	// Solve succeeded — stop any remaining prefetch work.
	prefetchCancel()
	prefetchWg.Wait()

	// Collect results.
	result := make([]ResolvedPackage, 0, len(state.resolved))
	for name, entry := range state.resolved {
		result = append(result, ResolvedPackage{
			Name:    name,
			Version: entry.entry.Version,
			Entry:   entry.entry,
			Dev:     entry.dev,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

type requirement struct {
	name       string
	constraint version.Constraint
	dev        bool
	root       bool
	deferred   bool // true if already deferred once (prevent infinite loop)
}

type resolvedEntry struct {
	entry     packagist.VersionEntry
	version   version.Version
	conflicts []dependencyRequirement
	dev       bool
}

type candidate struct {
	entry        packagist.VersionEntry
	version      version.Version
	dependencies []dependencyRequirement
	conflicts    []dependencyRequirement
}

type candidateCacheEntry struct {
	candidates []candidate
	err        error
	hasDev     bool
}

type dependencyRequirement struct {
	name       string
	constraint version.Constraint
	raw        string
}

// provider tracks which real package provides a virtual package.
type provider struct {
	realName string // the package that provides/replaces this virtual name
}

// state is the resolution state, cloneable for backtracking.
type state struct {
	resolved    map[string]resolvedEntry
	constraints map[string][]version.Constraint
	// providers maps virtual package names to the real package providing them.
	providers map[string]provider
}

func newState() *state {
	return &state{
		resolved:    make(map[string]resolvedEntry),
		constraints: make(map[string][]version.Constraint),
		providers:   make(map[string]provider),
	}
}

func (s *state) clone() *state {
	c := &state{
		resolved:    make(map[string]resolvedEntry, len(s.resolved)),
		constraints: make(map[string][]version.Constraint, len(s.constraints)),
		providers:   make(map[string]provider, len(s.providers)),
	}
	for k, v := range s.resolved {
		c.resolved[k] = v
	}
	for k, v := range s.constraints {
		cs := make([]version.Constraint, len(v))
		copy(cs, v)
		c.constraints[k] = cs
	}
	for k, v := range s.providers {
		c.providers[k] = v
	}
	return c
}

// registerProviders records the provide and replace entries for a resolved package.
func (s *state) registerProviders(entry packagist.VersionEntry) {
	for name := range entry.Provide {
		if pkg.IsPlatformPackage(name) {
			continue
		}
		s.providers[name] = provider{realName: entry.Name}
	}
	for name := range entry.Replace {
		if pkg.IsPlatformPackage(name) {
			continue
		}
		s.providers[name] = provider{realName: entry.Name}
	}
}

// conflict records a resolution failure for error reporting.
type conflict struct {
	packageName string
	constraint  version.Constraint
	reason      string
	depth       int // recursion depth — deeper = more specific
}

type resolver struct {
	ctx                  context.Context
	client               packagist.Fetcher
	minimumStability     version.Stability
	preferStable         bool
	fixed                map[string]string
	locked               map[string]string
	lockedEntries        map[string]packagist.VersionEntry
	rootProvide          map[string]string
	rootReplace          map[string]string
	rootConflict         map[string]string
	rootSatisfiers       map[string]rootSatisfier
	rootConflicts        map[string]version.Constraint
	restriction          version.Constraint
	hasRestriction       bool
	restrictedPackages   map[string]struct{}
	versionCache         map[string]candidateCacheEntry
	versionCacheMu       *sync.RWMutex // shared with background prefetch goroutine
	lastConflict         *conflict
	terminalErr          error
	progress             func(string)
	solveProgress        func(string)
	depth                int
	providedVersionCache map[string]providedVersion
}

type rootSatisfier struct {
	any      bool
	hasValue bool
	value    version.Version
}

type providedVersion struct {
	any      bool
	hasValue bool
	value    version.Version
	hasSet   bool
	set      version.VersionSet
}

// recordConflict records a conflict, keeping the deepest (most specific) one.
func (r *resolver) recordConflict(packageName string, constraint version.Constraint, reason string) {
	if r.lastConflict == nil || r.depth >= r.lastConflict.depth {
		r.lastConflict = &conflict{
			packageName: packageName,
			constraint:  constraint,
			reason:      reason,
			depth:       r.depth,
		}
	}
}

// solve tries to satisfy all requirements using recursive backtracking.
// Returns true if a solution was found, updating state in place.
func (r *resolver) solve(s *state, reqs []requirement) bool {
	if !r.checkContext() {
		return false
	}
	if len(reqs) == 0 {
		return true
	}

	r.depth++
	defer func() { r.depth-- }()

	req, rest, pendingConstraints := r.selectRequirement(s, reqs)
	if r.solveProgress != nil {
		r.solveProgress(req.name)
	}

	// Skip platform packages that slipped through.
	if pkg.IsPlatformPackage(req.name) {
		return r.solve(s, rest)
	}

	// If already resolved, check compatibility.
	if existing, ok := s.resolved[req.name]; ok {
		if matchesAll(existing.version, pendingConstraints) {
			// Compatible. Track constraint and continue. Restore on failure so
			// failed branches do not leak tightened constraints into siblings.
			prevConstraints, hadConstraints := s.constraints[req.name]
			mergedConstraints := appendConstraintsUnique(append([]version.Constraint{}, prevConstraints...), pendingConstraints)
			s.constraints[req.name] = mergedConstraints
			prevEntry := existing
			if existing.dev && !req.dev {
				existing.dev = false
				s.resolved[req.name] = existing
			}
			if r.solve(s, rest) {
				return true
			}
			if hadConstraints {
				s.constraints[req.name] = prevConstraints
			} else {
				delete(s.constraints, req.name)
			}
			if prevEntry.dev != existing.dev {
				s.resolved[req.name] = prevEntry
			}
			return false
		}
		// Not compatible — record conflict and let caller backtrack.
		r.recordConflict(req.name, req.constraint,
			fmt.Sprintf("required version %s conflicts with already resolved %s@%s", req.constraint, req.name, existing.entry.Version))
		return false
	}

	// Check if this requirement is satisfied by a provide/replace from
	// an already-resolved package.
	if prov, ok := s.providers[req.name]; ok {
		existing, hasReal := s.resolved[prov.realName]
		if hasReal {
			if pv, ok := r.providedVersion(existing.entry, req.name); ok {
				if !providedMatchesAll(pv, pendingConstraints) {
					r.recordConflict(req.name, req.constraint,
						fmt.Sprintf("%s provides %s which does not match %s", prov.realName, req.name, req.constraint))
					return false
				}
			}
			return r.solve(s, rest)
		}
	}
	if r.rootSatisfies(req.name, pendingConstraints) {
		return r.solve(s, rest)
	}

	// Compute effective stability for this requirement (explicit @flag > implicit dev-branch > global).
	effectiveStability := req.constraint.EffectiveStability(r.minimumStability)

	// Fetch candidates from packagist.
	candidates, err := r.getCandidates(req.name, effectiveStability)
	if err != nil || len(candidates) == 0 {
		if err != nil && errors.Is(err, packagist.ErrPackageNotFound) {
			// A missing package can still be satisfiable later via provide/replace
			// from another unresolved package in the queue.
			if !req.deferred && len(rest) > 0 {
				deferred := req
				deferred.deferred = true
				return r.solve(s, append(rest, deferred))
			}
			r.terminalErr = fmt.Errorf("resolver: package %s was not found on Packagist; private/custom repositories are not supported yet", req.name)
			return false
		}
		// Non-terminal fetch failures get one defer pass in case a later
		// resolved package provides/replaces this name. A Packagist 404 is
		// terminal for Phase 1 and should fail fast.
		if err != nil && !errors.Is(err, packagist.ErrPackageNotFound) && !req.deferred && len(rest) > 0 {
			deferred := req
			deferred.deferred = true
			return r.solve(s, append(rest, deferred))
		}
		if err != nil {
			r.recordConflict(req.name, version.Constraint{},
				fmt.Sprintf("could not fetch package %s (not provided by any resolved package): %v", req.name, err))
		} else {
			r.recordConflict(req.name, req.constraint,
				fmt.Sprintf("no versions of %s found matching stability %s", req.name, stabilityString(effectiveStability)))
		}
		return false
	}

	// Try each candidate version (highest first).
	var lastRejectedVersion string
	for _, c := range candidates {
		if !r.checkContext() {
			return false
		}
		if !matchesAll(c.version, pendingConstraints) {
			lastRejectedVersion = c.entry.Version
			continue
		}
		// Also check all accumulated constraints from other resolved packages.
		if !matchesAll(c.version, s.constraints[req.name]) {
			lastRejectedVersion = c.entry.Version
			continue
		}
		if reason, ok := r.requiresAlreadyResolvedConflict(c, s); ok {
			lastRejectedVersion = c.entry.Version
			r.recordConflict(req.name, req.constraint, reason)
			continue
		}
		if conflictReason, ok := r.conflictsWithResolved(c, s); ok {
			lastRejectedVersion = c.entry.Version
			r.recordConflict(req.name, req.constraint, conflictReason)
			continue
		}

		// Try this candidate: mutate local state and roll back on failure.
		prevConstraints, hadConstraints := s.constraints[req.name]
		mergedConstraints := appendConstraintsUnique(append([]version.Constraint{}, prevConstraints...), pendingConstraints)
		prevResolved, hadResolved := s.resolved[req.name]
		s.resolved[req.name] = resolvedEntry{entry: c.entry, version: c.version, conflicts: c.conflicts, dev: req.dev}
		s.constraints[req.name] = mergedConstraints
		type providerRollback struct {
			name    string
			prev    provider
			existed bool
		}
		rollbacks := make([]providerRollback, 0, len(c.entry.Provide)+len(c.entry.Replace))
		recordProvider := func(name string) {
			if pkg.IsPlatformPackage(name) {
				return
			}
			prev, existed := s.providers[name]
			rollbacks = append(rollbacks, providerRollback{name: name, prev: prev, existed: existed})
			s.providers[name] = provider{realName: c.entry.Name}
		}
		for name := range c.entry.Provide {
			recordProvider(name)
		}
		for name := range c.entry.Replace {
			recordProvider(name)
		}

		// Gather transitive deps and drop ones already satisfied by the current state.
		transitive, ok := r.pruneSatisfiedTransitives(s, transitiveReqs(c, req.dev))
		if !ok {
			lastRejectedVersion = c.entry.Version
			continue
		}
		if !r.transitivesRemainViable(s, transitive, rest) {
			lastRejectedVersion = c.entry.Version
			continue
		}
		allReqs := append(transitive, rest...)

		if r.solve(s, allReqs) {
			return true
		}
		if r.terminalErr != nil {
			return false
		}

		// Backtrack: restore state for this decision.
		if hadResolved {
			s.resolved[req.name] = prevResolved
		} else {
			delete(s.resolved, req.name)
		}
		if hadConstraints {
			s.constraints[req.name] = prevConstraints
		} else {
			delete(s.constraints, req.name)
		}
		for i := len(rollbacks) - 1; i >= 0; i-- {
			rb := rollbacks[i]
			if rb.existed {
				s.providers[rb.name] = rb.prev
			} else {
				delete(s.providers, rb.name)
			}
		}
	}

	// All candidates exhausted.
	reason := fmt.Sprintf("no version of %s matching %s could be resolved", req.name, req.constraint)
	if lastRejectedVersion != "" {
		reason += fmt.Sprintf(" (tried up to %s)", lastRejectedVersion)
	}
	// Preserve a more specific conflict already discovered for this package at
	// the same recursion depth.
	if r.lastConflict == nil || r.lastConflict.packageName != req.name || r.lastConflict.depth != r.depth {
		r.recordConflict(req.name, req.constraint, reason)
	}

	return false
}

func (r *resolver) selectRequirement(s *state, reqs []requirement) (requirement, []requirement, []version.Constraint) {
	type aggregate struct {
		req         requirement
		constraints []version.Constraint
		firstIndex  int
	}

	aggregates := make(map[string]*aggregate, len(reqs))
	order := make([]string, 0, len(reqs))
	for i, req := range reqs {
		agg, ok := aggregates[req.name]
		if !ok {
			aggregates[req.name] = &aggregate{
				req:         req,
				constraints: []version.Constraint{req.constraint},
				firstIndex:  i,
			}
			order = append(order, req.name)
			continue
		}
		agg.constraints = appendConstraintsUnique(agg.constraints, []version.Constraint{req.constraint})
		agg.req.dev = agg.req.dev && req.dev
		agg.req.root = agg.req.root || req.root
		agg.req.deferred = agg.req.deferred || req.deferred
	}

	bestName := order[0]
	bestScore := int(^uint(0) >> 1)
	bestIndex := len(reqs)

	for _, name := range order {
		agg := aggregates[name]
		score := r.requirementScore(s, agg.req, agg.constraints)
		if score < bestScore || (score == bestScore && agg.firstIndex < bestIndex) {
			bestName = name
			bestScore = score
			bestIndex = agg.firstIndex
		}
	}

	selected := aggregates[bestName]
	rest := make([]requirement, 0, len(reqs)-1)
	for _, req := range reqs {
		if req.name == bestName {
			continue
		}
		rest = append(rest, req)
	}

	return selected.req, rest, selected.constraints
}

func (r *resolver) requirementScore(s *state, req requirement, pending []version.Constraint) int {
	if pkg.IsPlatformPackage(req.name) {
		return -1
	}
	if existing, ok := s.resolved[req.name]; ok {
		if matchesAll(existing.version, pending) {
			return -1
		}
		return 0
	}
	if prov, ok := s.providers[req.name]; ok {
		if existing, hasReal := s.resolved[prov.realName]; hasReal {
			pv, ok := r.providedVersion(existing.entry, req.name)
			if !ok || pv.any {
				return -1
			}
			if providedMatchesAll(pv, pending) {
				return -1
			}
			return 0
		}
		// A virtual package tied to an unresolved provider should not be selected
		// before that real provider has a chance to be decided.
		return 1 << 20
	}

	score := 1000
	if req.root {
		score -= 100
	}
	score -= len(pending) * 10
	score -= len(s.constraints[req.name]) * 10

	if cached, ok := r.cacheGet(req.name); ok {
		if cached.err != nil {
			return 0
		}
		constraints := append([]version.Constraint{}, pending...)
		constraints = appendConstraintsUnique(constraints, s.constraints[req.name])

		scanLimit := len(cached.candidates)
		if scanLimit > requirementScoreCandidateScanLimit {
			scanLimit = requirementScoreCandidateScanLimit
		}

		matching := 0
		for _, c := range cached.candidates[:scanLimit] {
			if matchesAll(c.version, constraints) {
				matching++
			}
		}

		// Packages with many dependencies are highly constraining — resolve
		// them early to prune the search space before their deps get locked
		// to conflicting versions by other branches. This prevents
		// combinatorial explosions on metapackages like drupal/core-recommended
		// that pin exact versions of 100+ transitive dependencies.
		// Only apply the penalty for packages with a significant number of deps
		// (threshold 10) to avoid disturbing resolution order for normal packages.
		depPenalty := 0
		if len(cached.candidates) > 0 {
			depCount := len(cached.candidates[0].dependencies)
			if depCount > 10 {
				depPenalty = depCount * 2
			}
		}

		if len(cached.candidates) > scanLimit {
			// requirementScore is a heuristic, not a correctness gate. Bounded
			// scanning prevents O(requirements*candidates*constraints) blowups on
			// large package universes while preserving candidate-count ordering.
			return matching + 1 - depPenalty
		}
		return matching - depPenalty
	}

	return score
}

func (r *resolver) buildError(s *state) error {
	if r.terminalErr != nil {
		return r.terminalErr
	}
	if r.lastConflict != nil {
		return fmt.Errorf("resolver: %s", r.lastConflict.reason)
	}
	return fmt.Errorf("resolver: failed to resolve dependencies")
}

func (r *resolver) checkContext() bool {
	if err := r.ctx.Err(); err != nil {
		if r.terminalErr == nil {
			r.terminalErr = fmt.Errorf("resolver: %w", err)
		}
		return false
	}
	return true
}

func (r *resolver) cacheGet(name string) (candidateCacheEntry, bool) {
	if r.versionCacheMu != nil {
		r.versionCacheMu.RLock()
		defer r.versionCacheMu.RUnlock()
	}
	entry, ok := r.versionCache[name]
	return entry, ok
}

func (r *resolver) cacheSet(name string, entry candidateCacheEntry) {
	if r.versionCacheMu != nil {
		r.versionCacheMu.Lock()
		defer r.versionCacheMu.Unlock()
	}
	r.versionCache[name] = entry
}

func transitiveReqs(c candidate, dev bool) []requirement {
	if len(c.dependencies) == 0 {
		return nil
	}
	reqs := make([]requirement, 0, len(c.dependencies))
	for _, dep := range c.dependencies {
		reqs = append(reqs, requirement{name: dep.name, constraint: dep.constraint, dev: dev})
	}
	// Visit sibling transitive requirements in a deterministic order that
	// surfaces missing packages before we recurse into unrelated siblings.
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].name > reqs[j].name })
	return reqs
}

func (r *resolver) getCandidates(name string, effectiveStability version.Stability) ([]candidate, error) {
	if !r.checkContext() {
		return nil, r.terminalErr
	}
	includeDev := effectiveStability == version.Dev
	if cached, ok := r.cacheGet(name); ok {
		if cached.err != nil && (cached.hasDev || !includeDev) {
			return nil, cached.err
		}
		if cached.hasDev || !includeDev {
			// Filter by effective stability (which may differ from global minimumStability
			// if the requirement has an @dev/@alpha/etc flag or is a dev-branch constraint).
			filtered := filterByStability(cached.candidates, effectiveStability)
			return preferLocked(r.filterCandidates(name, filtered), r.locked[name]), nil
		}
	}
	if r.progress != nil {
		r.progress(name)
	}

	versions, err := fetchPackageVersions(r.ctx, r.client, name, includeDev)
	if err != nil {
		if errors.Is(err, packagist.ErrPackageNotFound) {
			if entry, ok := r.lockedEntries[name]; ok {
				if v, parseErr := version.Parse(entry.Version); parseErr == nil {
					candidates := []candidate{{
						entry:        entry,
						version:      v,
						dependencies: parseDependencyRequirements(entry),
						conflicts:    parseConflictRequirements(entry),
					}}
					sortCandidates(candidates, r.preferStable)
					r.cacheSet(name, candidateCacheEntry{candidates: candidates, hasDev: includeDev})
					filtered := filterByStability(candidates, effectiveStability)
					return preferLocked(r.filterCandidates(name, filtered), r.locked[name]), nil
				}
			}
		}
		cachedErr := fmt.Errorf("resolver: fetch %s: %w", name, err)
		r.cacheSet(name, candidateCacheEntry{err: cachedErr, hasDev: includeDev})
		return nil, cachedErr
	}

	var candidates []candidate
	for _, entry := range versions {
		v, err := version.Parse(entry.Version)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			entry:        entry,
			version:      v,
			dependencies: parseDependencyRequirements(entry),
			conflicts:    parseConflictRequirements(entry),
		})
	}

	// If Packagist returned no versions, try to use locked entry (VCS-only packages).
	if len(candidates) == 0 {
		if entry, ok := r.lockedEntries[name]; ok {
			if v, err := version.Parse(entry.Version); err == nil {
				candidates = []candidate{{
					entry:        entry,
					version:      v,
					dependencies: parseDependencyRequirements(entry),
					conflicts:    parseConflictRequirements(entry),
				}}
			}
		}
	}

	// Sort descending by version (highest first).
	sortCandidates(candidates, r.preferStable)

	// Cache ALL versions (no stability filtering), then filter for this request.
	r.cacheSet(name, candidateCacheEntry{candidates: candidates, hasDev: includeDev})

	filtered := filterByStability(candidates, effectiveStability)
	return preferLocked(r.filterCandidates(name, filtered), r.locked[name]), nil
}

func fetchPackageVersions(ctx context.Context, client packagist.Fetcher, name string, includeDev bool) ([]packagist.VersionEntry, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: %s", packagist.ErrPackageNotFound, name)
	}
	if devClient, ok := client.(packagist.DevFetcher); ok {
		return devClient.GetPackageWithDev(ctx, name, includeDev)
	}
	return client.GetPackage(ctx, name)
}

func (r *resolver) candidateProviders(name string) []string {
	seen := make(map[string]struct{})

	if r.versionCacheMu != nil {
		r.versionCacheMu.RLock()
		defer r.versionCacheMu.RUnlock()
	}

	for pkgName, cached := range r.versionCache {
		if cached.err != nil {
			continue
		}
		for _, cand := range cached.candidates {
			if cand.entry.Provide[name] == "" && cand.entry.Replace[name] == "" {
				continue
			}
			if _, ok := seen[pkgName]; ok {
				continue
			}
			seen[pkgName] = struct{}{}
		}
	}

	providers := make([]string, 0, len(seen))
	for pkgName := range seen {
		providers = append(providers, pkgName)
	}
	sort.Strings(providers)
	return providers
}

// filterByStability returns candidates that meet the minimum stability requirement.
func filterByStability(candidates []candidate, minStability version.Stability) []candidate {
	if minStability == version.Dev {
		// Dev accepts everything, no filtering needed.
		return candidates
	}
	filtered := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.version.StabilityAtLeast(minStability) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func (r *resolver) filterCandidates(name string, candidates []candidate) []candidate {
	if fixedVersion, ok := r.fixed[name]; ok && fixedVersion != "" {
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if candidate.entry.Version == fixedVersion {
				filtered = append(filtered, candidate)
			}
		}
		candidates = filtered
	}

	if !r.hasRestriction {
		return candidates
	}
	if _, ok := r.restrictedPackages[name]; !ok {
		return candidates
	}

	filtered := candidates[:0]
	for _, candidate := range candidates {
		if r.restriction.Matches(candidate.version) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func mergePendingRequirements(req requirement, rest []requirement) (requirement, []requirement, []version.Constraint) {
	constraints := []version.Constraint{req.constraint}
	seen := map[string]struct{}{
		req.constraint.String(): {},
	}
	filtered := rest[:0]

	for _, other := range rest {
		if other.name != req.name {
			filtered = append(filtered, other)
			continue
		}
		key := other.constraint.String()
		if _, ok := seen[key]; !ok {
			constraints = append(constraints, other.constraint)
			seen[key] = struct{}{}
		}
		req.dev = req.dev && other.dev
		req.root = req.root || other.root
		req.deferred = req.deferred || other.deferred
	}

	return req, filtered, constraints
}

func hasPendingRootRequirement(reqs []requirement) bool {
	for _, req := range reqs {
		if req.root {
			return true
		}
	}
	return false
}

func appendConstraintsUnique(existing []version.Constraint, incoming []version.Constraint) []version.Constraint {
	if len(incoming) == 0 {
		return existing
	}

	seen := make(map[string]struct{}, len(existing)+len(incoming))
	for _, c := range existing {
		seen[c.String()] = struct{}{}
	}
	for _, c := range incoming {
		key := c.String()
		if _, ok := seen[key]; ok {
			continue
		}
		existing = append(existing, c)
		seen[key] = struct{}{}
	}
	return existing
}

func matchesAll(v version.Version, constraints []version.Constraint) bool {
	for _, c := range constraints {
		if !c.Matches(v) {
			return false
		}
	}
	return true
}

func providedMatchesAll(pv providedVersion, constraints []version.Constraint) bool {
	if pv.any || (!pv.hasValue && !pv.hasSet) {
		return true
	}
	if pv.hasValue {
		return matchesAll(pv.value, constraints)
	}
	for _, c := range constraints {
		if !pv.set.AllowsAny(version.ConstraintVersionSet(c)) {
			return false
		}
	}
	return true
}

func providedAllowsSet(pv providedVersion, set version.VersionSet) bool {
	if pv.any || (!pv.hasValue && !pv.hasSet) {
		return true
	}
	if pv.hasValue {
		return set.Contains(pv.value)
	}
	return pv.set.AllowsAny(set)
}

func (r *resolver) conflictsWithResolved(cand candidate, s *state) (string, bool) {
	if reason, ok := rootConflictReason(cand, r.rootConflicts); ok {
		return reason, true
	}
	for resolvedName, resolved := range s.resolved {
		if reason, ok := candidateConflictsResolved(cand, resolved); ok {
			return reason, true
		}
		if reason, ok := candidateConflictsResolved(candidate{
			entry:     resolved.entry,
			version:   resolved.version,
			conflicts: resolved.conflicts,
		}, resolvedEntry{
			entry:   cand.entry,
			version: cand.version,
		}); ok {
			return reason, true
		}

		if prov, ok := s.providers[cand.entry.Name]; ok && prov.realName == resolvedName {
			continue
		}
	}
	return "", false
}

func (r *resolver) requiresAlreadyResolvedConflict(c candidate, s *state) (string, bool) {
	for _, dep := range c.dependencies {
		depName := dep.name
		depConstraintStr := dep.raw
		constraint := dep.constraint
		if existing, ok := s.resolved[depName]; ok {
			if !constraint.Matches(existing.version) {
				return fmt.Sprintf("%s@%s requires %s %s but already resolved %s@%s", c.entry.Name, c.entry.Version, depName, depConstraintStr, depName, existing.entry.Version), true
			}
			continue
		}

		if prov, ok := s.providers[depName]; ok {
			existing, hasReal := s.resolved[prov.realName]
			if !hasReal {
				continue
			}
			pv, ok := r.providedVersion(existing.entry, depName)
			if !ok {
				continue
			}
			if !providedMatchesAll(pv, []version.Constraint{constraint}) {
				return fmt.Sprintf("%s@%s requires %s %s but %s provides %s", c.entry.Name, c.entry.Version, depName, depConstraintStr, prov.realName, depName), true
			}
			continue
		}

		if rs, ok := r.rootSatisfiers[depName]; ok && rs.hasValue && !constraint.Matches(rs.value) {
			return fmt.Sprintf("%s@%s requires %s %s but root package provides %s", c.entry.Name, c.entry.Version, depName, depConstraintStr, depName), true
		}
	}

	return "", false
}

func (r *resolver) transitivesRemainViable(s *state, transitive []requirement, rest []requirement) bool {
	if len(transitive) == 0 {
		return true
	}

	pendingByName := make(map[string][]version.Constraint, len(rest))
	for _, req := range rest {
		pendingByName[req.name] = appendConstraintsUnique(pendingByName[req.name], []version.Constraint{req.constraint})
	}

	for _, dep := range transitive {
		constraints := []version.Constraint{dep.constraint}
		constraints = appendConstraintsUnique(constraints, s.constraints[dep.name])
		constraints = appendConstraintsUnique(constraints, pendingByName[dep.name])
		if !r.requirementHasPossibleCandidate(s, dep.name, constraints) {
			r.recordConflict(dep.name, dep.constraint,
				fmt.Sprintf("no version of %s can satisfy all constraints", dep.name))
			return false
		}
	}

	return true
}

func (r *resolver) pruneSatisfiedTransitives(s *state, transitive []requirement) ([]requirement, bool) {
	if len(transitive) == 0 {
		return nil, true
	}
	pruned := make([]requirement, 0, len(transitive))
	for _, dep := range transitive {
		if existing, ok := s.resolved[dep.name]; ok {
			if !dep.constraint.Matches(existing.version) {
				return nil, false
			}
			continue
		}
		if prov, ok := s.providers[dep.name]; ok {
			existing, hasReal := s.resolved[prov.realName]
			if hasReal {
				pv, ok := r.providedVersion(existing.entry, dep.name)
				if ok && providedMatchesAll(pv, []version.Constraint{dep.constraint}) {
					continue
				}
				if ok {
					return nil, false
				}
			}
		}
		if rs, ok := r.rootSatisfiers[dep.name]; ok {
			if rs.any {
				continue
			}
			if rs.hasValue {
				if !dep.constraint.Matches(rs.value) {
					return nil, false
				}
				continue
			}
		}
		pruned = append(pruned, dep)
	}
	return pruned, true
}

func (r *resolver) requirementHasPossibleCandidate(s *state, name string, constraints []version.Constraint) bool {
	return r.requirementHasPossibleCandidateWithFetch(s, name, constraints, false)
}

func (r *resolver) requirementHasPossibleCandidateWithFetch(s *state, name string, constraints []version.Constraint, fetchIfMissing bool) bool {
	if existing, ok := s.resolved[name]; ok {
		return matchesAll(existing.version, constraints)
	}

	if prov, ok := s.providers[name]; ok {
		existing, hasReal := s.resolved[prov.realName]
		if !hasReal {
			return true
		}
		pv, ok := r.providedVersion(existing.entry, name)
		if !ok {
			return true
		}
		return providedMatchesAll(pv, constraints)
	}

	if r.rootSatisfies(name, constraints) {
		return true
	}

	cached, ok := r.cacheGet(name)
	if (!ok || cached.err != nil || len(cached.candidates) == 0) && fetchIfMissing {
		effectiveStability := r.minimumStability
		for _, c := range constraints {
			s := c.EffectiveStability(r.minimumStability)
			if s < effectiveStability {
				effectiveStability = s
			}
		}
		candidates, err := r.getCandidates(name, effectiveStability)
		if err != nil {
			// A missing package can still be satisfied later via provide/replace.
			return errors.Is(err, packagist.ErrPackageNotFound)
		}
		return r.candidateSliceHasPossibleCandidate(s, name, constraints, candidates)
	}
	if !ok || cached.err != nil || len(cached.candidates) == 0 {
		// Unknown at this branch (e.g. provided by an unresolved package later):
		// do not prune unless impossibility is proven.
		return true
	}

	effectiveStability := r.minimumStability
	for _, c := range constraints {
		s := c.EffectiveStability(r.minimumStability)
		if s < effectiveStability {
			effectiveStability = s
		}
	}

	candidates := filterByStability(cached.candidates, effectiveStability)
	candidates = preferLocked(r.filterCandidates(name, candidates), r.locked[name])
	return r.candidateSliceHasPossibleCandidate(s, name, constraints, candidates)
}

func (r *resolver) candidateSliceHasPossibleCandidate(s *state, name string, constraints []version.Constraint, candidates []candidate) bool {
	for _, c := range candidates {
		if !matchesAll(c.version, constraints) {
			continue
		}
		if _, ok := r.conflictsWithResolved(c, s); ok {
			continue
		}
		if _, ok := r.requiresAlreadyResolvedConflict(c, s); ok {
			continue
		}
		return true
	}

	return false
}

func parseDependencyRequirements(entry packagist.VersionEntry) []dependencyRequirement {
	nonPlatform := entry.NonPlatformRequire()
	if len(nonPlatform) == 0 {
		return nil
	}
	deps := make([]dependencyRequirement, 0, len(nonPlatform))
	for name, raw := range nonPlatform {
		c, err := version.ParseConstraint(raw)
		if err != nil {
			continue
		}
		deps = append(deps, dependencyRequirement{name: name, constraint: c, raw: raw})
	}
	return deps
}

func parseConflictRequirements(entry packagist.VersionEntry) []dependencyRequirement {
	if len(entry.Conflict) == 0 {
		return nil
	}
	conflicts := make([]dependencyRequirement, 0, len(entry.Conflict))
	for name, raw := range entry.Conflict {
		c, err := version.ParseConstraint(raw)
		if err != nil {
			continue
		}
		conflicts = append(conflicts, dependencyRequirement{name: name, constraint: c, raw: raw})
	}
	return conflicts
}

func (r *resolver) rootSatisfies(name string, constraints []version.Constraint) bool {
	rs, ok := r.rootSatisfiers[name]
	if !ok {
		return false
	}
	if rs.any {
		return true
	}
	if !rs.hasValue {
		return false
	}
	return matchesAll(rs.value, constraints)
}

func candidateConflictsResolved(a candidate, b resolvedEntry) (string, bool) {
	for _, conflict := range a.conflicts {
		if conflict.name != b.entry.Name {
			continue
		}
		if !conflict.constraint.Matches(b.version) {
			continue
		}
		return fmt.Sprintf("%s@%s conflicts with already resolved %s@%s via constraint %s", a.entry.Name, a.entry.Version, b.entry.Name, b.entry.Version, conflict.raw), true
	}
	return "", false
}

func rootConflictReason(candidate candidate, conflicts map[string]version.Constraint) (string, bool) {
	constraint, ok := conflicts[candidate.entry.Name]
	if !ok {
		return "", false
	}
	if !constraint.Matches(candidate.version) {
		return "", false
	}

	return fmt.Sprintf("root package conflicts with %s@%s via constraint %s", candidate.entry.Name, candidate.entry.Version, constraint.String()), true
}

func buildRootSatisfiers(rootProvide, rootReplace map[string]string) map[string]rootSatisfier {
	out := make(map[string]rootSatisfier, len(rootProvide)+len(rootReplace))
	for name, raw := range rootReplace {
		out[name] = parseRootSatisfier(raw)
	}
	for name, raw := range rootProvide {
		out[name] = parseRootSatisfier(raw)
	}
	return out
}

func parseRootSatisfier(raw string) rootSatisfier {
	if raw == "*" {
		return rootSatisfier{any: true}
	}
	v, err := version.Parse(raw)
	if err != nil {
		return rootSatisfier{}
	}
	return rootSatisfier{hasValue: true, value: v}
}

func buildRootConflicts(conflicts map[string]string) map[string]version.Constraint {
	out := make(map[string]version.Constraint, len(conflicts))
	for name, raw := range conflicts {
		if raw == "" {
			continue
		}
		c, err := version.ParseConstraint(raw)
		if err != nil {
			continue
		}
		out[name] = c
	}
	return out
}

func (r *resolver) providedVersion(entry packagist.VersionEntry, virtual string) (providedVersion, bool) {
	key := entry.Name + "\x00" + entry.Version + "\x00" + virtual
	if pv, ok := r.providedVersionCache[key]; ok {
		return pv, true
	}
	raw := entry.Provide[virtual]
	if raw == "" {
		raw = entry.Replace[virtual]
	}
	if raw == "" {
		return providedVersion{}, false
	}
	if raw == "*" {
		pv := providedVersion{any: true}
		r.providedVersionCache[key] = pv
		return pv, true
	}
	if raw == "self.version" {
		raw = entry.Version
	}
	if c, err := version.ParseConstraint(raw); err == nil {
		pv := providedVersion{
			hasSet: true,
			set:    version.ConstraintVersionSet(c),
		}
		if v, err := version.Parse(raw); err == nil {
			pv.hasValue = true
			pv.value = v
		}
		r.providedVersionCache[key] = pv
		return pv, true
	}
	v, err := version.Parse(raw)
	if err != nil {
		pv := providedVersion{}
		r.providedVersionCache[key] = pv
		return pv, true
	}
	pv := providedVersion{hasValue: true, value: v}
	r.providedVersionCache[key] = pv
	return pv, true
}

func parseStability(s string) version.Stability {
	switch strings.ToLower(s) {
	case "dev":
		return version.Dev
	case "alpha":
		return version.Alpha
	case "beta":
		return version.Beta
	case "rc":
		return version.RC
	case "stable", "":
		return version.Stable
	default:
		return version.Stable
	}
}

func stabilityString(s version.Stability) string {
	switch s {
	case version.Dev:
		return "dev"
	case version.Alpha:
		return "alpha"
	case version.Beta:
		return "beta"
	case version.RC:
		return "RC"
	case version.Stable:
		return "stable"
	default:
		return "stable"
	}
}
