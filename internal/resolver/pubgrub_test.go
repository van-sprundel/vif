package resolver

import (
	"context"
	"testing"

	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/version"
)

type devAwareFetcher struct {
	stableCalls int
	devCalls    int
}

func (f *devAwareFetcher) GetPackage(ctx context.Context, name string) ([]packagist.VersionEntry, error) {
	return f.GetPackageWithDev(ctx, name, true)
}

func (f *devAwareFetcher) GetPackageWithDev(_ context.Context, name string, includeDev bool) ([]packagist.VersionEntry, error) {
	f.stableCalls++
	versions := []packagist.VersionEntry{{Name: name, Version: "1.0.0"}}
	if includeDev {
		f.devCalls++
		versions = append(versions, packagist.VersionEntry{Name: name, Version: "dev-main"})
	}
	return versions, nil
}

func TestPubGrubDecidePicksMostConstrainedPendingPackage(t *testing.T) {
	parse := func(raw string) version.Version {
		t.Helper()
		v, err := version.Parse(raw)
		if err != nil {
			t.Fatalf("parse version %q: %v", raw, err)
		}
		return v
	}
	cand := func(name, raw string) candidate {
		return candidate{
			entry:   packagist.VersionEntry{Name: name, Version: raw},
			version: parse(raw),
		}
	}

	r := &resolver{
		ctx:              context.Background(),
		minimumStability: version.Stable,
		versionCache: map[string]candidateCacheEntry{
			"acme/a": {candidates: []candidate{
				cand("acme/a", "3.0.0"),
				cand("acme/a", "2.0.0"),
				cand("acme/a", "1.0.0"),
			}},
			"acme/b": {candidates: []candidate{
				cand("acme/b", "1.0.0"),
			}},
		},
		heuristicCache: map[string]candidateCacheEntry{
			"acme/a": {candidates: []candidate{
				cand("acme/a", "3.0.0"),
				cand("acme/a", "2.0.0"),
				cand("acme/a", "1.0.0"),
			}},
			"acme/b": {candidates: []candidate{
				cand("acme/b", "1.0.0"),
			}},
		},
	}
	pg := &pubGrubSolver{
		r:               r,
		s:               newState(),
		solution:        newPGPartialSolution(),
		incompatByPkg:   make(map[string][]*pgIncompatibility),
		pending:         map[string]pgPendingMeta{"acme/a": {}, "acme/b": {}},
		minStabilityByP: make(map[string]version.Stability),
		candidateSets:   make(map[string]version.VersionSet),
		conflictPkgs:    make(map[string]struct{}),
	}

	var queue []string
	decided, missingOnly, unsat := pg.decide(&queue)
	if !decided {
		t.Fatalf("decide() = false, missing=%q unsat=%q", missingOnly, unsat)
	}
	if len(pg.decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(pg.decisions))
	}
	if got := pg.decisions[0].pkg; got != "acme/b" {
		t.Fatalf("decided %s, want acme/b", got)
	}
}

func TestRequirementScoreIgnoresSharedPrefetchCacheUntilSolverUsesPackage(t *testing.T) {
	parse := func(raw string) version.Version {
		t.Helper()
		v, err := version.Parse(raw)
		if err != nil {
			t.Fatalf("parse version %q: %v", raw, err)
		}
		return v
	}

	cand := candidate{
		entry:   packagist.VersionEntry{Name: "acme/a", Version: "1.0.0"},
		version: parse("1.0.0"),
	}

	r := &resolver{
		ctx:              context.Background(),
		minimumStability: version.Stable,
		versionCache: map[string]candidateCacheEntry{
			"acme/a": {candidates: []candidate{cand}},
		},
		heuristicCache: make(map[string]candidateCacheEntry),
	}

	scoreWithoutSolverTouch := r.requirementScore(newState(), requirement{name: "acme/a", root: true}, nil)
	if scoreWithoutSolverTouch != 900 {
		t.Fatalf("score without solver touch = %d, want 900", scoreWithoutSolverTouch)
	}

	r.heuristicSet("acme/a", candidateCacheEntry{candidates: []candidate{cand}})
	scoreAfterSolverTouch := r.requirementScore(newState(), requirement{name: "acme/a", root: true}, nil)
	if scoreAfterSolverTouch != 1 {
		t.Fatalf("score after solver touch = %d, want 1", scoreAfterSolverTouch)
	}
}

