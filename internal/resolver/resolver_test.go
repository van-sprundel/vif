package resolver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/resolver"
)

// registry is a test helper that serves Packagist p2 responses.
type registry struct {
	packages map[string][]packagist.VersionEntry
}

func newRegistry() *registry {
	return &registry{packages: make(map[string][]packagist.VersionEntry)}
}

func (r *registry) add(name, version string, require map[string]string) {
	r.addFull(name, version, require, nil, nil, nil)
}

func (r *registry) addFull(name, ver string, require, provide, replace, conflict map[string]string) {
	r.packages[name] = append(r.packages[name], packagist.VersionEntry{
		Name:     name,
		Version:  ver,
		Require:  require,
		Provide:  provide,
		Replace:  replace,
		Conflict: conflict,
		Dist: packagist.DistEntry{
			URL:       "https://example.com/" + name + "/" + ver + ".zip",
			Type:      "zip",
			Reference: "abc123",
		},
	})
}

func (r *registry) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Path: /p2/{vendor}/{name}.json
		path := req.URL.Path
		// Strip /p2/ prefix and .json suffix.
		name := path[len("/p2/") : len(path)-len(".json")]
		versions, ok := r.packages[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		resp := packagist.APIResponse{
			Packages: map[string][]packagist.VersionEntry{name: versions},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestResolveSinglePackage(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "2.0.0", nil)
	reg.add("acme/foo", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^1.0"},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 1 {
		t.Fatalf("got %d packages, want 1", len(resolved))
	}
	if resolved[0].Version != "1.0.0" {
		t.Errorf("version = %q, want %q", resolved[0].Version, "1.0.0")
	}
}

func TestResolvePicksHighestMatch(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "3.0.0", nil)
	reg.add("acme/foo", "2.5.0", nil)
	reg.add("acme/foo", "2.0.0", nil)
	reg.add("acme/foo", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^2.0"},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 1 {
		t.Fatalf("got %d packages, want 1", len(resolved))
	}
	if resolved[0].Version != "2.5.0" {
		t.Errorf("version = %q, want %q", resolved[0].Version, "2.5.0")
	}
}

func TestResolveTransitiveDeps(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "1.0.0", map[string]string{"acme/bar": "^1.0"})
	reg.add("acme/bar", "1.2.0", map[string]string{"acme/baz": "^2.0"})
	reg.add("acme/bar", "1.0.0", nil)
	reg.add("acme/baz", "2.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^1.0"},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 3 {
		t.Fatalf("got %d packages, want 3: %v", len(resolved), names(resolved))
	}

	byName := indexByName(resolved)
	if byName["acme/foo"].Version != "1.0.0" {
		t.Errorf("acme/foo = %q", byName["acme/foo"].Version)
	}
	if byName["acme/bar"].Version != "1.2.0" {
		t.Errorf("acme/bar = %q, want 1.2.0", byName["acme/bar"].Version)
	}
	if byName["acme/baz"].Version != "2.0.0" {
		t.Errorf("acme/baz = %q", byName["acme/baz"].Version)
	}
}

