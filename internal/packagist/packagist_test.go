package packagist_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composerauth"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/testhelper"
)

// sampleResponse returns a minimal but realistic Packagist p2 response.
func sampleResponse() packagist.APIResponse {
	return packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"acme/foo": {
				{
					Name:              "acme/foo",
					Version:           "2.0.0",
					VersionNormalized: "2.0.0.0",
					Type:              "library",
					Require: map[string]string{
						"php":      ">=8.0",
						"acme/bar": "^1.0",
						"psr/log":  "^3.0",
					},
					RequireDev: map[string]string{
						"phpunit/phpunit": "^10.0",
					},
					Autoload: json.RawMessage(`{"psr-4":{"Acme\\Foo\\":"src/"}}`),
					Dist: packagist.DistEntry{
						URL:       "https://api.github.com/repos/acme/foo/zipball/abc123",
						Type:      "zip",
						Reference: "abc123",
					},
				},
				{
					Name:              "acme/foo",
					Version:           "1.5.0",
					VersionNormalized: "1.5.0.0",
					Type:              "library",
					Require: map[string]string{
						"php":      ">=7.4",
						"acme/bar": "^1.0",
					},
					Dist: packagist.DistEntry{
						URL:       "https://api.github.com/repos/acme/foo/zipball/def456",
						Type:      "zip",
						Reference: "def456",
					},
				},
				{
					Name:    "acme/foo",
					Version: "dev-main",
					Type:    "library",
					Require: map[string]string{
						"acme/bar": "^2.0",
					},
					Dist: packagist.DistEntry{
						URL:  "https://api.github.com/repos/acme/foo/zipball/HEAD",
						Type: "zip",
					},
				},
			},
		},
	}
}

