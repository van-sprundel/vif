package resolver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
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
func Resolve(ctx context.Context, cj *composer.ComposerJSON, client *packagist.Client) ([]ResolvedPackage, error) {
	r := &resolver{
		ctx:              ctx,
		client:           client,
		minimumStability: parseStability(cj.MinimumStability),
		versionCache:     make(map[string][]candidate),
	}

	// Collect all root requirements (prod + dev).
	var reqs []requirement
	for name, constraint := range cj.NonPlatformRequire() {
		c, err := version.ParseConstraint(constraint)
		if err != nil {
			return nil, fmt.Errorf("resolver: parse constraint %q for %s: %w", constraint, name, err)
		}
		reqs = append(reqs, requirement{name: name, constraint: c, dev: false})
	}
	for name, constraint := range cj.NonPlatformRequireDev() {
		c, err := version.ParseConstraint(constraint)
		if err != nil {
			return nil, fmt.Errorf("resolver: parse constraint %q for %s: %w", constraint, name, err)
		}
		reqs = append(reqs, requirement{name: name, constraint: c, dev: true})
	}

	// Sort requirements for deterministic resolution order.
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].name < reqs[j].name })

	state := newState()
	if !r.solve(state, reqs) {
		return nil, fmt.Errorf("resolver: failed to resolve dependencies")
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
}

type resolvedEntry struct {
	entry packagist.VersionEntry
	dev   bool
}

type candidate struct {
	entry   packagist.VersionEntry
	version version.Version
}

// state is the resolution state, cloneable for backtracking.
type state struct {
	resolved    map[string]resolvedEntry
	constraints map[string][]version.Constraint
}

func newState() *state {
	return &state{
		resolved:    make(map[string]resolvedEntry),
		constraints: make(map[string][]version.Constraint),
	}
}

func (s *state) clone() *state {
	c := &state{
		resolved:    make(map[string]resolvedEntry, len(s.resolved)),
		constraints: make(map[string][]version.Constraint, len(s.constraints)),
	}
	for k, v := range s.resolved {
		c.resolved[k] = v
	}
	for k, v := range s.constraints {
		cs := make([]version.Constraint, len(v))
		copy(cs, v)
		c.constraints[k] = cs
	}
	return c
}

type resolver struct {
	ctx              context.Context
	client           *packagist.Client
	minimumStability version.Stability
	versionCache     map[string][]candidate
}

// solve tries to satisfy all requirements using recursive backtracking.
// Returns true if a solution was found, updating state in place.
func (r *resolver) solve(s *state, reqs []requirement) bool {
	if len(reqs) == 0 {
		return true
	}

	req := reqs[0]
	rest := reqs[1:]

	// If already resolved, check compatibility.
	if existing, ok := s.resolved[req.name]; ok {
		v, err := version.Parse(existing.entry.Version)
		if err != nil {
			return false
		}
		if req.constraint.Matches(v) {
			// Compatible. Track constraint and continue.
			s.constraints[req.name] = append(s.constraints[req.name], req.constraint)
			if existing.dev && !req.dev {
				existing.dev = false
				s.resolved[req.name] = existing
			}
			return r.solve(s, rest)
		}
		// Not compatible — caller must backtrack.
		return false
	}

	// Fetch candidates.
	candidates, err := r.getCandidates(req.name)
	if err != nil {
		return false
	}

	// Try each candidate version (highest first).
	for _, c := range candidates {
		if !req.constraint.Matches(c.version) {
			continue
		}
		// Also check all accumulated constraints from other resolved packages.
		if !matchesAll(c.version, s.constraints[req.name]) {
			continue
		}

		// Try this candidate: snapshot state, resolve transitive deps.
		snapshot := s.clone()
		s.resolved[req.name] = resolvedEntry{entry: c.entry, dev: req.dev}
		s.constraints[req.name] = append(s.constraints[req.name], req.constraint)

		// Gather transitive deps.
		transitive := transitiveReqs(c.entry, req.dev)
		allReqs := append(transitive, rest...)

		if r.solve(s, allReqs) {
			return true
		}

		// Backtrack: restore state.
		*s = *snapshot
	}

	return false
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
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].name < reqs[j].name })
	return reqs
}

func (r *resolver) getCandidates(name string) ([]candidate, error) {
	if cached, ok := r.versionCache[name]; ok {
		return cached, nil
	}

	versions, err := r.client.GetPackage(r.ctx, name)
	if err != nil {
		return nil, fmt.Errorf("resolver: fetch %s: %w", name, err)
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

	r.versionCache[name] = candidates
	return candidates, nil
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
