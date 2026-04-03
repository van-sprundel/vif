package resolver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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

// Options controls solver behavior for update-like flows.
type Options struct {
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
	for name := range cj.NonPlatformRequireDev() {
		rootNames = append(rootNames, name)
	}

	// Prefetch all reachable metadata in parallel before the sequential solve
	// phase, so getCandidates hits the cache for every package.
	prefetched := prefetchMetadata(ctx, client, rootNames, progress)

	versionCache := make(map[string]candidateCacheEntry, len(prefetched))
	populateVersionCache(versionCache, prefetched, cj.PreferStable)

	// Inject locked entries for packages with no Packagist versions (VCS-only packages).
	// This allows the resolver to keep them from the lockfile instead of failing.
	injectLockedEntries(versionCache, prefetched, opts.LockedEntries, cj.PreferStable)

	r := &resolver{
		ctx:              ctx,
		client:           client,
		minimumStability: minimumStability,
		preferStable:     cj.PreferStable,
		locked:           opts.Locked,
		lockedEntries:    opts.LockedEntries,
		rootProvide:      cj.Provide,
		rootReplace:      cj.Replace,
		rootConflict:     cj.Conflict,
		versionCache:     versionCache,
		// progress is nil during solve to avoid double-reporting; all lookups
		// were already reported during the prefetch pass above.
		progress: nil,
	}
	if opts.Restriction != "" && len(opts.RestrictedPackages) > 0 {
		c, err := version.ParseConstraint(opts.Restriction)
		if err != nil {
			return nil, fmt.Errorf("resolver: parse restriction %q: %w", opts.Restriction, err)
		}
		r.restriction = c
		r.hasRestriction = true
		r.restrictedPackages = opts.RestrictedPackages
	}

	// Collect all root requirements (prod + dev).
	var reqs []requirement
	for name, constraint := range cj.NonPlatformRequire() {
		c, err := version.ParseConstraint(constraint)
		if err != nil {
			return nil, fmt.Errorf("resolver: parse constraint %q for %s: %w", constraint, name, err)
		}
		reqs = append(reqs, requirement{name: name, constraint: c, dev: false, root: true})
	}
	for name, constraint := range cj.NonPlatformRequireDev() {
		c, err := version.ParseConstraint(constraint)
		if err != nil {
			return nil, fmt.Errorf("resolver: parse constraint %q for %s: %w", constraint, name, err)
		}
		reqs = append(reqs, requirement{name: name, constraint: c, dev: true, root: true})
	}

	// Sort requirements for deterministic resolution order.
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].name < reqs[j].name })

	state := newState()
	if !r.solve(state, reqs) {
		return nil, r.buildError(state)
	}

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
	entry packagist.VersionEntry
	dev   bool
}

type candidate struct {
	entry   packagist.VersionEntry
	version version.Version
}