func TestClientGetPackage(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p2/acme/foo.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"etag-123"`)
		w.Write(data)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}

	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}

	// Verify first version (should be 2.0.0 — order preserved from API).
	v := versions[0]
	if v.Version != "2.0.0" {
		t.Errorf("versions[0].Version = %q, want %q", v.Version, "2.0.0")
	}
	if v.Require["acme/bar"] != "^1.0" {
		t.Errorf("require[acme/bar] = %q, want %q", v.Require["acme/bar"], "^1.0")
	}
	if v.Dist.URL != "https://api.github.com/repos/acme/foo/zipball/abc123" {
		t.Errorf("dist.url = %q", v.Dist.URL)
	}
}

func TestClientMatchPackageLearnsVendorAvailability(t *testing.T) {
	type matcher interface {
		MatchPackage(string) bool
	}

	barResp := packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"acme/bar": {{Name: "acme/bar", Version: "1.0.0", Type: "library"}},
		},
	}
	barData, _ := json.Marshal(barResp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/acme/foo.json":
			http.NotFound(w, r)
		case "/p2/acme/bar.json":
			w.Write(barData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	m, ok := interface{}(client).(matcher)
	if !ok {
		t.Fatal("client should implement MatchPackage")
	}

	if !m.MatchPackage("acme/foo") {
		t.Fatal("unknown vendor should be eligible before learning")
	}

	if _, err := client.GetPackage(context.Background(), "acme/foo"); !errors.Is(err, packagist.ErrPackageNotFound) {
		t.Fatalf("GetPackage(acme/foo) error = %v, want ErrPackageNotFound", err)
	}

	if m.MatchPackage("acme/baz") {
		t.Fatal("vendor with only misses should be skipped")
	}

	if _, err := client.GetPackage(context.Background(), "acme/bar"); err != nil {
		t.Fatalf("GetPackage(acme/bar): %v", err)
	}

	if !m.MatchPackage("acme/qux") {
		t.Fatal("vendor should be eligible again after a hit")
	}
}

func TestClientGetPackageMergesDevVersions(t *testing.T) {
	stableResp := packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"acme/foo": {
				{Name: "acme/foo", Version: "1.0.0", Type: "library"},
				{Name: "acme/foo", Version: "2.0.0", Type: "library"},
			},
		},
	}
	stableData, _ := json.Marshal(stableResp)

	devResp := packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"acme/foo": {
				{Name: "acme/foo", Version: "dev-master", Type: "library"},
				{Name: "acme/foo", Version: "dev-feature-x", Type: "library"},
			},
		},
	}
	devData, _ := json.Marshal(devResp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/acme/foo.json":
			w.Header().Set("ETag", `"stable-1"`)
			w.Write(stableData)
		case "/p2/acme/foo~dev.json":
			w.Header().Set("ETag", `"dev-1"`)
			w.Write(devData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}

	if len(versions) != 4 {
		t.Fatalf("got %d versions, want 4 (2 stable + 2 dev)", len(versions))
	}

	gotVersions := make(map[string]bool, len(versions))
	for _, v := range versions {
		gotVersions[v.Version] = true
	}
	for _, want := range []string{"1.0.0", "2.0.0", "dev-master", "dev-feature-x"} {
		if !gotVersions[want] {
			t.Errorf("missing version %q in merged result", want)
		}
	}

	// Second call should be served from in-memory cache (no additional HTTP requests).
	versions2, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("second GetPackage: %v", err)
	}
	if len(versions2) != 4 {
		t.Fatalf("second call: got %d versions, want 4", len(versions2))
	}
}

func TestClientGetPackageMergesDevVersionsDeduplicates(t *testing.T) {
	stableResp := packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"acme/foo": {
				{Name: "acme/foo", Version: "1.0.0", Type: "library"},
			},
		},
	}
	stableData, _ := json.Marshal(stableResp)

	devResp := packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"acme/foo": {
				{Name: "acme/foo", Version: "1.0.0", Type: "library", Dist: packagist.DistEntry{URL: "dev-dist"}},
				{Name: "acme/foo", Version: "dev-main", Type: "library"},
			},
		},
	}
	devData, _ := json.Marshal(devResp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/acme/foo.json":
			w.Write(stableData)
		case "/p2/acme/foo~dev.json":
			w.Write(devData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}

	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2 (deduped 1.0.0)", len(versions))
	}
}

func TestClientGetPackageWithDevCanSkipDevEndpoint(t *testing.T) {
	stableResp := packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"acme/foo": {
				{Name: "acme/foo", Version: "1.0.0", Type: "library"},
			},
		},
	}
	stableData, _ := json.Marshal(stableResp)

	var devRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/acme/foo.json":
			w.Write(stableData)
		case "/p2/acme/foo~dev.json":
			devRequests++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	devClient, ok := interface{}(client).(packagist.DevFetcher)
	if !ok {
		t.Fatal("client should implement DevFetcher")
	}

	versions, err := devClient.GetPackageWithDev(context.Background(), "acme/foo", false)
	if err != nil {
		t.Fatalf("GetPackageWithDev: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want 1", len(versions))
	}
	if devRequests != 0 {
		t.Fatalf("dev endpoint called %d times, want 0", devRequests)
	}
}

func TestClientGetPackageObjectShapedP2Response(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p2/urbanheroes-symfony/uh-enhanced-security-bundle.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"packages":{"urbanheroes-symfony/uh-enhanced-security-bundle":{"1.0.0":{"name":"urbanheroes-symfony/uh-enhanced-security-bundle","version":"1.0.0"}}}}`))
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "urbanheroes-symfony/uh-enhanced-security-bundle")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != 1 || versions[0].Version != "1.0.0" {
		t.Fatalf("unexpected versions: %+v", versions)
	}
}

