package packagist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/van-sprundel/vif/internal/composerauth"
	"github.com/van-sprundel/vif/internal/pkg"
)

// APIResponse is the top-level Packagist p2 response.
type APIResponse struct {
	Packages map[string][]VersionEntry `json:"packages"`
}

// UnmarshalJSON accepts both Packagist's array-shaped p2 responses and
// GitLab-style object-shaped package maps keyed by version.
func (r *APIResponse) UnmarshalJSON(data []byte) error {
	type rawAPIResponse struct {
		Packages rawPackageMap `json:"packages"`
	}

	var raw rawAPIResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if len(raw.Packages) == 0 {
		r.Packages = nil
		return nil
	}

	r.Packages = make(map[string][]VersionEntry, len(raw.Packages))
	for name := range raw.Packages {
		versions, ok, err := decodeComposerPackages(raw.Packages, name)
		if err != nil {
			return err
		}
		if ok {
			r.Packages[name] = versions
		}
	}
	return nil
}

// VersionEntry is a single version of a package from Packagist.
type VersionEntry struct {
	Name              string          `json:"name"`
	Version           string          `json:"version"`
	VersionNormalized string          `json:"version_normalized"`
	Type              string          `json:"type"`
	Bin               StringList      `json:"bin"`
	Require           RelationMap     `json:"require"`
	RequireDev        RelationMap     `json:"require-dev"`
	Provide           RelationMap     `json:"provide"`
	Replace           RelationMap     `json:"replace"`
	Conflict          RelationMap     `json:"conflict"`
	Autoload          json.RawMessage `json:"autoload"`
	AutoloadDev       json.RawMessage `json:"autoload-dev"`
	Dist              DistEntry       `json:"dist"`
	Time              string          `json:"time"`
}

// StringList tolerates Composer fields that may be encoded as a single string,
// a list of strings, null, or an empty array.
type StringList []string

// UnmarshalJSON accepts either "bin.php" or ["bin.php"] shapes.
func (s *StringList) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "", "null", "[]", `""`:
		*s = nil
		return nil
	}

	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = []string{single}
		return nil
	}

	var many []string
	if err := json.Unmarshal(data, &many); err == nil {
		*s = many
		return nil
	}

	return fmt.Errorf("string list: unsupported JSON shape %s", strings.TrimSpace(string(data)))
}

// RelationMap stores Composer dependency relation fields, which are usually
// objects but can appear as null, empty arrays, booleans, or strings in some
// Packagist payloads. Non-object values are treated as empty.
type RelationMap map[string]string

// UnmarshalJSON tolerates mixed JSON shapes emitted by Packagist metadata.
func (m *RelationMap) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "", "null", "[]", "false", "true", `""`:
		*m = nil
		return nil
	}

	var obj map[string]string
	if err := json.Unmarshal(data, &obj); err == nil {
		*m = obj
		return nil
	}

	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*m = nil
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("relation map: %w", err)
	}

	out := make(map[string]string, len(raw))
	for key, value := range raw {
		var constraint string
		if err := json.Unmarshal(value, &constraint); err != nil {
			continue
		}
		out[key] = constraint
	}
	*m = out
	return nil
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

type composerRootResponse struct {
	Packages rawPackageMap          `json:"packages"`
	Includes map[string]includeEntry `json:"includes"`
}

type includeEntry struct {
	SHA1 string `json:"sha1"`
}

type rawPackageMap map[string]json.RawMessage

func (m *rawPackageMap) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "", "null", "[]":
		*m = nil
		return nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*m = obj
	return nil
}

// Client fetches package metadata from a Packagist-compatible API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	auth       *composerauth.Config
	mu         sync.RWMutex
	cache      map[string]cacheEntry
	authErr    bool
	rootLoaded bool
	rootErr    error
	root       rawPackageMap
	includes   []string
}

// Fetcher is the metadata interface used by the resolver.
type Fetcher interface {
	GetPackage(context.Context, string) ([]VersionEntry, error)
}

const defaultHTTPTimeout = 10 * time.Second

// ErrPackageNotFound marks a package as absent from the Packagist registry.
var ErrPackageNotFound = errors.New("packagist: package not found")

// ErrAuthRequired marks a repository as requiring credentials for metadata access.
var ErrAuthRequired = errors.New("packagist: authentication required")

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

// SetAuth configures Composer-style auth for metadata requests.
func (c *Client) SetAuth(cfg *composerauth.Config) {
	c.auth = cfg
}