func TestPubGrubDecideUsesResolvedProviderForMissingVirtual(t *testing.T) {
	parse := func(raw string) version.Version {
		t.Helper()
		v, err := version.Parse(raw)
		if err != nil {
			t.Fatalf("parse version %q: %v", raw, err)
		}
		return v
	}

	virtualName := "zz/virtual-contract"
	providerEntry := packagist.VersionEntry{
		Name:    "acme/provider",
		Version: "1.0.0",
		Provide: map[string]string{virtualName: "1.0.0"},
	}

	r := &resolver{
		ctx:                  context.Background(),
		minimumStability:     version.Stable,
		versionCache:         map[string]candidateCacheEntry{},
		providedVersionCache: make(map[string]providedVersion),
	}
	r.versionCache[providerEntry.Name] = candidateCacheEntry{
		candidates: []candidate{{
			entry:   providerEntry,
			version: parse("1.0.0"),
		}},
	}

	pg := &pubGrubSolver{
		r:               r,
		s:               newState(),
		solution:        newPGPartialSolution(),
		incompatByPkg:   make(map[string][]*pgIncompatibility),
		pending:         map[string]pgPendingMeta{virtualName: {}},
		minStabilityByP: make(map[string]version.Stability),
		candidateSets:   make(map[string]version.VersionSet),
		conflictPkgs:    make(map[string]struct{}),
	}
	pg.s.resolved[providerEntry.Name] = resolvedEntry{
		entry:   providerEntry,
		version: parse("1.0.0"),
	}

	var queue []string
	decided, missingOnly, unsat := pg.decide(&queue)
	if !decided {
		t.Fatalf("decide() = false, missing=%q unsat=%q", missingOnly, unsat)
	}
	if missingOnly != "" || unsat != "" {
		t.Fatalf("unexpected decide result: missing=%q unsat=%q", missingOnly, unsat)
	}

	prov, ok := pg.s.providers[virtualName]
	if !ok {
		t.Fatalf("missing provider registration for %s", virtualName)
	}
	if prov.realName != providerEntry.Name {
		t.Fatalf("provider realName=%q, want %q", prov.realName, providerEntry.Name)
	}
}

func TestPubGrubDecideDefersMissingVirtualWhenProviderPending(t *testing.T) {
	parse := func(raw string) version.Version {
		t.Helper()
		v, err := version.Parse(raw)
		if err != nil {
			t.Fatalf("parse version %q: %v", raw, err)
		}
		return v
	}

	virtualName := "zz/virtual-contract"
	providerName := "acme/provider"
	providerEntry := packagist.VersionEntry{
		Name:    providerName,
		Version: "1.0.0",
		Provide: map[string]string{virtualName: "1.0.0"},
	}

	r := &resolver{
		ctx:                  context.Background(),
		minimumStability:     version.Stable,
		versionCache:         map[string]candidateCacheEntry{},
		providedVersionCache: make(map[string]providedVersion),
	}
	r.versionCache[providerName] = candidateCacheEntry{
		candidates: []candidate{{
			entry:   providerEntry,
			version: parse("1.0.0"),
		}},
	}

	pg := &pubGrubSolver{
		r:               r,
		s:               newState(),
		solution:        newPGPartialSolution(),
		incompatByPkg:   make(map[string][]*pgIncompatibility),
		pending:         map[string]pgPendingMeta{virtualName: {}, providerName: {}},
		minStabilityByP: make(map[string]version.Stability),
		candidateSets:   make(map[string]version.VersionSet),
		conflictPkgs:    make(map[string]struct{}),
	}

	var queue []string
	decided, missingOnly, unsat := pg.decide(&queue)
	if !decided {
		t.Fatalf("decide() = false, missing=%q unsat=%q", missingOnly, unsat)
	}
	if missingOnly != "" || unsat != "" {
		t.Fatalf("unexpected decide result: missing=%q unsat=%q", missingOnly, unsat)
	}

	prov, ok := pg.s.providers[virtualName]
	if !ok {
		t.Fatalf("missing provider registration for %s", virtualName)
	}
	if prov.realName != providerName {
		t.Fatalf("provider realName=%q, want %q", prov.realName, providerName)
	}
}

