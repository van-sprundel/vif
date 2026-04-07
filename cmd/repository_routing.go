package cmd

import "strings"

type repositoryClass int

const (
	repositoryClassDefault repositoryClass = iota
	repositoryClassDrupal
	repositoryClassAsset
	repositoryClassUrbanHeroes
	repositoryClassPackagist
)

func classifyRepository(repoURL string) repositoryClass {
	lowerURL := strings.ToLower(strings.TrimSpace(repoURL))

	switch {
	case strings.Contains(lowerURL, "packages.drupal.org"):
		return repositoryClassDrupal
	case strings.Contains(lowerURL, "asset-packagist.org"):
		return repositoryClassAsset
	case strings.Contains(lowerURL, "satis.urban-heroes.nl"),
		strings.Contains(lowerURL, "gitlab.com/api/v4/group/13017208/-/packages/composer"):
		return repositoryClassUrbanHeroes
	case strings.Contains(lowerURL, "repo.packagist.org"):
		return repositoryClassPackagist
	default:
		return repositoryClassDefault
	}
}

func matcherForRepositoryClass(class repositoryClass) func(string) bool {
	switch class {
	case repositoryClassDrupal:
		return func(name string) bool {
			return strings.HasPrefix(name, "drupal/")
		}
	case repositoryClassAsset:
		return func(name string) bool {
			return strings.HasPrefix(name, "bower-asset/") || strings.HasPrefix(name, "npm-asset/")
		}
	case repositoryClassUrbanHeroes:
		return func(name string) bool {
			return strings.HasPrefix(name, "urbanheroes-")
		}
	case repositoryClassPackagist:
		return func(name string) bool {
			if strings.HasPrefix(name, "bower-asset/") || strings.HasPrefix(name, "npm-asset/") {
				return false
			}
			if strings.HasPrefix(name, "urbanheroes-") {
				return false
			}
			return !strings.HasPrefix(name, "drupal/")
		}
	default:
		return nil
	}
}

func repositoryMatcher(repoURL string) func(string) bool {
	return matcherForRepositoryClass(classifyRepository(repoURL))
}
