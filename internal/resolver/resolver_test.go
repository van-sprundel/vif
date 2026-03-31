package resolver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	r.packages[name] = append(r.packages[name], packagist.VersionEntry{
		Name:    name,
		Version: version,
		Require: require,
		Dist: packagist.DistEntry{
			URL:       "https://example.com/" + name + "/" + version + ".zip",
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
		"php":          ">=8.0",
		"ext-json":     "*",
		"acme/bar":     "^1.0",
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