func TestResolveConflictRequiresBacktrack(t *testing.T) {
	// acme/foo 2.0.0 requires acme/shared ^2.0
	// acme/bar 1.0.0 requires acme/shared ^1.0
	// Both are required by root — only compatible if foo downgrades to 1.0.0 which requires acme/shared ^1.0.
	reg := newRegistry()
	reg.add("acme/foo", "2.0.0", map[string]string{"acme/shared": "^2.0"})
	reg.add("acme/foo", "1.0.0", map[string]string{"acme/shared": "^1.0"})
	reg.add("acme/bar", "1.0.0", map[string]string{"acme/shared": "^1.0"})
	reg.add("acme/shared", "2.0.0", nil)
	reg.add("acme/shared", "1.5.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name: "test/project",
		Require: map[string]string{
			"acme/foo": ">=1.0",
			"acme/bar": "^1.0",
		},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	byName := indexByName(resolved)
	// foo should have been backtracked to 1.0.0.
	if byName["acme/foo"].Version != "1.0.0" {
		t.Errorf("acme/foo = %q, want 1.0.0 (backtracked)", byName["acme/foo"].Version)
	}
	// shared should be ^1.0 compatible.
	if byName["acme/shared"].Version != "1.5.0" {
		t.Errorf("acme/shared = %q, want 1.5.0", byName["acme/shared"].Version)
	}
}

func TestResolveHonorsPackageConflicts(t *testing.T) {
	reg := newRegistry()
	reg.add("symfony/framework-bundle", "6.4.36", map[string]string{
		"symfony/config":          "^6.1|^7.0",
		"symfony/http-foundation": "^6.4|^7.0",
	})
	reg.add("symfony/config", "7.4.8", nil)
	reg.add("symfony/config", "6.4.34", nil)
	reg.addFull("symfony/http-foundation", "7.4.8", nil, nil, nil, map[string]string{
		"symfony/framework-bundle": "<7.0",
	})
	reg.add("symfony/http-foundation", "6.4.35", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name: "test/project",
		Require: map[string]string{
			"symfony/framework-bundle": "^6.4",
			"symfony/config":           "^6.1|^7.0",
			"symfony/http-foundation":  "^6.4|^7.0",
		},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	byName := indexByName(resolved)
	if byName["symfony/framework-bundle"].Version != "6.4.36" {
		t.Fatalf("framework-bundle = %q, want 6.4.36", byName["symfony/framework-bundle"].Version)
	}
	if byName["symfony/http-foundation"].Version != "6.4.35" {
		t.Fatalf("http-foundation = %q, want 6.4.35", byName["symfony/http-foundation"].Version)
	}
	if byName["symfony/config"].Version != "7.4.8" {
		t.Fatalf("config = %q, want 7.4.8", byName["symfony/config"].Version)
	}
}

func TestResolveUnsatisfiable(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^99.0"},
		MinimumStability: "stable",
	}

	_, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err == nil {
		t.Fatal("expected error for unsatisfiable constraint")
	}
}

func TestResolveDevDeps(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "1.0.0", nil)
	reg.add("acme/test", "2.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^1.0"},
		RequireDev:       map[string]string{"acme/test": "^2.0"},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 2 {
		t.Fatalf("got %d packages, want 2", len(resolved))
	}

	byName := indexByName(resolved)
	if _, ok := byName["acme/test"]; !ok {
		t.Error("missing dev dependency acme/test")
	}
}

func TestResolveFiltersPlatformFromTransitive(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "1.0.0", map[string]string{
		"php":      ">=8.0",
		"ext-json": "*",
		"acme/bar": "^1.0",
	})
	reg.add("acme/bar", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^1.0"},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 2 {
		t.Fatalf("got %d packages, want 2 (php/ext filtered): %v", len(resolved), names(resolved))
	}
}

func TestResolveStabilityFiltering(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "2.0.0-beta1", nil)
	reg.add("acme/foo", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	// With minimum-stability=stable, should pick 1.0.0.
	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": ">=1.0"},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	byName := indexByName(resolved)
	if byName["acme/foo"].Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0 (beta filtered)", byName["acme/foo"].Version)
	}

	// With minimum-stability=beta, should pick 2.0.0-beta1.
	cj.MinimumStability = "beta"
	resolved, err = resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	byName = indexByName(resolved)
	if byName["acme/foo"].Version != "2.0.0-beta1" {
		t.Errorf("version = %q, want 2.0.0-beta1", byName["acme/foo"].Version)
	}
}

func TestResolvePreferStable(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "2.0.0-beta1", nil)
	reg.add("acme/foo", "1.9.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": ">=1.0"},
		MinimumStability: "dev",
		PreferStable:     true,
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	byName := indexByName(resolved)
	if byName["acme/foo"].Version != "1.9.0" {
		t.Errorf("version = %q, want 1.9.0 (stable preferred over beta)", byName["acme/foo"].Version)
	}

	cj.PreferStable = false
	resolved, err = resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve without prefer-stable: %v", err)
	}

	byName = indexByName(resolved)
	if byName["acme/foo"].Version != "2.0.0-beta1" {
		t.Errorf("version = %q, want 2.0.0-beta1", byName["acme/foo"].Version)
	}
}

func TestResolvePackageNotFound(t *testing.T) {
	reg := newRegistry()
	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"nonexistent/pkg": "^1.0"},
		MinimumStability: "stable",
	}

	_, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err == nil {
		t.Fatal("expected error for nonexistent package")
	}
	if !strings.Contains(err.Error(), "nonexistent/pkg") {
		t.Fatalf("expected missing package in error, got %v", err)
	}
}

func TestResolveCancellationDuringBacktracking(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "3.0.0", map[string]string{"acme/bar": "^1.0"})
	reg.add("acme/foo", "2.0.0", map[string]string{"acme/bar": "^1.0"})
	reg.add("acme/foo", "1.0.0", map[string]string{"acme/bar": "^1.0"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := req.URL.Path[len("/p2/") : len(req.URL.Path)-len(".json")]
		versions, ok := reg.packages[name]
		if !ok {
			http.NotFound(w, req)
			return
		}

		resp := packagist.APIResponse{
			Packages: map[string][]packagist.VersionEntry{name: versions},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}

		if name == "acme/foo" {
			cancel()
		}
	}))
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": ">=1.0"},
		MinimumStability: "stable",
	}

	_, err := resolver.Resolve(ctx, cj, packagist.NewClient(srv.URL))
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context canceled error, got %v", err)
	}
}