// GetPackage fetches all versions of a package from Packagist.
// Uses ETag-based HTTP caching to avoid redundant downloads.
func (c *Client) GetPackage(ctx context.Context, name string) ([]VersionEntry, error) {
	c.mu.RLock()
	cached, hasCached := c.cache[name]
	authErr := c.authErr
	c.mu.RUnlock()
	if authErr {
		return nil, fmt.Errorf("%w: %s", ErrAuthRequired, name)
	}
	if hasCached && cached.notFound {
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
	}
	if hasCached && cached.etag == "" && len(cached.versions) > 0 {
		return cached.versions, nil
	}

	versions, err := c.getPackageP2(ctx, name, cached, hasCached)
	if err == nil {
		return versions, nil
	}
	if !errors.Is(err, ErrPackageNotFound) {
		return nil, err
	}

	versions, err = c.getPackageFromRootIncludes(ctx, name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[name] = cacheEntry{versions: versions}
	c.mu.Unlock()
	return versions, nil
}

func (c *Client) getPackageP2(ctx context.Context, name string, cached cacheEntry, hasCached bool) ([]VersionEntry, error) {
	url := fmt.Sprintf("%s/p2/%s.json", c.baseURL, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("packagist: create request for %s: %w", name, err)
	}
	if hasCached && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}
	c.auth.ApplyRequest(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("packagist: fetch %s: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && hasCached {
		return cached.versions, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.mu.Lock()
		c.authErr = true
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAuthRequired, name)
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
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
	}

	etag := resp.Header.Get("ETag")
	c.mu.Lock()
	c.cache[name] = cacheEntry{etag: etag, versions: versions}
	c.mu.Unlock()

	return versions, nil
}

func (c *Client) getPackageFromRootIncludes(ctx context.Context, name string) ([]VersionEntry, error) {
	rootPackages, includeURLs, err := c.loadRootMetadata(ctx)
	if err != nil {
		if errors.Is(err, ErrPackageNotFound) {
			c.mu.Lock()
			c.cache[name] = cacheEntry{notFound: true}
			c.mu.Unlock()
		}
		return nil, err
	}

	if versions, ok, err := decodeComposerPackages(rootPackages, name); err != nil {
		return nil, err
	} else if ok {
		return versions, nil
	}

	for _, includeURL := range includeURLs {
		data, err := c.fetchJSON(ctx, includeURL)
		if err != nil {
			return nil, fmt.Errorf("packagist: fetch include for %s: %w", name, err)
		}
		var includeResp composerRootResponse
		if err := json.Unmarshal(data, &includeResp); err != nil {
			return nil, fmt.Errorf("packagist: unmarshal include for %s: %w", name, err)
		}
		if versions, ok, err := decodeComposerPackages(includeResp.Packages, name); err != nil {
			return nil, err
		} else if ok {
			return versions, nil
		}
	}

	c.mu.Lock()
	c.cache[name] = cacheEntry{notFound: true}
	c.mu.Unlock()
	return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
}

func (c *Client) loadRootMetadata(ctx context.Context) (rawPackageMap, []string, error) {
	c.mu.RLock()
	if c.rootLoaded {
		packages := cloneRawMessageMap(c.root)
		includeURLs := append([]string(nil), c.includes...)
		rootErr := c.rootErr
		c.mu.RUnlock()
		return packages, includeURLs, rootErr
	}
	c.mu.RUnlock()

	data, err := c.fetchJSON(ctx, c.baseURL+"/packages.json")
	if err != nil {
		c.mu.Lock()
		c.rootLoaded = true
		c.rootErr = err
		c.mu.Unlock()
		return nil, nil, err
	}

	var root composerRootResponse
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("packagist: unmarshal packages.json: %w", err)
	}

	includeURLs := make([]string, 0, len(root.Includes))
	for path := range root.Includes {
		includeURLs = append(includeURLs, strings.TrimRight(c.baseURL, "/")+"/"+strings.TrimLeft(path, "/"))
	}

	c.mu.Lock()
	c.rootLoaded = true
	c.rootErr = nil
	c.root = cloneRawMessageMap(root.Packages)
	c.includes = includeURLs
	c.mu.Unlock()
	return root.Packages, append([]string(nil), includeURLs...), nil
}

func (c *Client) fetchJSON(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("packagist: create request %s: %w", url, err)
	}
	c.auth.ApplyRequest(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("packagist: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.mu.Lock()
		c.authErr = true
		c.mu.Unlock()
		return nil, ErrAuthRequired
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPackageNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("packagist: fetch %s: HTTP %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("packagist: read response %s: %w", url, err)
	}
	return body, nil
}

func decodeComposerPackages(packages rawPackageMap, name string) ([]VersionEntry, bool, error) {
	if len(packages) == 0 {
		return nil, false, nil
	}
	raw, ok := packages[name]
	if !ok {
		return nil, false, nil
	}

	var list []VersionEntry
	if err := json.Unmarshal(raw, &list); err == nil {
		normalizeVersionEntries(name, list)
		return list, true, nil
	}

	var byVersion map[string]VersionEntry
	if err := json.Unmarshal(raw, &byVersion); err != nil {
		return nil, false, fmt.Errorf("packagist: unmarshal %s: %w", name, err)
	}

	versions := make([]VersionEntry, 0, len(byVersion))
	for _, version := range byVersion {
		versions = append(versions, version)
	}
	normalizeVersionEntries(name, versions)
	return versions, true, nil
}

func normalizeVersionEntries(name string, versions []VersionEntry) {
	for i := range versions {
		if versions[i].Name == "" {
			versions[i].Name = name
		}
	}
}

func cloneRawMessageMap(src rawPackageMap) rawPackageMap {
	if len(src) == 0 {
		return nil
	}
	dst := make(rawPackageMap, len(src))
	for key, value := range src {
		dst[key] = append(json.RawMessage(nil), value...)
	}
	return dst
}
