package packagist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/van-sprundel/vif/internal/pkg"
)

// APIResponse is the top-level Packagist p2 response.
type APIResponse struct {
	Packages map[string][]VersionEntry `json:"packages"`
}

// VersionEntry is a single version of a package from Packagist.
type VersionEntry struct {
	Name              string            `json:"name"`
	Version           string            `json:"version"`
	VersionNormalized string            `json:"version_normalized"`
	Type              string            `json:"type"`
	Bin               []string          `json:"bin"`
	Require           map[string]string `json:"require"`
	RequireDev        map[string]string `json:"require-dev"`
	Provide           map[string]string `json:"provide"`
	Replace           map[string]string `json:"replace"`
	Conflict          map[string]string `json:"conflict"`
	Autoload          json.RawMessage   `json:"autoload"`
	AutoloadDev       json.RawMessage   `json:"autoload-dev"`
	Dist              DistEntry         `json:"dist"`
	Time              string            `json:"time"`
}

// DistEntry holds dist metadata from Packagist.
type DistEntry struct {
	URL       string `json:"url"`
	Type      string `json:"type"`
	Reference string `json:"reference"`
	Shasum    string `json:"shasum"`
}

// NonPlatformRequire returns the require map with platform packages
// (php, ext-*, lib-*) filtered out.
func (v VersionEntry) NonPlatformRequire() map[string]string {
	out := make(map[string]string, len(v.Require))
	for name, constraint := range v.Require {
		if pkg.IsPlatformPackage(name) {
			continue
		}
		out[name] = constraint
	}
	return out
}

// cacheEntry stores a cached API response with its ETag.
type cacheEntry struct {
	etag     string
	versions []VersionEntry
	notFound bool
}

// Client fetches package metadata from a Packagist-compatible API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	mu         sync.RWMutex
	cache      map[string]cacheEntry
}

const defaultHTTPTimeout = 10 * time.Second

// NewClient creates a Packagist client. baseURL is typically "https://repo.packagist.org".
func NewClient(baseURL string) *Client {
	return NewClientWithHTTPClient(baseURL, &http.Client{Timeout: defaultHTTPTimeout})
}

// NewClientWithHTTPClient creates a Packagist client with a custom HTTP client.
func NewClientWithHTTPClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		cache:      make(map[string]cacheEntry),
	}
}

// GetPackage fetches all versions of a package from Packagist.
// Uses ETag-based HTTP caching to avoid redundant downloads.
func (c *Client) GetPackage(ctx context.Context, name string) ([]VersionEntry, error) {
	url := fmt.Sprintf("%s/p2/%s.json", c.baseURL, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("packagist: create request for %s: %w", name, err)
	}

	// Send If-None-Match if we have a cached ETag.
	c.mu.RLock()
	cached, hasCached := c.cache[name]
	c.mu.RUnlock()
	if hasCached && cached.notFound {
		return nil, fmt.Errorf("packagist: package %s not found", name)
	}
	if hasCached && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("packagist: fetch %s: %w", name, err)
	}
	defer resp.Body.Close()

	// 304 Not Modified — return cached data.
	if resp.StatusCode == http.StatusNotModified && hasCached {
		return cached.versions, nil
	}

	if resp.StatusCode == http.StatusNotFound {
		c.mu.Lock()
		c.cache[name] = cacheEntry{notFound: true}
		c.mu.Unlock()
		return nil, fmt.Errorf("packagist: package %s not found", name)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("packagist: fetch %s: HTTP %d", name, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("packagist: read response for %s: %w", name, err)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("packagist: unmarshal %s: %w", name, err)
	}

	versions, ok := apiResp.Packages[name]
	if !ok {
		return nil, fmt.Errorf("packagist: package %s not found in response", name)
	}

	// Cache the result with ETag.
	etag := resp.Header.Get("ETag")
	c.mu.Lock()
	c.cache[name] = cacheEntry{etag: etag, versions: versions}
	c.mu.Unlock()

	return versions, nil
}