func TestResolveProgressReportsUniqueLookups(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "1.0.0", map[string]string{"acme/bar": "^1.0"})
	reg.add("acme/bar", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^1.0", "acme/bar": "^1.0"},
		MinimumStability: "stable",
	}

	var (
		seenMu sync.Mutex
		seen   []string
	)
	_, err := resolver.ResolveWithProgress(context.Background(), cj, packagist.NewClient(srv.URL), func(name string) {
		seenMu.Lock()
		seen = append(seen, name)
		seenMu.Unlock()
	})
	if err != nil {
		t.Fatalf("ResolveWithProgress: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("got %d progress events, want 2: %v", len(seen), seen)
	}
	// Prefetch is parallel so ordering is non-deterministic; verify both names appear.
	seenSet := make(map[string]bool, len(seen))
	for _, n := range seen {
		seenSet[n] = true
	}
	if !seenSet["acme/bar"] || !seenSet["acme/foo"] {
		t.Fatalf("progress events = %v, want both acme/bar and acme/foo", seen)
	}
}

func TestResolveMergesDuplicatePendingRequirements(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/app", "1.0.0", map[string]string{
		"nikic/php-parser":              "^5.0",
		"romanzipp/php-cs-fixer-config": "^1.0",
	})
	reg.add("romanzipp/php-cs-fixer-config", "1.0.0", map[string]string{
		"nikic/php-parser": "^5.0",
	})
	reg.add("nikic/php-parser", "5.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/app": "^1.0"},
		MinimumStability: "stable",
	}

	var (
		seenMu sync.Mutex
		seen   []string
	)
	resolved, err := resolver.ResolveWithProgress(context.Background(), cj, packagist.NewClient(srv.URL), func(name string) {
		seenMu.Lock()
		seen = append(seen, name)
		seenMu.Unlock()
	})
	if err != nil {
		t.Fatalf("ResolveWithProgress: %v", err)
	}
	if len(resolved) != 3 {
		t.Fatalf("got %d packages, want 3", len(resolved))
	}

	var phpParserLookups int
	for _, name := range seen {
		if name == "nikic/php-parser" {
			phpParserLookups++
		}
	}
	if phpParserLookups != 1 {
		t.Fatalf("nikic/php-parser looked up %d times, want 1; seen=%v", phpParserLookups, seen)
	}
}

func TestResolveCachesMissingPackageFailures(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/app", "1.0.0", map[string]string{
		"missing/private-package": "^1.0",
		"acme/provider":           "^1.0",
	})
	reg.add("acme/provider", "1.0.0", map[string]string{
		"missing/private-package": "^1.0",
	})

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/app": "^1.0"},
		MinimumStability: "stable",
	}

	var (
		seenMu sync.Mutex
		seen   []string
	)
	_, err := resolver.ResolveWithProgress(context.Background(), cj, packagist.NewClient(srv.URL), func(name string) {
		seenMu.Lock()
		seen = append(seen, name)
		seenMu.Unlock()
	})
	if err == nil {
		t.Fatal("expected error")
	}

	var missingLookups int
	for _, name := range seen {
		if name == "missing/private-package" {
			missingLookups++
		}
	}
	if missingLookups != 1 {
		t.Fatalf("missing/private-package looked up %d times, want 1; seen=%v", missingLookups, seen)
	}
}

func TestResolveDoesNotDeferPackagistNotFound(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/app", "1.0.0", map[string]string{
		"missing/private-package": "^1.0",
		"acme/other":              "^1.0",
	})
	reg.add("acme/other", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/app": "^1.0"},
		MinimumStability: "stable",
	}

	var (
		seenMu sync.Mutex
		seen   []string
	)
	_, err := resolver.ResolveWithProgress(context.Background(), cj, packagist.NewClient(srv.URL), func(name string) {
		seenMu.Lock()
		seen = append(seen, name)
		seenMu.Unlock()
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "private/custom repositories are not supported yet") {
		t.Fatalf("expected unsupported private repository error, got %v", err)
	}
	// With parallel prefetch all transitive deps (including acme/other) are fetched
	// eagerly. The important invariant is that the resolver returns the correct
	// terminal error for the missing package, not that acme/other is never looked up.
}

// helpers

func indexByName(pkgs []resolver.ResolvedPackage) map[string]resolver.ResolvedPackage {
	m := make(map[string]resolver.ResolvedPackage, len(pkgs))
	for _, p := range pkgs {
		m[p.Name] = p
	}
	return m
}