type candidateCacheEntry struct {
	candidates []candidate
	err        error
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
	ctx                context.Context
	client             packagist.Fetcher
	minimumStability   version.Stability
	preferStable       bool
	locked             map[string]string
	lockedEntries      map[string]packagist.VersionEntry
	rootProvide        map[string]string
	rootReplace        map[string]string
	rootConflict       map[string]string
	restriction        version.Constraint
	hasRestriction     bool
	restrictedPackages map[string]struct{}
	versionCache       map[string]candidateCacheEntry
	lastConflict       *conflict
	terminalErr        error
	progress           func(string)
	depth              int
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

	// Skip platform packages that slipped through.
	if pkg.IsPlatformPackage(req.name) {
		return r.solve(s, rest)
	}

	// If already resolved, check compatibility.
	if existing, ok := s.resolved[req.name]; ok {
		v, err := version.Parse(existing.entry.Version)
		if err != nil {
			return false
		}
		if matchesAll(v, pendingConstraints) {
			// Compatible. Track constraint and continue.
			s.constraints[req.name] = appendConstraintsUnique(s.constraints[req.name], pendingConstraints)
			if existing.dev && !req.dev {
				existing.dev = false
				s.resolved[req.name] = existing
			}
			return r.solve(s, rest)
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
			provVersion := existing.entry.Provide[req.name]
			if provVersion == "" {
				provVersion = existing.entry.Replace[req.name]
			}
			if provVersion != "" && provVersion != "*" {
				v, err := version.Parse(provVersion)
				if err == nil && !matchesAll(v, pendingConstraints) {
					r.recordConflict(req.name, req.constraint,
						fmt.Sprintf("%s provides %s@%s which does not match %s", prov.realName, req.name, provVersion, req.constraint))
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
			if !req.deferred && hasPendingRootRequirement(rest) {
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
		if conflictReason, ok := r.conflictsWithResolved(c.entry, s); ok {
			lastRejectedVersion = c.entry.Version
			r.recordConflict(req.name, req.constraint, conflictReason)
			continue
		}

		// Try this candidate: snapshot state, resolve transitive deps.
		snapshot := s.clone()
		s.resolved[req.name] = resolvedEntry{entry: c.entry, dev: req.dev}
		s.constraints[req.name] = appendConstraintsUnique(s.constraints[req.name], pendingConstraints)
		s.registerProviders(c.entry)

		// Gather transitive deps.
		transitive := transitiveReqs(c.entry, req.dev)
		allReqs := append(transitive, rest...)

		if r.solve(s, allReqs) {
			return true
		}
		if r.terminalErr != nil {
			return false
		}

		// Backtrack: restore state.
		*s = *snapshot
	}

	// All candidates exhausted.
	reason := fmt.Sprintf("no version of %s matching %s could be resolved", req.name, req.constraint)
	if lastRejectedVersion != "" {
		reason += fmt.Sprintf(" (tried up to %s)", lastRejectedVersion)
	}
	r.recordConflict(req.name, req.constraint, reason)

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
		v, err := version.Parse(existing.entry.Version)
		if err != nil {
			return 0
		}
		if matchesAll(v, pending) {
			return -1
		}
		return 0
	}
	if prov, ok := s.providers[req.name]; ok {
		if existing, hasReal := s.resolved[prov.realName]; hasReal {
			provVersion := existing.entry.Provide[req.name]
			if provVersion == "" {
				provVersion = existing.entry.Replace[req.name]
			}
			if provVersion == "" || provVersion == "*" {
				return -1
			}
			v, err := version.Parse(provVersion)
			if err == nil && matchesAll(v, pending) {
				return -1
			}
			return 0
		}
	}

	score := 1000
	if req.root {
		score -= 100
	}
	score -= len(pending) * 10
	score -= len(s.constraints[req.name]) * 10

	if cached, ok := r.versionCache[req.name]; ok {
		if cached.err != nil {
			return 0
		}
		constraints := append([]version.Constraint{}, pending...)
		constraints = appendConstraintsUnique(constraints, s.constraints[req.name])

		matching := 0
		for _, c := range cached.candidates {
			if matchesAll(c.version, constraints) {
				matching++
			}
		}
		return matching
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

func transitiveReqs(entry packagist.VersionEntry, dev bool) []requirement {
	nonPlatform := entry.NonPlatformRequire()
	if len(nonPlatform) == 0 {
		return nil
	}
	reqs := make([]requirement, 0, len(nonPlatform))
	for name, constraintStr := range nonPlatform {
		c, err := version.ParseConstraint(constraintStr)
		if err != nil {
			continue
		}
		reqs = append(reqs, requirement{name: name, constraint: c, dev: dev})
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
	if cached, ok := r.versionCache[name]; ok {
		if cached.err != nil {
			return nil, cached.err
		}
		// Filter by effective stability (which may differ from global minimumStability
		// if the requirement has an @dev/@alpha/etc flag or is a dev-branch constraint).
		filtered := filterByStability(cached.candidates, effectiveStability)
		return preferLocked(r.filterCandidates(name, filtered), r.locked[name]), nil
	}
	if r.progress != nil {
		r.progress(name)
	}

	versions, err := r.client.GetPackage(r.ctx, name)
	if err != nil {
		cachedErr := fmt.Errorf("resolver: fetch %s: %w", name, err)
		r.versionCache[name] = candidateCacheEntry{err: cachedErr}
		return nil, cachedErr
	}

	var candidates []candidate
	for _, entry := range versions {
		v, err := version.Parse(entry.Version)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{entry: entry, version: v})
	}

	// If Packagist returned no versions, try to use locked entry (VCS-only packages).
	if len(candidates) == 0 {
		if entry, ok := r.lockedEntries[name]; ok {
			if v, err := version.Parse(entry.Version); err == nil {
				candidates = []candidate{{entry: entry, version: v}}
			}
		}
	}

	// Sort descending by version (highest first).
	sortCandidates(candidates, r.preferStable)

	// Cache ALL versions (no stability filtering), then filter for this request.
	r.versionCache[name] = candidateCacheEntry{candidates: candidates}

	filtered := filterByStability(candidates, effectiveStability)
	return preferLocked(r.filterCandidates(name, filtered), r.locked[name]), nil
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

func (r *resolver) conflictsWithResolved(candidate packagist.VersionEntry, s *state) (string, bool) {
	if reason, ok := rootConflictReason(candidate, r.rootConflict); ok {
		return reason, true
	}
	for resolvedName, resolved := range s.resolved {
		if reason, ok := packageConflictReason(candidate, resolved.entry); ok {
			return reason, true
		}
		if reason, ok := packageConflictReason(resolved.entry, candidate); ok {
			return reason, true
		}

		if prov, ok := s.providers[candidate.Name]; ok && prov.realName == resolvedName {
			continue
		}
	}
	return "", false
}

func (r *resolver) rootSatisfies(name string, constraints []version.Constraint) bool {
	relVersion := r.rootProvide[name]
	if relVersion == "" {
		relVersion = r.rootReplace[name]
	}
	if relVersion == "" {
		return false
	}
	if relVersion == "*" {
		return true
	}

	v, err := version.Parse(relVersion)
	if err != nil {
		return false
	}
	return matchesAll(v, constraints)
}

func packageConflictReason(a, b packagist.VersionEntry) (string, bool) {
	constraintStr, ok := a.Conflict[b.Name]
	if !ok || constraintStr == "" {
		return "", false
	}

	constraint, err := version.ParseConstraint(constraintStr)
	if err != nil {
		return "", false
	}

	v, err := version.Parse(b.Version)
	if err != nil {
		return "", false
	}

	if !constraint.Matches(v) {
		return "", false
	}

	return fmt.Sprintf("%s@%s conflicts with already resolved %s@%s via constraint %s", a.Name, a.Version, b.Name, b.Version, constraintStr), true
}

func rootConflictReason(candidate packagist.VersionEntry, conflicts map[string]string) (string, bool) {
	constraintStr, ok := conflicts[candidate.Name]
	if !ok || constraintStr == "" {
		return "", false
	}

	constraint, err := version.ParseConstraint(constraintStr)
	if err != nil {
		return "", false
	}

	v, err := version.Parse(candidate.Version)
	if err != nil {
		return "", false
	}

	if !constraint.Matches(v) {
		return "", false
	}

	return fmt.Sprintf("root package conflicts with %s@%s via constraint %s", candidate.Name, candidate.Version, constraintStr), true
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
