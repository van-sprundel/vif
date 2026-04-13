package composer

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/van-sprundel/vif/internal/pkg"
)

// ComposerJSON represents a parsed composer.json file.
type ComposerJSON struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Type             string            `json:"type"`
	Require          map[string]string `json:"require"`
	RequireDev       map[string]string `json:"require-dev"`
	Conflict         map[string]string `json:"conflict"`
	Replace          map[string]string `json:"replace"`
	Provide          map[string]string `json:"provide"`
	Repositories     []Repository      `json:"-"`
	Autoload         pkg.Autoload      `json:"autoload"`
	AutoloadDev      pkg.Autoload      `json:"autoload-dev"`
	MinimumStability string            `json:"minimum-stability"`
	PreferStable     bool              `json:"prefer-stable"`
	Config           composerConfig    `json:"config"`
	Scripts          Scripts           `json:"scripts"`
	Extra            composerExtra     `json:"extra"`

	// raw holds the original decoded JSON for content-hash computation.
	raw map[string]json.RawMessage

	// reposRaw holds the raw repositories JSON for deferred parsing.
	reposRaw json.RawMessage
}

// composerConfig holds the config section of composer.json.
type composerConfig struct {
	OptimizeAutoloader bool        `json:"optimize-autoloader"`
	PrependAutoloader  *bool       `json:"prepend-autoloader,omitempty"`
	PlatformCheck      *boolOrStr  `json:"platform-check,omitempty"`
	Platform           platformMap `json:"platform,omitempty"`
}

type platformMap map[string]platformValue

type platformValue struct {
	String   string
	Bool     bool
	IsString bool
	IsBool   bool
}

func (v *platformValue) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v.String = s
		v.IsString = true
		v.IsBool = false
		return nil
	}

	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		v.Bool = b
		v.IsBool = true
		v.IsString = false
		return nil
	}

	return fmt.Errorf("platform value: unsupported JSON shape %s", strings.TrimSpace(string(data)))
}

func (v platformValue) MarshalJSON() ([]byte, error) {
	if v.IsString {
		return json.Marshal(v.String)
	}
	if v.IsBool {
		return json.Marshal(v.Bool)
	}
	return []byte("null"), nil
}

type boolOrStr struct {
	Bool  bool
	Str   string
	IsStr bool
}

func (b *boolOrStr) UnmarshalJSON(data []byte) error {
	var raw bool
	if err := json.Unmarshal(data, &raw); err == nil {
		b.Bool = raw
		b.IsStr = false
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	b.Str = s
	b.IsStr = true
	return nil
}

func (b *boolOrStr) IsTrue() bool {
	if b == nil {
		return true
	}
	if b.IsStr {
		return false
	}
	return b.Bool
}

func (b *boolOrStr) IsPHPOnly() bool {
	if b == nil {
		return false
	}
	return b.IsStr && b.Str == "php-only"
}

type composerExtra struct {
	Symfony composerExtraSymfony `json:"symfony"`
}

type composerExtraSymfony struct {
	Require     string            `json:"require"`
	AutoScripts map[string]string `json:"auto-scripts"`
}

// Scripts maps event names (e.g. "post-install-cmd") to their handlers.
// Each handler can be a single string or an array of strings in composer.json.
type Scripts map[string][]string

// UnmarshalJSON handles string, []string, and object forms for each event.
// Object maps (e.g. Symfony Flex auto-scripts) are silently skipped since
// they are handled separately via extra.symfony.auto-scripts.
func (s *Scripts) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = make(Scripts, len(raw))
	for event, val := range raw {
		// Try array first.
		var arr []string
		if err := json.Unmarshal(val, &arr); err == nil {
			(*s)[event] = arr
			continue
		}
		// Single string.
		var str string
		if err := json.Unmarshal(val, &str); err == nil {
			(*s)[event] = []string{str}
			continue
		}
		// Object map (e.g. auto-scripts): skip silently.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(val, &obj); err != nil {
			return fmt.Errorf("scripts[%q]: unsupported format: %w", event, err)
		}
		// Not stored — object-form scripts are not executable as handlers.
	}
	return nil
}

// PrependAutoloaderOrDefault returns the prepend-autoloader setting, defaulting to true.
func (c composerConfig) PrependAutoloaderOrDefault() bool {
	if c.PrependAutoloader == nil {
		return true
	}
	return *c.PrependAutoloader
}