func TestClientGetPackageMinifiedResponseInheritsPreviousFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p2/symfony/monolog-bundle.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
			"minified":"composer/2.0",
			"packages":{
				"symfony/monolog-bundle":[
					{
						"name":"symfony/monolog-bundle",
						"version":"v3.11.2",
						"require":{
							"monolog/monolog":"^1.25.1 || ^2.0 || ^3.0",
							"symfony/monolog-bridge":"^6.4 || ^7.0"
						}
					},
					{
						"version":"v3.11.1"
					}
				]
			}
		}`))
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "symfony/monolog-bundle")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}

	if got := versions[1].Require["monolog/monolog"]; got != "^1.25.1 || ^2.0 || ^3.0" {
		t.Fatalf("v3.11.1 require[monolog/monolog] = %q", got)
	}
	if got := versions[1].Require["symfony/monolog-bridge"]; got != "^6.4 || ^7.0" {
		t.Fatalf("v3.11.1 require[symfony/monolog-bridge] = %q", got)
	}
}

func TestClientGetPackageNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	_, err := client.GetPackage(context.Background(), "nonexistent/pkg")
	if err == nil {
		t.Fatal("expected error for nonexistent package")
	}
}

func TestClientETagCaching(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)
	var requestCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		etag := `"etag-v1"`

		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("ETag", etag)
		w.Write(data)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)

	// First request — full response.
	v1, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("first GetPackage: %v", err)
	}
	if len(v1) != 3 {
		t.Fatalf("first: got %d versions, want 3", len(v1))
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 requests, got %d", requestCount)
	}

	// Second request — should return from in-memory cache (devMerged), no HTTP.
	v2, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("second GetPackage: %v", err)
	}
	if len(v2) != 3 {
		t.Fatalf("second: got %d versions, want 3", len(v2))
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 requests after cache hit, got %d", requestCount)
	}
}

func TestClientContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetPackage(ctx, "acme/foo")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	client := packagist.NewClientWithHTTPClient(srv.URL, &http.Client{Timeout: 20 * time.Millisecond})
	_, err := client.GetPackage(context.Background(), "acme/foo")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestClientCachesNotFound(t *testing.T) {
	var requestCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)

	for range 2 {
		_, err := client.GetPackage(context.Background(), "missing/pkg")
		if err == nil {
			t.Fatal("expected not found error")
		}
	}

	if requestCount != 2 {
		t.Fatalf("expected 2 requests, got %d", requestCount)
	}
}

func TestClientCachesAuthRequired(t *testing.T) {
	var requestCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		http.Error(w, "auth required", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)

	for _, name := range []string{"acme/foo", "acme/bar"} {
		_, err := client.GetPackage(context.Background(), name)
		if err == nil {
			t.Fatal("expected auth-required error")
		}
		if !strings.Contains(err.Error(), "authentication required") {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if requestCount != 1 {
		t.Fatalf("expected 1 request, got %d", requestCount)
	}
}

func TestClientAppliesComposerAuth(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	client.SetAuth(&composerauth.Config{
		Bearer: map[string]string{
			strings.TrimPrefix(strings.TrimPrefix(srv.URL, "http://"), "https://"): "token-123",
		},
	})

	if _, err := client.GetPackage(context.Background(), "acme/foo"); err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
}

func TestClientFallsBackToComposerIncludes(t *testing.T) {
	var rootRequests int
	var includeRequests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/urbanheroes-sf/asset.json":
			http.NotFound(w, r)
		case "/packages.json":
			rootRequests++
			_, _ = w.Write([]byte(`{"includes":{"include/all$abc.json":{"sha1":"abc"}}}`))
		case "/include/all$abc.json":
			includeRequests++
			_, _ = w.Write([]byte(`{"packages":{"urbanheroes-sf/asset":{"1.0.0":{"name":"urbanheroes-sf/asset","version":"1.0.0"}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)

	for range 2 {
		versions, err := client.GetPackage(context.Background(), "urbanheroes-sf/asset")
		if err != nil {
			t.Fatalf("GetPackage: %v", err)
		}
		if len(versions) != 1 || versions[0].Version != "1.0.0" {
			t.Fatalf("unexpected versions: %+v", versions)
		}
	}

	if rootRequests != 1 {
		t.Fatalf("rootRequests = %d, want 1", rootRequests)
	}
	if includeRequests != 1 {
		t.Fatalf("includeRequests = %d, want 1", includeRequests)
	}
}

func TestClientIgnoresEmptyArrayRootPackages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/acme/foo.json":
			http.NotFound(w, r)
		case "/packages.json":
			_, _ = w.Write([]byte(`{"packages":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)

	_, err := client.GetPackage(context.Background(), "acme/foo")
	if err == nil {
		t.Fatal("expected not found error")
	}
	if !strings.Contains(err.Error(), "package not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientDoesNotFallbackToIncludesWhenMetadataURLPresent(t *testing.T) {
	var rootRequests int
	var includeRequests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/acme/foo.json":
			http.NotFound(w, r)
		case "/packages.json":
			rootRequests++
			_, _ = w.Write([]byte(`{"metadata-url":"/p2/%package%.json","includes":{"include/all$abc.json":{"sha1":"abc"}}}`))
		case "/include/all$abc.json":
			includeRequests++
			_, _ = w.Write([]byte(`{"packages":{"acme/foo":{"1.0.0":{"name":"acme/foo","version":"1.0.0"}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	_, err := client.GetPackage(context.Background(), "acme/foo")
	if err == nil {
		t.Fatal("expected not found error")
	}
	if !strings.Contains(err.Error(), "package not found") {
		t.Fatalf("unexpected error: %v", err)
	}
	if rootRequests != 1 {
		t.Fatalf("rootRequests = %d, want 1", rootRequests)
	}
	if includeRequests != 0 {
		t.Fatalf("includeRequests = %d, want 0", includeRequests)
	}
}

func TestChainFallsBackAcrossRepositories(t *testing.T) {
	privateResp := packagist.APIResponse{
		Packages: map[string][]packagist.VersionEntry{
			"urbanheroes-sf/asset": {
				{
					Name:    "urbanheroes-sf/asset",
					Version: "1.0.0",
				},
			},
		},
	}
	privateData, _ := json.Marshal(privateResp)

	privateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p2/urbanheroes-sf/asset.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(privateData)
	}))
	defer privateSrv.Close()

	publicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer publicSrv.Close()

	chain := packagist.NewChain(
		packagist.NewClient(privateSrv.URL),
		packagist.NewClient(publicSrv.URL),
	)

	versions, err := chain.GetPackage(context.Background(), "urbanheroes-sf/asset")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != 1 || versions[0].Name != "urbanheroes-sf/asset" {
		t.Fatalf("unexpected versions: %+v", versions)
	}
}

func TestChainSkipsAuthRequiredRepository(t *testing.T) {
	publicResp := sampleResponse()
	publicData, _ := json.Marshal(publicResp)

	privateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "auth required", http.StatusUnauthorized)
	}))
	defer privateSrv.Close()

	publicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p2/acme/foo.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(publicData)
	}))
	defer publicSrv.Close()

	chain := packagist.NewChain(
		packagist.NewClient(privateSrv.URL),
		packagist.NewClient(publicSrv.URL),
	)

	versions, err := chain.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}
}

