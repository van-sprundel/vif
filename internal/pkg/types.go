package pkg

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Package represents a single entry from composer.lock packages or packages-dev.
type Package struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Type        string   `json:"type"`
	Bin         []string `json:"bin"`
	IncludePath []string `json:"include-path"`
	Dist        Dist     `json:"dist"`
	Source      Dist     `json:"source"`
	Autoload    Autoload `json:"autoload"`
	AutoloadDev Autoload `json:"autoload-dev"`
}

// Dist holds the distribution metadata for downloading a package archive.
type Dist struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Reference string `json:"reference"`
	Shasum    string `json:"shasum"`
}

// Autoload holds the autoloading configuration for a package.
// PSR4 and PSR0 use the normalized form map[string][]string.
// Composer allows values to be either a single string or an array of strings;
// UnmarshalJSON normalizes both forms to []string.
type Autoload struct {
	PSR4                map[string][]string `json:"psr-4"`
	PSR0                map[string][]string `json:"psr-0"`
	Classmap            []string            `json:"classmap"`
	Files               []string            `json:"files"`
	ExcludeFromClassmap []string            `json:"exclude-from-classmap"`
}

// autoloadRaw is the wire format before normalization.
type autoloadRaw struct {
	PSR4                map[string]json.RawMessage `json:"psr-4"`
	PSR0                map[string]json.RawMessage `json:"psr-0"`
	Classmap            []string                   `json:"classmap"`
	Files               []string                   `json:"files"`
	ExcludeFromClassmap []string                   `json:"exclude-from-classmap"`
}

// UnmarshalJSON implements json.Unmarshaler for Autoload.
// It normalizes composer's mixed string/[]string PSR-4 and PSR-0 values into
// []string so callers always see map[string][]string.
func (a *Autoload) UnmarshalJSON(data []byte) error {
	var raw autoloadRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("autoload: %w", err)
	}

	a.Classmap = raw.Classmap
	a.Files = raw.Files
	a.ExcludeFromClassmap = raw.ExcludeFromClassmap

	var err error
	a.PSR4, err = normalizeStringMap(raw.PSR4)
	if err != nil {
		return fmt.Errorf("autoload psr-4: %w", err)
	}
	a.PSR0, err = normalizeStringMap(raw.PSR0)
	if err != nil {
		return fmt.Errorf("autoload psr-0: %w", err)
	}

	return nil
}

// normalizeStringMap converts a map whose values are either a JSON string or a
// JSON array of strings into map[string][]string.
func normalizeStringMap(raw map[string]json.RawMessage) (map[string][]string, error) {
	if raw == nil {
		return nil, nil
	}
	out := make(map[string][]string, len(raw))
	for k, v := range raw {
		var single string
		if err := json.Unmarshal(v, &single); err == nil {
			out[k] = []string{single}
			continue
		}
		var multi []string
		if err := json.Unmarshal(v, &multi); err != nil {
			return nil, fmt.Errorf("key %q: expected string or []string, got %s", k, v)
		}
		out[k] = multi
	}
	return out, nil
}

// RequiresDownload reports whether a package should be fetched via HTTP dist.
func RequiresDownload(p Package) bool {
	if p.Type == "metapackage" {
		return false
	}
	if p.Dist.Type == "path" {
		return strings.TrimSpace(p.Dist.URL) != ""
	}
	return strings.TrimSpace(p.Dist.URL) != ""
}

// RequiresGitClone reports whether a package has no dist but has a git source
// that can be cloned.
func RequiresGitClone(p Package) bool {
	if p.Type == "metapackage" {
		return false
	}
	if strings.TrimSpace(p.Dist.URL) != "" {
		return false
	}
	return p.Source.Type == "git" && strings.TrimSpace(p.Source.URL) != ""
}

// IsInstallable reports whether a package can be installed by any supported method.
func IsInstallable(p Package) bool {
	return p.Type == "metapackage" || RequiresDownload(p) || RequiresGitClone(p)
}
