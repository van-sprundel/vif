package packagist_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/van-sprundel/vif/internal/composerauth"
	"github.com/van-sprundel/vif/internal/packagist"
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
	if requestCount != 1 {
		t.Fatalf("expected 1 request, got %d", requestCount)
	}

	// Second request — should send If-None-Match, get 304, use cache.
	v2, err := client.GetPackage(context.Background(), "acme/foo")
	if err != nil {
		t.Fatalf("second GetPackage: %v", err)
	}
	if len(v2) != 3 {
		t.Fatalf("second: got %d versions, want 3", len(v2))
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 requests, got %d", requestCount)
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

	if requestCount != 1 {
		t.Fatalf("expected 1 request, got %d", requestCount)
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