type timeoutFetcher struct{}

func (timeoutFetcher) GetPackage(context.Context, string) ([]packagist.VersionEntry, error) {
	return nil, fmt.Errorf("packagist: fetch include: %w", context.DeadlineExceeded)
}

func TestChainSkipsTransientTimeoutRepository(t *testing.T) {
	publicResp := sampleResponse()
	publicData, _ := json.Marshal(publicResp)

	publicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p2/acme/foo.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(publicData)
	}))
	defer publicSrv.Close()

	chain := packagist.NewChain(
		timeoutFetcher{},
		packagist.NewClient(publicSrv.URL),
	)

	versions, err := chain.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}
}

type notFoundFetcher struct{}

func (notFoundFetcher) GetPackage(context.Context, string) ([]packagist.VersionEntry, error) {
	return nil, fmt.Errorf("%w: missing/pkg", packagist.ErrPackageNotFound)
}

type countingNotFoundFetcher struct {
	calls atomic.Int32
	delay time.Duration
}

func (f *countingNotFoundFetcher) GetPackage(_ context.Context, _ string) ([]packagist.VersionEntry, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.calls.Add(1)
	return nil, fmt.Errorf("%w: missing/pkg", packagist.ErrPackageNotFound)
}

func TestChainReturnsTransientErrorWhenNoRepositorySucceeds(t *testing.T) {
	chain := packagist.NewChain(timeoutFetcher{}, notFoundFetcher{})

	_, err := chain.GetPackage(context.Background(), "missing/pkg")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestChainSkipsSourceAfterPrefixMiss(t *testing.T) {
	source := &countingNotFoundFetcher{}

	chain := packagist.NewChain(source)

	// First lookup: source is called (miss recorded for "acme/" prefix).
	_, _ = chain.GetPackage(context.Background(), "acme/foo")
	if got := source.calls.Load(); got != 1 {
		t.Fatalf("first call: got %d calls, want 1", got)
	}

	// Second lookup with same prefix: source is skipped.
	_, _ = chain.GetPackage(context.Background(), "acme/bar")
	if got := source.calls.Load(); got != 1 {
		t.Fatalf("second call: got %d calls, want 1 (should be skipped)", got)
	}
}

func TestChainDoesNotSkipPrefixWithHit(t *testing.T) {
	hitVersions := []packagist.VersionEntry{{Version: "1.0.0"}}
	hitThenMiss := &hitThenMissFetcher{hitVersions: hitVersions}

	chain := packagist.NewChain(hitThenMiss)

	// First lookup: hit (records a hit for "acme/" prefix).
	_, err := chain.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("first call: unexpected error %v", err)
	}

	// Second lookup with same prefix: source is NOT skipped because it had a hit.
	_, _ = chain.GetPackage(context.Background(), "acme/bar")
	if hitThenMiss.calls != 2 {
		t.Fatalf("second call: got %d calls, want 2 (should not be skipped)", hitThenMiss.calls)
	}
}

