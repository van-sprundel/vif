package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/pkg"
)

// applyLocalPathPackages maps lock/resolved packages to local Composer path
// repositories and fills Dist for local installation when needed.
func applyLocalPathPackages(packages []pkg.Package, repositories []composer.Repository, projectDir string) ([]pkg.Package, error) {
	if len(packages) == 0 || len(repositories) == 0 {
		return packages, nil
	}

	localPackages, err := discoverPathRepositoryPackages(repositories, projectDir)
	if err != nil {
		return nil, err
	}
	if len(localPackages) == 0 {
		return packages, nil
	}

	out := slices.Clone(packages)
	for i := range out {
		localDir, ok := localPackages[out[i].Name]
		if !ok {
			continue
		}

		if out[i].Dist.Type == "path" || strings.TrimSpace(out[i].Dist.URL) == "" {
			out[i].Dist.Type = "path"
			out[i].Dist.URL = localDir
			out[i].Dist.Reference = ""
			out[i].Dist.Shasum = ""
		}
	}

	return out, nil
}

func discoverPathRepositoryPackages(repositories []composer.Repository, projectDir string) (map[string]string, error) {
	packages := make(map[string]string)

	for _, repo := range repositories {
		if repo.Type != "path" || strings.TrimSpace(repo.URL) == "" {
			continue
		}

		candidates, err := resolvePathRepositoryCandidates(projectDir, repo.URL)
		if err != nil {
			return nil, err
		}

		for _, dir := range candidates {
			name, err := readPackageName(filepath.Join(dir, "composer.json"))
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(name) == "" {
				continue
			}

			if _, exists := packages[name]; !exists {
				packages[name] = dir
			}
		}
	}

	return packages, nil
}

func resolvePathRepositoryCandidates(projectDir, repoURL string) ([]string, error) {
	pattern := repoURL
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(projectDir, pattern)
	}

	if strings.ContainsAny(pattern, "*?[") {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("path repository glob %q: %w", repoURL, err)
		}
		return matches, nil
	}

	return []string{pattern}, nil
}

func readPackageName(composerPath string) (string, error) {
	data, err := os.ReadFile(composerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %q: %w", composerPath, err)
	}

	var doc struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("parse %q: %w", composerPath, err)
	}

	return doc.Name, nil
}
