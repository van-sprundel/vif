package resolver

import (
	"context"
	"testing"

	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/version"
)

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
		ctx: context.Background(),
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
