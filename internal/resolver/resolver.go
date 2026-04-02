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

// Resolve resolves all dependencies from a composer.json using the given Packagist client.
// Returns a flat list of all resolved packages (including transitive dependencies).
func Resolve(ctx context.Context, cj *composer.ComposerJSON, client packagist.Fetcher) ([]ResolvedPackage, error) {
	return ResolveWithProgress(ctx, cj, client, nil)
}

// ResolveWithProgress resolves dependencies and reports each unique package lookup.
func ResolveWithProgress(ctx context.Context, cj *composer.ComposerJSON, client packagist.Fetcher, progress func(string)) ([]ResolvedPackage, error) {
	r := &resolver{
		ctx:              ctx,
		client:           client,
		minimumStability: parseStability(cj.MinimumStability),
		versionCache:     make(map[string]candidateCacheEntry),
		progress:         progress,
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
	ctx              context.Context
	client           packagist.Fetcher
	minimumStability version.Stability
	versionCache     map[string]candidateCacheEntry
	lastConflict     *conflict
	terminalErr      error
	progress         func(string)
	depth            int
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

	req := reqs[0]
	rest := reqs[1:]
	req, rest, pendingConstraints := mergePendingRequirements(req, rest)

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

	// Fetch candidates from packagist.
	candidates, err := r.getCandidates(req.name)
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
				fmt.Sprintf("no versions of %s found matching stability %s", req.name, stabilityString(r.minimumStability)))
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

func (r *resolver) getCandidates(name string) ([]candidate, error) {
	if !r.checkContext() {
		return nil, r.terminalErr
	}
	if cached, ok := r.versionCache[name]; ok {
		return cached.candidates, cached.err
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
		if !v.StabilityAtLeast(r.minimumStability) {
			continue
		}
		candidates = append(candidates, candidate{entry: entry, version: v})
	}

	// Sort descending by version (highest first).
	sort.Slice(candidates, func(i, j int) bool {
		return version.Compare(candidates[i].version, candidates[j].version) > 0
	})

	r.versionCache[name] = candidateCacheEntry{candidates: candidates}
	return candidates, nil
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
