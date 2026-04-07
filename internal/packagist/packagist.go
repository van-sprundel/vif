package packagist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/van-sprundel/vif/internal/composerauth"
	"github.com/van-sprundel/vif/internal/pkg"
)

// APIResponse is the top-level Packagist p2 response.
type APIResponse struct {
	Minified string                    `json:"minified"`
	Packages map[string][]VersionEntry `json:"packages"`
}

// UnmarshalJSON accepts both Packagist's array-shaped p2 responses and
// GitLab-style object-shaped package maps keyed by version.
func (r *APIResponse) UnmarshalJSON(data []byte) error {
	type rawAPIResponse struct {
		Minified string        `json:"minified"`
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

	r.Minified = raw.Minified
	r.Packages = make(map[string][]VersionEntry, len(raw.Packages))
	for name := range raw.Packages {
		versions, ok, err := decodeComposerPackages(raw.Packages, name, raw.Minified == "composer/2.0")
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
	Source            DistEntry       `json:"source"`
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

// MetadataCache is an optional persistent store for P2 metadata responses.
// cache.Cache satisfies this interface directly.
type MetadataCache interface {
	LookupMetadata(repoURL, packageName string) (etag string, body []byte, ok bool, err error)
	InsertMetadata(repoURL, packageName, etag string, body []byte) error
}

// cacheEntry stores a cached API response with its ETag.
type cacheEntry struct {
	etag      string
	versions  []VersionEntry
	notFound  bool
	devMerged bool
}

type composerRootResponse struct {
	Minified    string                  `json:"minified"`
	MetadataURL string                  `json:"metadata-url"`
	Packages    rawPackageMap           `json:"packages"`
	Includes    map[string]includeEntry `json:"includes"`
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
	baseURL      string
	httpClient   *http.Client
	auth         *composerauth.Config
	metaCache    MetadataCache // nil = no persistent cache
	mu           sync.RWMutex
	cache        map[string]cacheEntry
	authErr      bool
	rootLoaded   bool
	rootErr      error
	root         rawPackageMap
	rootMinified bool
	metadataURL  string
	includes     []string
	// vendorStates learns which vendor prefixes are likely absent on this source.
	vendorStates sync.Map // key: vendor prefix -> *vendorState
}

type vendorState struct {
	hits   atomic.Int32
	misses atomic.Int32
}

// Fetcher is the metadata interface used by the resolver.
type Fetcher interface {
	GetPackage(context.Context, string) ([]VersionEntry, error)
}

// DevFetcher optionally allows callers to skip separate ~dev metadata lookups
// until a package actually needs dev candidates.
type DevFetcher interface {
	GetPackageWithDev(context.Context, string, bool) ([]VersionEntry, error)
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

// RepositoryLabel identifies this repository in trace output.
func (c *Client) RepositoryLabel() string {
	return c.baseURL
}

// MatchPackage narrows lookups for well-known repositories that only host
// vendor prefixes this source has already shown to be absent.
func (c *Client) MatchPackage(name string) bool {
	vendor := vendorPrefix(name)
	if vendor == "" {
		return true
	}
	val, ok := c.vendorStates.Load(vendor)
	if !ok {
		return true
	}
	state := val.(*vendorState)
	return !(state.misses.Load() > 0 && state.hits.Load() == 0)
}

// SetAuth configures Composer-style auth for metadata requests.
func (c *Client) SetAuth(cfg *composerauth.Config) {
	c.auth = cfg
}

// SetMetadataCache wires a persistent metadata cache into the client.
// When set, getPackageP2 will seed its ETag from the cache on cache misses
// and persist fresh 200 responses back to the cache.
func (c *Client) SetMetadataCache(mc MetadataCache) {
	c.metaCache = mc
}

const metadataMaxRetries = 3

func (c *Client) doWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	baseDelay := 500 * time.Millisecond

	var lastErr error
	for attempt := range metadataMaxRetries {
		if attempt > 0 {
			delay := baseDelay * time.Duration(1<<(attempt-1))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if !isRetryableMetadataStatus(resp.StatusCode) || attempt == metadataMaxRetries-1 {
			return resp, nil
		}

		resp.Body.Close()

		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, parseErr := strconv.Atoi(ra); parseErr == nil && secs > 0 && secs <= 60 {
				delay := time.Duration(secs) * time.Second
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
				continue
			}
		}
	}

	return nil, fmt.Errorf("packagist: %d attempts failed: %w", metadataMaxRetries, lastErr)
}

func isRetryableMetadataStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return code >= 500 && code <= 599
	}
}

// GetPackage fetches all versions of a package from Packagist (stable + dev).
// Uses ETag-based HTTP caching to avoid redundant downloads.
func (c *Client) GetPackage(ctx context.Context, name string) ([]VersionEntry, error) {
	return c.GetPackageWithDev(ctx, name, true)
}

// GetPackageWithDev fetches package versions and optionally merges the
// dedicated ~dev metadata endpoint.
func (c *Client) GetPackageWithDev(ctx context.Context, name string, includeDev bool) ([]VersionEntry, error) {
	c.mu.RLock()
	cached, hasCached := c.cache[name]
	authErr := c.authErr
	c.mu.RUnlock()
	if authErr {
		return nil, fmt.Errorf("%w: %s", ErrAuthRequired, name)
	}
	if hasCached && cached.notFound {
		c.recordVendorResult(name, ErrPackageNotFound)
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
	}
	if hasCached && (cached.devMerged || !includeDev) {
		c.recordVendorResult(name, nil)
		return cached.versions, nil
	}

	versions, err := c.getPackageP2(ctx, name, cached, hasCached)
	if err == nil {
		c.recordVendorResult(name, nil)
		if !includeDev {
			c.mu.Lock()
			entry := c.cache[name]
			entry.versions = versions
			c.cache[name] = entry
			c.mu.Unlock()
			return versions, nil
		}
		return c.mergeDevVersions(ctx, name, versions), nil
	}
	if !errors.Is(err, ErrPackageNotFound) {
		return nil, err
	}
	useIncludeFallback, fallbackErr := c.shouldUseIncludeFallback(ctx)
	if fallbackErr != nil {
		return nil, fallbackErr
	}
	if !useIncludeFallback {
		c.recordVendorResult(name, ErrPackageNotFound)
		c.mu.Lock()
		c.cache[name] = cacheEntry{notFound: true}
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, name)
	}

	versions, err = c.getPackageFromRootIncludes(ctx, name)
	if err != nil {
		if errors.Is(err, ErrPackageNotFound) {
			c.recordVendorResult(name, ErrPackageNotFound)
		}
		return nil, err
	}
	c.recordVendorResult(name, nil)

	c.mu.Lock()
	c.cache[name] = cacheEntry{versions: versions}
	c.mu.Unlock()
	if !includeDev {
		return versions, nil
	}
	return c.mergeDevVersions(ctx, name, versions), nil
}

