package composer

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ComposerJSON represents a parsed composer.json file.
type ComposerJSON struct {
	Name             string            `json:"name"`
	Type             string            `json:"type"`
	Require          map[string]string `json:"require"`
	RequireDev       map[string]string `json:"require-dev"`
	MinimumStability string            `json:"minimum-stability"`
	PreferStable     bool              `json:"prefer-stable"`
	Config           json.RawMessage   `json:"config"`

	// raw holds the original decoded JSON for content-hash computation.
	raw map[string]json.RawMessage
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

func filterPlatform(deps map[string]string) map[string]string {
	out := make(map[string]string, len(deps))
	for name, constraint := range deps {
		if isPlatformPackage(name) {
			continue
		}
		out[name] = constraint
	}
	return out
}

func isPlatformPackage(name string) bool {
	return name == "php" ||
		strings.HasPrefix(name, "ext-") ||
		strings.HasPrefix(name, "lib-") ||
		name == "php-64bit" ||
		name == "php-ipv6" ||
		name == "hhvm" ||
		name == "composer-plugin-api" ||
		name == "composer-runtime-api"
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

	// Sort keys for deterministic output.
	keys := make([]string, 0, len(relevant))
	for k := range relevant {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build sorted JSON object.
	var buf strings.Builder
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyJSON, _ := json.Marshal(k)
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(relevant[k])
	}
	buf.WriteByte('}')

	hash := md5.Sum([]byte(buf.String()))
	return fmt.Sprintf("%x", hash)
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

// syncRaw updates the raw JSON map for content-hash recomputation.
func (cj *ComposerJSON) syncRaw(key string, val interface{}) {
	data, err := json.Marshal(val)
	if err != nil {
		return
	}
	if cj.raw == nil {
		cj.raw = make(map[string]json.RawMessage)
	}
	cj.raw[key] = json.RawMessage(data)
}

// Write writes the composer.json back to the given path, preserving all
// original keys and updating require/require-dev.
func (cj *ComposerJSON) Write(path string) error {
	// Build output from raw, overriding require/require-dev.
	out := make(map[string]json.RawMessage, len(cj.raw))
	for k, v := range cj.raw {
		out[k] = v
	}

	// Override require and require-dev with current values.
	if cj.Require != nil {
		data, _ := json.Marshal(cj.Require)
		out["require"] = json.RawMessage(data)
	}
	if cj.RequireDev != nil {
		data, _ := json.Marshal(cj.RequireDev)
		out["require-dev"] = json.RawMessage(data)
	}

	data, err := json.MarshalIndent(out, "", "    ")
	if err != nil {
		return fmt.Errorf("composer: marshal: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("composer: write %q: %w", path, err)
	}
	return nil
}