type hitThenMissFetcher struct {
	calls       int
	hitVersions []packagist.VersionEntry
}

func (f *hitThenMissFetcher) GetPackage(_ context.Context, _ string) ([]packagist.VersionEntry, error) {
	f.calls++
	if f.calls == 1 {
		return f.hitVersions, nil
	}
	return nil, fmt.Errorf("%w: missing", packagist.ErrPackageNotFound)
}

func TestChainProbeGatePreventsConcurrentMisses(t *testing.T) {
	source := &countingNotFoundFetcher{delay: 50 * time.Millisecond}

	chain := packagist.NewChain(source)

	// Launch 5 concurrent lookups for the same prefix.
	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, _ = chain.GetPackage(context.Background(), fmt.Sprintf("acme/pkg%d", i))
		}(i)
	}
	wg.Wait()

	// Only the first goroutine should have called the source (the "prober").
	// The others waited, saw the miss, and skipped.
	if got := source.calls.Load(); got != 1 {
		t.Fatalf("concurrent lookups: got %d calls, want 1", got)
	}
}

func TestChainEmitsLookupTracePerRepositoryAttempt(t *testing.T) {
	publicResp := sampleResponse()
	publicData, _ := json.Marshal(publicResp)

	privateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer privateSrv.Close()

	publicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p2/acme/foo.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(publicData)
	}))
	defer publicSrv.Close()

	chain := packagist.NewChain(
		packagist.NewClient(privateSrv.URL),
		packagist.NewClient(publicSrv.URL),
	)

	var traces []packagist.LookupTrace
	chain.SetLookupTrace(func(trace packagist.LookupTrace) {
		traces = append(traces, trace)
	})

	versions, err := chain.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}
	if len(traces) != 2 {
		t.Fatalf("got %d traces, want 2", len(traces))
	}
	if traces[0].Source != privateSrv.URL {
		t.Fatalf("first source = %q, want %q", traces[0].Source, privateSrv.URL)
	}
	if !errors.Is(traces[0].Err, packagist.ErrPackageNotFound) {
		t.Fatalf("first trace err = %v, want not found", traces[0].Err)
	}
	if traces[1].Source != publicSrv.URL {
		t.Fatalf("second source = %q, want %q", traces[1].Source, publicSrv.URL)
	}
	if traces[1].Err != nil {
		t.Fatalf("second trace err = %v, want nil", traces[1].Err)
	}
}

func TestPersistentMetadataCacheETagRoundTrip(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)
	var requestCount int
	var lastIfNoneMatch string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		lastIfNoneMatch = r.Header.Get("If-None-Match")
		etag := `"etag-persistent"`

		if lastIfNoneMatch == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("ETag", etag)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	// Open a cache in a temp dir.
	cacheDir := testhelper.TempDir(t, "cache")
	mc, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer mc.Close()

	// First client — cold start, should get a 200 and persist to the cache.
	client1 := packagist.NewClient(srv.URL)
	client1.SetMetadataCache(mc)

	versions, err := client1.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("first GetPackage: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("first: got %d versions, want 3", len(versions))
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 requests after first call, got %d", requestCount)
	}
	if lastIfNoneMatch != "" {
		t.Fatalf("first request should not send If-None-Match, got %q", lastIfNoneMatch)
	}

	// Second client — simulates a fresh process with the same on-disk cache.
	// It has no in-memory cache entry, so it must load the ETag from the persistent cache.
	client2 := packagist.NewClient(srv.URL)
	client2.SetMetadataCache(mc)

	versions2, err := client2.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("second GetPackage: %v", err)
	}
	if len(versions2) != 3 {
		t.Fatalf("second: got %d versions, want 3", len(versions2))
	}
	if requestCount != 4 {
		t.Fatalf("expected 4 requests after second call, got %d", requestCount)
	}
	if lastIfNoneMatch != `"etag-persistent"` {
		t.Fatalf("second request should send If-None-Match=%q, got %q", `"etag-persistent"`, lastIfNoneMatch)
	}
}

