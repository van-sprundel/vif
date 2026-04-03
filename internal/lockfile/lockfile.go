package lockfile

import (
	"bytes"
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
	Readme           []string          `json:"_readme"`
	ContentHash      string            `json:"content-hash"`
	Packages         []json.RawMessage `json:"packages"`
	PackagesDev      []json.RawMessage `json:"packages-dev"`
	Aliases          []interface{}     `json:"aliases"`
	MinimumStability string            `json:"minimum-stability"`
	StabilityFlags   json.RawMessage   `json:"stability-flags"`
	PreferStable     bool              `json:"prefer-stable"`
	PreferLowest     bool              `json:"prefer-lowest"`
	Platform         json.RawMessage   `json:"platform"`
	PlatformDev      json.RawMessage   `json:"platform-dev"`
	PluginAPIVersion string            `json:"plugin-api-version,omitempty"`
}

// lockPkgEntry is a single package entry in composer.lock.
type lockPkgEntry struct {
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Dist        packagist.DistEntry  `json:"dist"`
	Source      packagist.DistEntry  `json:"source,omitempty"`
	Require     map[string]string    `json:"require,omitempty"`
	RequireDev  map[string]string    `json:"require-dev,omitempty"`
	Provide     map[string]string    `json:"provide,omitempty"`
	Replace     map[string]string    `json:"replace,omitempty"`
	Conflict    map[string]string    `json:"conflict,omitempty"`
	Type        string               `json:"type,omitempty"`
	Bin         packagist.StringList `json:"bin,omitempty"`
	Autoload    json.RawMessage      `json:"autoload,omitempty"`
	AutoloadDev json.RawMessage      `json:"autoload-dev,omitempty"`
	Time        string               `json:"time,omitempty"`
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

// LockedEntries returns a map of package name to VersionEntry for all locked packages.
// This is used by the resolver to preserve VCS-only packages that have no Packagist releases.
func (lf *LockFile) LockedEntries() map[string]packagist.VersionEntry {
	entries := make(map[string]packagist.VersionEntry, len(lf.Packages)+len(lf.PackagesDev))

	for _, p := range lf.Packages {
		entries[p.Name] = packageToVersionEntry(p)
	}
	for _, p := range lf.PackagesDev {
		entries[p.Name] = packageToVersionEntry(p)
	}

	return entries
}

// packageToVersionEntry converts a lockfile Package to a packagist VersionEntry.
func packageToVersionEntry(p pkg.Package) packagist.VersionEntry {
	return packagist.VersionEntry{
		Name:              p.Name,
		Version:           p.Version,
		VersionNormalized: p.VersionNormalized,
		Type:              p.Type,
		Bin:               p.Bin,
		Require:           p.Require,
		RequireDev:        p.RequireDev,
		Provide:           p.Provide,
		Replace:           p.Replace,
		Conflict:          p.Conflict,
		Dist: packagist.DistEntry{
			Type:      p.Dist.Type,
			URL:       p.Dist.URL,
			Reference: p.Dist.Reference,
			Shasum:    p.Dist.Shasum,
		},
		Source: packagist.DistEntry{
			Type:      p.Source.Type,
			URL:       p.Source.URL,
			Reference: p.Source.Reference,
		},
		Time: p.Time,
	}
}

// Generate creates a composer.lock file from resolved packages and writes it to path.
func Generate(path string, resolved []resolver.ResolvedPackage, cj *composer.ComposerJSON) error {
	existing, _ := loadExistingLockfile(path)
	var packages, packagesDev []json.RawMessage
	for _, rp := range resolved {
		if raw, ok := existing.lookupPackage(rp.Name, rp.Version, rp.Dev); ok {
			if rp.Dev {
				packagesDev = append(packagesDev, raw)
			} else {
				packages = append(packages, raw)
			}
			continue
		}

		entry := lockPkgEntry{
			Name:        rp.Name,
			Version:     rp.Version,
			Dist:        rp.Entry.Dist,
			Source:      rp.Entry.Source,
			Require:     rp.Entry.Require,
			RequireDev:  rp.Entry.RequireDev,
			Provide:     rp.Entry.Provide,
			Replace:     rp.Entry.Replace,
			Conflict:    rp.Entry.Conflict,
			Type:        rp.Entry.Type,
			Bin:         rp.Entry.Bin,
			Autoload:    rp.Entry.Autoload,
			AutoloadDev: rp.Entry.AutoloadDev,
			Time:        rp.Entry.Time,
		}
		raw, err := marshalJSON(entry)
		if err != nil {
			return fmt.Errorf("lockfile: marshal package %s@%s: %w", rp.Name, rp.Version, err)
		}
		if rp.Dev {
			packagesDev = append(packagesDev, raw)
		} else {
			packages = append(packages, raw)
		}
	}

	sortRawPackages(packages)
	sortRawPackages(packagesDev)

	// Ensure non-nil slices for JSON.
	if packages == nil {
		packages = []json.RawMessage{}
	}
	if packagesDev == nil {
		packagesDev = []json.RawMessage{}
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
		Platform:         mustMarshalObject(cj.PlatformRequire()),
		PlatformDev:      mustMarshalObject(cj.PlatformRequireDev()),
		PluginAPIVersion: existing.pluginAPIVersion,
	}

	data, err := marshalJSONIndent(lf, "", "    ")
	if err != nil {
		return fmt.Errorf("lockfile: marshal: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("lockfile: write %q: %w", path, err)
	}

	return nil
}

type existingLockfile struct {
	packageByKey     map[string]json.RawMessage
	packageDevByKey  map[string]json.RawMessage
	pluginAPIVersion string
}

func loadExistingLockfile(path string) (existingLockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return existingLockfile{}, nil
		}
		return existingLockfile{}, fmt.Errorf("lockfile: read existing %q: %w", path, err)
	}

	var root struct {
		Packages         []json.RawMessage `json:"packages"`
		PackagesDev      []json.RawMessage `json:"packages-dev"`
		PluginAPIVersion string            `json:"plugin-api-version"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return existingLockfile{}, nil
	}

	return existingLockfile{
		packageByKey:     indexRawPackages(root.Packages),
		packageDevByKey:  indexRawPackages(root.PackagesDev),
		pluginAPIVersion: root.PluginAPIVersion,
	}, nil
}

func indexRawPackages(rawPackages []json.RawMessage) map[string]json.RawMessage {
	index := make(map[string]json.RawMessage, len(rawPackages))
	for _, raw := range rawPackages {
		var entry struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if entry.Name == "" || entry.Version == "" {
			continue
		}
		index[entry.Name+"@"+entry.Version] = raw
	}
	return index
}

func (e existingLockfile) lookupPackage(name, version string, dev bool) (json.RawMessage, bool) {
	key := name + "@" + version
	if dev {
		raw, ok := e.packageDevByKey[key]
		return raw, ok
	}
	raw, ok := e.packageByKey[key]
	return raw, ok
}

func sortRawPackages(rawPackages []json.RawMessage) {
	sort.Slice(rawPackages, func(i, j int) bool {
		return rawPackageName(rawPackages[i]) < rawPackageName(rawPackages[j])
	})
}

func rawPackageName(raw json.RawMessage) string {
	var entry struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ""
	}
	return entry.Name
}

func mustMarshalObject(v interface{}) json.RawMessage {
	data, err := marshalJSON(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
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