func names(pkgs []resolver.ResolvedPackage) []string {
	out := make([]string, len(pkgs))
	for i, p := range pkgs {
		out[i] = p.Name + "@" + p.Version
	}
	return out
}

// --- provide/replace tests ---

func TestResolveProvide(t *testing.T) {
	// acme/log requires psr/log-implementation ^1.0
	// acme/monolog provides psr/log-implementation 1.0.0
	reg := newRegistry()
	reg.add("acme/log", "1.0.0", map[string]string{"psr/log-implementation": "^1.0"})
	reg.addFull("acme/monolog", "2.0.0", nil, map[string]string{"psr/log-implementation": "1.0.0"}, nil, nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name: "test/project",
		Require: map[string]string{
			"acme/monolog": "^2.0",
			"acme/log":     "^1.0",
		},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Should resolve acme/monolog and acme/log only — psr/log-implementation
	// is satisfied by the provide from acme/monolog.
	if len(resolved) != 2 {
		t.Fatalf("got %d packages, want 2: %v", len(resolved), names(resolved))
	}

	byName := indexByName(resolved)
	if _, ok := byName["acme/monolog"]; !ok {
		t.Error("missing acme/monolog")
	}
	if _, ok := byName["acme/log"]; !ok {
		t.Error("missing acme/log")
	}
}

func TestResolveReplace(t *testing.T) {
	// acme/app requires acme/old-lib ^1.0
	// acme/new-lib replaces acme/old-lib 1.0.0
	reg := newRegistry()
	reg.add("acme/app", "1.0.0", map[string]string{"acme/old-lib": "^1.0"})
	reg.addFull("acme/new-lib", "2.0.0", nil, nil, map[string]string{"acme/old-lib": "1.0.0"}, nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name: "test/project",
		Require: map[string]string{
			"acme/new-lib": "^2.0",
			"acme/app":     "^1.0",
		},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 2 {
		t.Fatalf("got %d packages, want 2: %v", len(resolved), names(resolved))
	}

	byName := indexByName(resolved)
	if _, ok := byName["acme/new-lib"]; !ok {
		t.Error("missing acme/new-lib")
	}
}

func TestResolveProvideWildcard(t *testing.T) {
	// Provide with no version (wildcard) should satisfy any constraint.
	reg := newRegistry()
	reg.add("acme/consumer", "1.0.0", map[string]string{"acme/contract": "^2.0"})
	reg.addFull("acme/impl", "1.0.0", nil, map[string]string{"acme/contract": "*"}, nil, nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name: "test/project",
		Require: map[string]string{
			"acme/impl":     "^1.0",
			"acme/consumer": "^1.0",
		},
		MinimumStability: "stable",
	}

	resolved, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 2 {
		t.Fatalf("got %d packages, want 2: %v", len(resolved), names(resolved))
	}
}

// --- error message tests ---

func TestResolveErrorMessageUnsatisfiable(t *testing.T) {
	reg := newRegistry()
	reg.add("acme/foo", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"acme/foo": "^99.0"},
		MinimumStability: "stable",
	}

	_, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "acme/foo") {
		t.Errorf("error should mention package name, got: %s", errMsg)
	}
}

func TestResolveErrorMessageConflict(t *testing.T) {
	// Create an unsatisfiable scenario: acme/a requires acme/shared ^2.0, acme/b requires acme/shared ^1.0
	// but acme/a only has one version so backtracking can't help.
	reg := newRegistry()
	reg.add("acme/a", "1.0.0", map[string]string{"acme/shared": "^2.0"})
	reg.add("acme/b", "1.0.0", map[string]string{"acme/shared": "^1.0"})
	reg.add("acme/shared", "2.0.0", nil)
	reg.add("acme/shared", "1.0.0", nil)

	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name: "test/project",
		Require: map[string]string{
			"acme/a": "^1.0",
			"acme/b": "^1.0",
		},
		MinimumStability: "stable",
	}

	_, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "acme/shared") {
		t.Errorf("error should mention the conflicting package, got: %s", errMsg)
	}
}

func TestResolveErrorMessageNotFound(t *testing.T) {
	reg := newRegistry()
	srv := reg.serve(t)
	defer srv.Close()

	cj := &composer.ComposerJSON{
		Name:             "test/project",
		Require:          map[string]string{"nonexistent/pkg": "^1.0"},
		MinimumStability: "stable",
	}

	_, err := resolver.Resolve(context.Background(), cj, packagist.NewClient(srv.URL))
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "nonexistent/pkg") {
		t.Errorf("error should mention the missing package, got: %s", errMsg)
	}
}