func TestPersistentMetadataCacheIgnoresCorruptBody(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)
	var requestCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("ETag", `"etag-fresh"`)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	cacheDir := testhelper.TempDir(t, "cache")
	mc, err := cache.New(cacheDir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer mc.Close()

	// Pre-populate cache with corrupt JSON body so unmarshal will fail.
	if err := mc.InsertMetadata(srv.URL, "acme/foo", `"etag-corrupt"`, []byte(`{not valid json`)); err != nil {
		t.Fatalf("InsertMetadata: %v", err)
	}

	// Client should fall through to a fresh HTTP request (treating corrupt cache as miss).
	client := packagist.NewClient(srv.URL)
	client.SetMetadataCache(mc)

	versions, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 HTTP requests, got %d", requestCount)
	}
}

func TestFilterPlatformRequirements(t *testing.T) {
	entry := packagist.VersionEntry{
		Name:    "acme/foo",
		Version: "1.0.0",
		Require: packagist.RelationMap{
			"php":          ">=8.0",
			"ext-json":     "*",
			"ext-mbstring": "*",
			"acme/bar":     "^1.0",
			"psr/log":      "^3.0",
		},
	}

	filtered := entry.NonPlatformRequire()
	if len(filtered) != 2 {
		t.Fatalf("got %d non-platform requires, want 2: %v", len(filtered), filtered)
	}
	if filtered["acme/bar"] != "^1.0" {
		t.Errorf("missing acme/bar")
	}
	if filtered["psr/log"] != "^3.0" {
		t.Errorf("missing psr/log")
	}
}

func TestRelationMapUnmarshalMixedShapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  packagist.RelationMap
	}{
		{
			name:  "object",
			input: `{"doctrine/event-manager":"^1|^2"}`,
			want:  packagist.RelationMap{"doctrine/event-manager": "^1|^2"},
		},
		{
			name:  "string treated as empty",
			input: `"conflicts-with-nothing"`,
			want:  nil,
		},
		{
			name:  "empty array treated as empty",
			input: `[]`,
			want:  nil,
		},
		{
			name:  "null treated as empty",
			input: `null`,
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got packagist.RelationMap
			if err := json.Unmarshal([]byte(tc.input), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(tc.want))
			}
			for key, wantValue := range tc.want {
				if got[key] != wantValue {
					t.Fatalf("got[%q] = %q, want %q", key, got[key], wantValue)
				}
			}
		})
	}
}

func TestStringListUnmarshalMixedShapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  packagist.StringList
	}{
		{
			name:  "single string",
			input: `"bin/console"`,
			want:  packagist.StringList{"bin/console"},
		},
		{
			name:  "array",
			input: `["bin/console","bin/tool"]`,
			want:  packagist.StringList{"bin/console", "bin/tool"},
		},
		{
			name:  "null",
			input: `null`,
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got packagist.StringList
			if err := json.Unmarshal([]byte(tc.input), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestGetPackageRetriesOn429(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "~dev") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := int(attempts)
		attempts++
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != len(resp.Packages["acme/foo"]) {
		t.Fatalf("got %d versions, want %d", len(versions), len(resp.Packages["acme/foo"]))
	}
	if int(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestGetPackageRetriesOn503(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "~dev") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := int(attempts)
		attempts++
		if n < 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) != len(resp.Packages["acme/foo"]) {
		t.Fatalf("got %d versions, want %d", len(versions), len(resp.Packages["acme/foo"]))
	}
}

func TestGetPackageExhaustsRetriesOn429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := packagist.NewClientWithHTTPClient(srv.URL, &http.Client{Timeout: 10 * time.Second})
	_, err := client.GetPackage(context.Background(), "acme/foo")
	if err == nil {
		t.Fatal("expected error when all retries are 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 in error, got: %v", err)
	}
}

func TestGetPackageRetryAfterHeader(t *testing.T) {
	resp := sampleResponse()
	data, _ := json.Marshal(resp)

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "~dev") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := int(attempts)
		attempts++
		if n < 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	start := time.Now()
	client := packagist.NewClient(srv.URL)
	versions, err := client.GetPackage(context.Background(), "acme/foo")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("GetPackage: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("expected versions")
	}
	if elapsed < 800*time.Millisecond {
		t.Fatalf("expected at least ~1s retry delay, got %v", elapsed)
	}
}