func (c *Client) recordVendorResult(name string, err error) {
	vendor := vendorPrefix(name)
	if vendor == "" {
		return
	}
	val, _ := c.vendorStates.LoadOrStore(vendor, &vendorState{})
	state := val.(*vendorState)
	if err == nil {
		state.hits.Add(1)
		return
	}
	if errors.Is(err, ErrPackageNotFound) {
		state.misses.Add(1)
	}
}

// WarmupMatchScope seeds vendor availability from packages.json when the
// repository exposes package names there. Failures are intentionally ignored.
func (c *Client) WarmupMatchScope(ctx context.Context) {
	rootPackages, _, _, err := c.loadRootMetadata(ctx)
	if err != nil || len(rootPackages) == 0 {
		return
	}
	for name := range rootPackages {
		c.recordVendorResult(name, nil)
	}
}

func (c *Client) mergeDevVersions(ctx context.Context, name string, versions []VersionEntry) []VersionEntry {
	devVersions := c.fetchP2Dev(ctx, name)
	merged := mergeVersionEntries(versions, devVersions)
	c.mu.Lock()
	entry := c.cache[name]
	entry.versions = merged
	entry.devMerged = true
	c.cache[name] = entry
	c.mu.Unlock()
	return merged
}

func (c *Client) getPackageP2(ctx context.Context, name string, cached cacheEntry, hasCached bool) ([]VersionEntry, error) {
	// If no in-memory cache hit, try the persistent cache to seed the ETag.
	if !hasCached && c.metaCache != nil {
		etag, body, ok, err := c.metaCache.LookupMetadata(c.baseURL, name)
		if err == nil && ok {
			var apiResp APIResponse
			if jsonErr := json.Unmarshal(body, &apiResp); jsonErr == nil {
				if versions, found := apiResp.Packages[name]; found {
					cached = cacheEntry{etag: etag, versions: versions}
					hasCached = true
					c.mu.Lock()
					c.cache[name] = cached
					c.mu.Unlock()
				}
			}
			// On unmarshal failure, fall through to a fresh HTTP request.
		}
	}

	url := fmt.Sprintf("%s/p2/%s.json", c.baseURL, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("packagist: create request for %s: %w", name, err)
	}
	if hasCached && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}
	c.auth.ApplyRequest(req)

	resp, err := c.doWithRetry(ctx, req)
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

	if c.metaCache != nil {
		_ = c.metaCache.InsertMetadata(c.baseURL, name, etag, body)
	}

	return versions, nil
}