// UnmarshalJSON implements custom unmarshaling to capture the raw repositories JSON.
func (cj *ComposerJSON) UnmarshalJSON(data []byte) error {
	type alias ComposerJSON
	var a struct {
		alias
		ReposRaw json.RawMessage `json:"repositories"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*cj = ComposerJSON(a.alias)
	cj.reposRaw = a.ReposRaw
	return nil
}

func parseRepositories(raw json.RawMessage) ([]Repository, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Try array form first: [{"type":..., "url":...}, ...]
	var arr []Repository
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	// Object form: {"repo-name": {"type":..., "url":...}, ...}
	var obj map[string]Repository
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("repositories: expected array or object: %w", err)
	}
	repos := make([]Repository, 0, len(obj))
	for _, repo := range obj {
		repos = append(repos, repo)
	}
	return repos, nil
}

// Repository is a Composer repository entry supported by vif.
type Repository struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Parse reads and parses a composer.json file at the given path.
func Parse(path string) (*ComposerJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("composer: %w", err)
	}

	var cj ComposerJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return nil, fmt.Errorf("composer: %w", err)
	}

	// Decode raw for content-hash.
	if err := json.Unmarshal(data, &cj.raw); err != nil {
		return nil, fmt.Errorf("composer: %w", err)
	}

	// Parse repositories from the raw field (handles both array and object).
	cj.Repositories, err = parseRepositories(cj.reposRaw)
	if err != nil {
		return nil, fmt.Errorf("composer: %w", err)
	}

	// Apply defaults.
	if cj.MinimumStability == "" {
		cj.MinimumStability = "stable"
	}

	return &cj, nil
}

// NonPlatformRequire returns the require map with platform packages filtered out.
func (cj *ComposerJSON) NonPlatformRequire() map[string]string {
	return filterPlatform(cj.Require)
}

// NonPlatformRequireDev returns the require-dev map with platform packages filtered out.
func (cj *ComposerJSON) NonPlatformRequireDev() map[string]string {
	return filterPlatform(cj.RequireDev)
}

// PlatformRequire returns only platform requirements from require.
func (cj *ComposerJSON) PlatformRequire() map[string]string {
	return filterOnlyPlatform(cj.Require)
}

// PlatformRequireDev returns only platform requirements from require-dev.
func (cj *ComposerJSON) PlatformRequireDev() map[string]string {
	return filterOnlyPlatform(cj.RequireDev)
}

// PlatformOverrides returns string-valued config.platform overrides, such as
// pinned PHP or extension versions, for use during dependency resolution.
func (cj *ComposerJSON) PlatformOverrides() map[string]string {
	out := make(map[string]string, len(cj.Config.Platform))
	for name, value := range cj.Config.Platform {
		if !pkg.IsPlatformPackage(name) || !value.IsString {
			continue
		}
		out[name] = value.String
	}
	return out
}

// DisabledPlatformPackages returns config.platform entries explicitly set to false.
func (cj *ComposerJSON) DisabledPlatformPackages() map[string]bool {
	out := make(map[string]bool)
	for name, value := range cj.Config.Platform {
		if !pkg.IsPlatformPackage(name) || !value.IsBool || value.Bool {
			continue
		}
		out[name] = true
	}
	return out
}

func filterPlatform(deps map[string]string) map[string]string {
	out := make(map[string]string, len(deps))
	for name, constraint := range deps {
		if pkg.IsPlatformPackage(name) {
			continue
		}
		out[name] = constraint
	}
	return out
}

func filterOnlyPlatform(deps map[string]string) map[string]string {
	out := make(map[string]string, len(deps))
	for name, constraint := range deps {
		if !pkg.IsPlatformPackage(name) {
			continue
		}
		out[name] = constraint
	}
	return out
}

// contentHashKeys are the composer.json keys that Composer includes in its content-hash.
// See: https://github.com/composer/composer/blob/main/src/Composer/Package/Locker.php
var contentHashKeys = []string{
	"name",
	"version",
	"require",
	"require-dev",
	"conflict",
	"replace",
	"provide",
	"minimum-stability",
	"prefer-stable",
	"repositories",
	"extra",
}

// ContentHash computes the Composer content-hash (md5 of relevant sorted JSON keys).
func (cj *ComposerJSON) ContentHash() string {
	relevant := make(map[string]json.RawMessage)
	for _, key := range contentHashKeys {
		if val, ok := cj.raw[key]; ok {
			relevant[key] = val
		}
	}
	if configRaw, ok := cj.raw["config"]; ok {
		var config map[string]json.RawMessage
		if err := json.Unmarshal(configRaw, &config); err == nil {
			if platformRaw, ok := config["platform"]; ok {
				relevant["config"] = json.RawMessage(`{"platform":` + string(platformRaw) + `}`)
			}
		}
	}

	keys := make([]string, 0, len(relevant))
	for k := range relevant {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyJSON, _ := marshalJSON(k)
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(phpJSON(relevant[k]))
	}
	buf.WriteByte('}')

	hash := md5.Sum([]byte(buf.String()))
	return fmt.Sprintf("%x", hash)
}

func phpJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return raw
	}

	return bytes.ReplaceAll(compact.Bytes(), []byte("/"), []byte(`\/`))
}

// AddRequire adds or updates a package in the require section.
func (cj *ComposerJSON) AddRequire(name, constraint string) {
	if cj.Require == nil {
		cj.Require = make(map[string]string)
	}
	cj.Require[name] = constraint
	cj.syncRaw("require", cj.Require)
}

// AddRequireDev adds or updates a package in the require-dev section.
func (cj *ComposerJSON) AddRequireDev(name, constraint string) {
	if cj.RequireDev == nil {
		cj.RequireDev = make(map[string]string)
	}
	cj.RequireDev[name] = constraint
	cj.syncRaw("require-dev", cj.RequireDev)
}

// RemoveRequire removes a package from the require section.
func (cj *ComposerJSON) RemoveRequire(name string) {
	delete(cj.Require, name)
	cj.syncRaw("require", cj.Require)
}

// RemoveRequireDev removes a package from the require-dev section.
func (cj *ComposerJSON) RemoveRequireDev(name string) {
	delete(cj.RequireDev, name)
	cj.syncRaw("require-dev", cj.RequireDev)
}

// syncRaw updates the raw JSON map for content-hash recomputation.
func (cj *ComposerJSON) syncRaw(key string, val interface{}) {
	data, err := marshalJSON(val)
	if err != nil {
		return
	}
	if cj.raw == nil {
		cj.raw = make(map[string]json.RawMessage)
	}
	cj.raw[key] = json.RawMessage(data)
}

// composerKeyOrder defines the conventional key ordering for composer.json.
// Keys not in this list are appended alphabetically after the known keys.
var composerKeyOrder = []string{
	"name",
	"description",
	"version",
	"type",
	"keywords",
	"homepage",
	"readme",
	"time",
	"license",
	"authors",
	"support",
	"funding",
	"require",
	"require-dev",
	"conflict",
	"replace",
	"provide",
	"suggest",
	"autoload",
	"autoload-dev",
	"repositories",
	"minimum-stability",
	"prefer-stable",
	"config",
	"scripts",
	"extra",
	"bin",
	"archive",
	"non-feature-branches",
}

// Write writes the composer.json back to the given path, preserving all
// original keys and updating require/require-dev with deterministic key order.
func (cj *ComposerJSON) Write(path string) error {
	// Build output from raw, overriding require/require-dev.
	out := make(map[string]json.RawMessage, len(cj.raw))
	for k, v := range cj.raw {
		out[k] = v
	}

	// Override require and require-dev with current values.
	if cj.Require != nil {
		data, _ := marshalJSON(cj.Require)
		out["require"] = json.RawMessage(data)
	}
	if cj.RequireDev != nil {
		data, _ := marshalJSON(cj.RequireDev)
		out["require-dev"] = json.RawMessage(data)
	}

	data, err := marshalOrdered(out)
	if err != nil {
		return fmt.Errorf("composer: marshal: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("composer: write %q: %w", path, err)
	}
	return nil
}

// marshalOrdered produces indented JSON with keys in Composer's conventional order.
func marshalOrdered(m map[string]json.RawMessage) ([]byte, error) {
	// Collect keys in order: known keys first, then unknown keys sorted.
	seen := make(map[string]bool, len(m))
	var keys []string
	for _, k := range composerKeyOrder {
		if _, ok := m[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var extra []string
	for k := range m {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)

	var buf strings.Builder
	buf.WriteString("{\n")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",\n")
		}
		keyJSON, _ := marshalJSON(k)
		// Indent the value properly.
		val, err := indentValue(m[k])
		if err != nil {
			return nil, err
		}
		buf.WriteString("    ")
		buf.Write(keyJSON)
		buf.WriteString(": ")
		buf.Write(val)
	}
	buf.WriteString("\n}")
	return []byte(buf.String()), nil
}

// indentValue re-indents a JSON value with 4-space indentation at depth 1.
func indentValue(raw json.RawMessage) ([]byte, error) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	data, err := marshalJSONIndent(v, "    ", "    ")
	if err != nil {
		return nil, err
	}
	return data, nil
}

func marshalJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func marshalJSONIndent(v interface{}, prefix, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
