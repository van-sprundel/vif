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