func (c *Client) fetchP2Dev(ctx context.Context, name string) []VersionEntry {
	devKey := name + "~dev"

	c.mu.RLock()
	cached, hasCached := c.cache[devKey]
	c.mu.RUnlock()

	if hasCached {
		if cached.notFound || len(cached.versions) > 0 {
			return cached.versions
		}
	}

	if !hasCached && c.metaCache != nil {
		etag, body, ok, err := c.metaCache.LookupMetadata(c.baseURL, devKey)
		if err == nil && ok {
			var apiResp APIResponse
			if jsonErr := json.Unmarshal(body, &apiResp); jsonErr == nil {
				if versions, found := apiResp.Packages[name]; found {
					cached = cacheEntry{etag: etag, versions: versions}
					hasCached = true
					c.mu.Lock()
					c.cache[devKey] = cached
					c.mu.Unlock()
				}
			}
		}
	}

	url := fmt.Sprintf("%s/p2/%s~dev.json", c.baseURL, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if hasCached && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}
	c.auth.ApplyRequest(req)

	resp, err := c.doWithRetry(ctx, req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && hasCached {
		return cached.versions
	}
	if resp.StatusCode == http.StatusNotFound {
		c.mu.Lock()
		c.cache[devKey] = cacheEntry{notFound: true}
		c.mu.Unlock()
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil
	}

	versions, ok := apiResp.Packages[name]
	if !ok {
		return nil
	}

	etag := resp.Header.Get("ETag")
	c.mu.Lock()
	c.cache[devKey] = cacheEntry{etag: etag, versions: versions}
	c.mu.Unlock()

	if c.metaCache != nil {
		_ = c.metaCache.InsertMetadata(c.baseURL, devKey, etag, body)
	}

	return versions
}

func mergeVersionEntries(stable, dev []VersionEntry) []VersionEntry {
	if len(dev) == 0 {
		return stable
	}
	seen := make(map[string]struct{}, len(stable)+len(dev))
	merged := make([]VersionEntry, 0, len(stable)+len(dev))
	for _, v := range stable {
		if _, ok := seen[v.Version]; !ok {
			seen[v.Version] = struct{}{}
			merged = append(merged, v)
		}
	}
	for _, v := range dev {
		if _, ok := seen[v.Version]; !ok {
			seen[v.Version] = struct{}{}
			merged = append(merged, v)
		}
	}
	return merged
}

func (c *Client) getPackageFromRootIncludes(ctx context.Context, name string) ([]VersionEntry, error) {
	rootPackages, includeURLs, rootMinified, err := c.loadRootMetadata(ctx)
	if err != nil {
		if errors.Is(err, ErrPackageNotFound) {
			c.mu.Lock()
			c.cache[name] = cacheEntry{notFound: true}
			c.mu.Unlock()
		}
		return nil, err
	}

	if versions, ok, err := decodeComposerPackages(rootPackages, name, rootMinified); err != nil {
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
		if versions, ok, err := decodeComposerPackages(includeResp.Packages, name, includeResp.Minified == "composer/2.0"); err != nil {
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

func (c *Client) loadRootMetadata(ctx context.Context) (rawPackageMap, []string, bool, error) {
	c.mu.RLock()
	if c.rootLoaded {
		packages := cloneRawMessageMap(c.root)
		includeURLs := append([]string(nil), c.includes...)
		rootErr := c.rootErr
		c.mu.RUnlock()
		return packages, includeURLs, c.rootMinified, rootErr
	}
	c.mu.RUnlock()

	data, err := c.fetchJSON(ctx, c.baseURL+"/packages.json")
	if err != nil {
		c.mu.Lock()
		c.rootLoaded = true
		c.rootErr = err
		c.mu.Unlock()
		return nil, nil, false, err
	}

	var root composerRootResponse
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, false, fmt.Errorf("packagist: unmarshal packages.json: %w", err)
	}

	includeURLs := make([]string, 0, len(root.Includes))
	for path := range root.Includes {
		includeURLs = append(includeURLs, strings.TrimRight(c.baseURL, "/")+"/"+strings.TrimLeft(path, "/"))
	}

	c.mu.Lock()
	c.rootLoaded = true
	c.rootErr = nil
	c.root = cloneRawMessageMap(root.Packages)
	c.rootMinified = root.Minified == "composer/2.0"
	c.metadataURL = strings.TrimSpace(root.MetadataURL)
	c.includes = includeURLs
	c.mu.Unlock()
	return root.Packages, append([]string(nil), includeURLs...), root.Minified == "composer/2.0", nil
}

func (c *Client) shouldUseIncludeFallback(ctx context.Context) (bool, error) {
	_, includeURLs, _, err := c.loadRootMetadata(ctx)
	if err != nil {
		if errors.Is(err, ErrAuthRequired) {
			return false, err
		}
		return false, nil
	}
	c.mu.RLock()
	metadataURL := c.metadataURL
	c.mu.RUnlock()
	// Composer 2 repositories advertise metadata-url; p2 404 can be treated as final.
	if metadataURL != "" {
		return false, nil
	}
	return len(includeURLs) > 0, nil
}

func (c *Client) fetchJSON(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("packagist: create request %s: %w", url, err)
	}
	c.auth.ApplyRequest(req)

	resp, err := c.doWithRetry(ctx, req)
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

func decodeComposerPackages(packages rawPackageMap, name string, minified bool) ([]VersionEntry, bool, error) {
	if len(packages) == 0 {
		return nil, false, nil
	}
	raw, ok := packages[name]
	if !ok {
		return nil, false, nil
	}

	if minified {
		list, err := decodeMinifiedVersionList(raw, name)
		if err == nil {
			normalizeVersionEntries(name, list)
			return list, true, nil
		}
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

func decodeMinifiedVersionList(raw json.RawMessage, name string) ([]VersionEntry, error) {
	var rawList []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawList); err != nil {
		return nil, err
	}

	versions := make([]VersionEntry, 0, len(rawList))
	var prev map[string]json.RawMessage
	for _, current := range rawList {
		merged := make(map[string]json.RawMessage, len(prev)+len(current))
		for key, value := range prev {
			merged[key] = append(json.RawMessage(nil), value...)
		}
		for key, value := range current {
			if string(value) == `"__unset"` {
				delete(merged, key)
				continue
			}
			merged[key] = value
		}

		data, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("packagist: marshal minified %s entry: %w", name, err)
		}

		var entry VersionEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return nil, fmt.Errorf("packagist: unmarshal minified %s entry: %w", name, err)
		}
		versions = append(versions, entry)
		prev = merged
	}

	return versions, nil
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
