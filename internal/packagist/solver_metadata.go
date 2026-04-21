package packagist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/version"
)

// SolverMetadataSchemaVersion increments when the compact solver-record encoding changes.
const SolverMetadataSchemaVersion = 1

// SolverPackageRecord stores only the metadata required for dependency resolution.
type SolverPackageRecord struct {
	Name        string                `json:"name"`
	RepoURL     string                `json:"repo_url"`
	DevIncluded bool                  `json:"dev_included"`
	SourceETag  string                `json:"source_etag"`
	SourceHash  string                `json:"source_hash"`
	Versions    []SolverVersionRecord `json:"versions"`
}

// SolverVersionRecord stores the minimal per-version fields needed by the resolver.
type SolverVersionRecord struct {
	Version           string            `json:"version"`
	VersionNormalized string            `json:"version_normalized"`
	Stability         string            `json:"stability"`
	Require           map[string]string `json:"require,omitempty"`
	Conflict          map[string]string `json:"conflict,omitempty"`
	Provide           map[string]string `json:"provide,omitempty"`
	Replace           map[string]string `json:"replace,omitempty"`
	RequirePHP        string            `json:"require_php,omitempty"`
	RequireExt        map[string]string `json:"require_ext,omitempty"`
	RequireLib        map[string]string `json:"require_lib,omitempty"`
}

func NormalizeForSolver(repoURL, name string, versions []VersionEntry, devIncluded bool, sourceETag string, sourceBody []byte) SolverPackageRecord {
	record := SolverPackageRecord{
		Name:        name,
		RepoURL:     repoURL,
		DevIncluded: devIncluded,
		SourceETag:  sourceETag,
		SourceHash:  solverSourceHash(sourceBody),
		Versions:    make([]SolverVersionRecord, 0, len(versions)),
	}

	for _, entry := range versions {
		record.Versions = append(record.Versions, normalizeSolverVersion(entry))
	}

	return record
}

func MarshalSolverRecord(record SolverPackageRecord) ([]byte, error) {
	return json.Marshal(record)
}

func UnmarshalSolverRecord(data []byte) (SolverPackageRecord, error) {
	var record SolverPackageRecord
	err := json.Unmarshal(data, &record)
	return record, err
}

func normalizeSolverVersion(entry VersionEntry) SolverVersionRecord {
	rec := SolverVersionRecord{
		Version:           entry.Version,
		VersionNormalized: entry.VersionNormalized,
		Stability:         solverStability(entry.Version),
		Require:           entry.NonPlatformRequire(),
		Conflict:          cloneRelationMap(entry.Conflict),
		Provide:           cloneRelationMap(entry.Provide),
		Replace:           cloneRelationMap(entry.Replace),
	}

	if len(entry.Require) > 0 {
		var requireExt, requireLib map[string]string
		for name, constraint := range entry.Require {
			switch {
			case name == "php":
				rec.RequirePHP = constraint
			case strings.HasPrefix(name, "ext-"):
				if requireExt == nil {
					requireExt = make(map[string]string)
				}
				requireExt[name] = constraint
			case strings.HasPrefix(name, "lib-"):
				if requireLib == nil {
					requireLib = make(map[string]string)
				}
				requireLib[name] = constraint
			case pkg.IsPlatformPackage(name):
				// Other platform packages are intentionally omitted from the compact record
				// until the resolver needs them explicitly.
			}
		}
		rec.RequireExt = requireExt
		rec.RequireLib = requireLib
	}

	return rec
}

func cloneRelationMap(in RelationMap) map[string]string {
	return cloneStringMap(map[string]string(in))
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func solverSourceHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func solverStability(rawVersion string) string {
	v, err := version.Parse(rawVersion)
	if err != nil {
		return "unknown"
	}
	switch v.Stability {
	case version.Dev:
		return "dev"
	case version.Alpha:
		return "alpha"
	case version.Beta:
		return "beta"
	case version.RC:
		return "rc"
	case version.Patch:
		return "patch"
	case version.Stable:
		return "stable"
	default:
		return "unknown"
	}
}