func TestPubGrubDecidePrefersRealProviderOverVirtualPlaceholder(t *testing.T) {
	parse := func(raw string) version.Version {
		t.Helper()
		v, err := version.Parse(raw)
		if err != nil {
			t.Fatalf("parse version %q: %v", raw, err)
		}
		return v
	}

	virtualName := "zz/virtual-contract"
	providerName := "acme/provider"
	providerEntry := packagist.VersionEntry{
		Name:    providerName,
		Version: "1.0.0",
		Provide: map[string]string{virtualName: "1.0.0"},
	}

	r := &resolver{
		ctx:                  context.Background(),
		minimumStability:     version.Stable,
		versionCache:         map[string]candidateCacheEntry{},
		providedVersionCache: make(map[string]providedVersion),
	}
	r.versionCache[virtualName] = candidateCacheEntry{err: packagist.ErrPackageNotFound}
	r.versionCache[providerName] = candidateCacheEntry{
		candidates: []candidate{{
			entry:   providerEntry,
			version: parse("1.0.0"),
		}},
	}

	pg := &pubGrubSolver{
		r:               r,
		s:               newState(),
		solution:        newPGPartialSolution(),
		incompatByPkg:   make(map[string][]*pgIncompatibility),
		pending:         map[string]pgPendingMeta{virtualName: {}, providerName: {}},
		minStabilityByP: make(map[string]version.Stability),
		candidateSets:   make(map[string]version.VersionSet),
		conflictPkgs:    make(map[string]struct{}),
	}
	pg.s.providers[virtualName] = provider{realName: providerName}

	var queue []string
	decided, missingOnly, unsat := pg.decide(&queue)
	if !decided {
		t.Fatalf("decide() = false, missing=%q unsat=%q", missingOnly, unsat)
	}
	if missingOnly != "" || unsat != "" {
		t.Fatalf("unexpected decide result: missing=%q unsat=%q", missingOnly, unsat)
	}
	if len(pg.decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(pg.decisions))
	}
	if got := pg.decisions[0].pkg; got != providerName {
		t.Fatalf("decided %q, want %q", got, providerName)
	}
}

func TestResolvePGIncompatibilitiesUnionsDuplicatePackageTerms(t *testing.T) {
	parseConstraint := func(raw string) version.VersionSet {
		t.Helper()
		c, err := version.ParseConstraint(raw)
		if err != nil {
			t.Fatalf("parse constraint %q: %v", raw, err)
		}
		return version.ConstraintVersionSet(c)
	}
	parseVersion := func(raw string) version.Version {
		t.Helper()
		v, err := version.Parse(raw)
		if err != nil {
			t.Fatalf("parse version %q: %v", raw, err)
		}
		return v
	}

	resolved := resolvePGIncompatibilities(
		&pgIncompatibility{terms: []pgTerm{
			{pkg: "acme/a", set: parseConstraint("^1.0")},
			{pkg: "acme/shared", set: parseConstraint("^1.0")},
		}},
		&pgIncompatibility{terms: []pgTerm{
			{pkg: "acme/a", set: parseConstraint("^2.0")},
			{pkg: "acme/shared", set: parseConstraint("^2.0")},
		}},
		"acme/a",
	)

	if len(resolved.terms) != 1 {
		t.Fatalf("got %d terms, want 1", len(resolved.terms))
	}
	shared := resolved.terms[0]
	if shared.pkg != "acme/shared" {
		t.Fatalf("pkg = %q, want acme/shared", shared.pkg)
	}
	if !shared.set.Contains(parseVersion("1.5.0")) {
		t.Fatal("resolved set should contain ^1.0 versions")
	}
	if !shared.set.Contains(parseVersion("2.5.0")) {
		t.Fatal("resolved set should contain ^2.0 versions")
	}
	if shared.set.Contains(parseVersion("3.0.0")) {
		t.Fatal("resolved set should not contain 3.0.0")
	}
}

func TestGetCandidatesRefetchesWithDevWhenNeeded(t *testing.T) {
	fetcher := &devAwareFetcher{}
	r := &resolver{
		ctx:          context.Background(),
		client:       fetcher,
		versionCache: make(map[string]candidateCacheEntry),
	}

	stableOnly, err := r.getCandidates("acme/foo", version.Stable)
	if err != nil {
		t.Fatalf("stable getCandidates: %v", err)
	}
	if len(stableOnly) != 1 {
		t.Fatalf("stable candidates = %d, want 1", len(stableOnly))
	}

	withDev, err := r.getCandidates("acme/foo", version.Dev)
	if err != nil {
		t.Fatalf("dev getCandidates: %v", err)
	}
	if len(withDev) != 2 {
		t.Fatalf("dev candidates = %d, want 2", len(withDev))
	}
	if fetcher.devCalls != 1 {
		t.Fatalf("dev fetches = %d, want 1", fetcher.devCalls)
	}
}
