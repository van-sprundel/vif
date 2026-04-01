package lockfile

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/resolver"
)

// LockFile represents the top-level structure of a composer.lock file.
type LockFile struct {
	ContentHash string        `json:"content-hash"`
	Packages    []pkg.Package `json:"packages"`
	PackagesDev []pkg.Package `json:"packages-dev"`
}

// lockFileOut is the full composer.lock JSON structure for writing.
type lockFileOut struct {
	Readme           []string        `json:"_readme"`
	ContentHash      string          `json:"content-hash"`
	Packages         []lockPkgEntry  `json:"packages"`
	PackagesDev      []lockPkgEntry  `json:"packages-dev"`
	Aliases          []interface{}   `json:"aliases"`
	MinimumStability string          `json:"minimum-stability"`
	StabilityFlags   json.RawMessage `json:"stability-flags"`
	PreferStable     bool            `json:"prefer-stable"`
	PreferLowest     bool            `json:"prefer-lowest"`
	Platform         json.RawMessage `json:"platform"`
	PlatformDev      json.RawMessage `json:"platform-dev"`
}

// lockPkgEntry is a single package entry in composer.lock.
type lockPkgEntry struct {
	Name       string              `json:"name"`
	Version    string              `json:"version"`
	Dist       packagist.DistEntry `json:"dist"`
	Require    map[string]string   `json:"require,omitempty"`
	RequireDev map[string]string   `json:"require-dev,omitempty"`
	Type       string              `json:"type,omitempty"`
	Autoload   json.RawMessage     `json:"autoload,omitempty"`
	Time       string              `json:"time,omitempty"`
}

// Parse reads the composer.lock file at path and returns a parsed LockFile.
// Errors are wrapped with context.
func Parse(path string) (*LockFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lockfile: read %q: %w", path, err)
	}

	var lf LockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("lockfile: unmarshal %q: %w", path, err)
	}

	return &lf, nil
}

// Generate creates a composer.lock file from resolved packages and writes it to path.
func Generate(path string, resolved []resolver.ResolvedPackage, cj *composer.ComposerJSON) error {
	var packages, packagesDev []lockPkgEntry
	for _, rp := range resolved {
		entry := lockPkgEntry{
			Name:       rp.Name,
			Version:    rp.Version,
			Dist:       rp.Entry.Dist,
			Require:    rp.Entry.Require,
			RequireDev: rp.Entry.RequireDev,
			Type:       rp.Entry.Type,
			Autoload:   rp.Entry.Autoload,
			Time:       rp.Entry.Time,
		}
		if rp.Dev {
			packagesDev = append(packagesDev, entry)
		} else {
			packages = append(packages, entry)
		}
	}

	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	sort.Slice(packagesDev, func(i, j int) bool { return packagesDev[i].Name < packagesDev[j].Name })

	// Ensure non-nil slices for JSON.
	if packages == nil {
		packages = []lockPkgEntry{}
	}
	if packagesDev == nil {
		packagesDev = []lockPkgEntry{}
	}

	lf := lockFileOut{
		Readme: []string{
			"This file locks the dependencies of your project to a known state",
			"Read more about it at https://getcomposer.org/doc/01-basic-usage.md#installing-dependencies",
			"This file is @generated automatically",
		},
		ContentHash:      cj.ContentHash(),
		Packages:         packages,
		PackagesDev:      packagesDev,
		Aliases:          []interface{}{},
		MinimumStability: cj.MinimumStability,
		StabilityFlags:   json.RawMessage(`{}`),
		PreferStable:     cj.PreferStable,
		PreferLowest:     false,
		Platform:         json.RawMessage(`{}`),
		PlatformDev:      json.RawMessage(`{}`),
	}

	data, err := json.MarshalIndent(lf, "", "    ")
	if err != nil {
		return fmt.Errorf("lockfile: marshal: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("lockfile: write %q: %w", path, err)
	}

	return nil
}
