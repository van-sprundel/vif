package cmd

import "testing"

func TestClassifyRepository(t *testing.T) {
	tests := []struct {
		name    string
		repoURL string
		want    repositoryClass
	}{
		{name: "drupal", repoURL: "https://packages.drupal.org/8", want: repositoryClassDrupal},
		{name: "asset", repoURL: "https://asset-packagist.org", want: repositoryClassAsset},
		{name: "urbanheroes satis", repoURL: "https://satis.urban-heroes.nl", want: repositoryClassUrbanHeroes},
		{name: "urbanheroes gitlab", repoURL: "https://gitlab.com/api/v4/group/13017208/-/packages/composer", want: repositoryClassUrbanHeroes},
		{name: "packagist", repoURL: "https://repo.packagist.org", want: repositoryClassPackagist},
		{name: "default", repoURL: "https://example.org/packages", want: repositoryClassDefault},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyRepository(tt.repoURL); got != tt.want {
				t.Fatalf("classifyRepository(%q) = %v, want %v", tt.repoURL, got, tt.want)
			}
		})
	}
}

func TestRepositoryMatcher(t *testing.T) {
	tests := []struct {
		name    string
		repoURL string
		pkg     string
		want    bool
	}{
		{name: "drupal repo accepts drupal package", repoURL: "https://packages.drupal.org/8", pkg: "drupal/pathauto", want: true},
		{name: "drupal repo skips symfony package", repoURL: "https://packages.drupal.org/8", pkg: "symfony/console", want: false},
		{name: "asset repo accepts bower asset", repoURL: "https://asset-packagist.org", pkg: "bower-asset/lazysizes", want: true},
		{name: "asset repo skips composer package", repoURL: "https://asset-packagist.org", pkg: "composer/installers", want: false},
		{name: "urbanheroes repo accepts private package", repoURL: "https://satis.urban-heroes.nl", pkg: "urbanheroes-d8/uh_views", want: true},
		{name: "urbanheroes repo skips symfony package", repoURL: "https://satis.urban-heroes.nl", pkg: "symfony/console", want: false},
		{name: "packagist accepts symfony package", repoURL: "https://repo.packagist.org", pkg: "symfony/console", want: true},
		{name: "packagist skips drupal package", repoURL: "https://repo.packagist.org", pkg: "drupal/pathauto", want: false},
		{name: "packagist skips asset package", repoURL: "https://repo.packagist.org", pkg: "bower-asset/lazysizes", want: false},
		{name: "packagist skips urbanheroes package", repoURL: "https://repo.packagist.org", pkg: "urbanheroes-d8/uh_views", want: false},
		{name: "default matcher allows package", repoURL: "https://example.org/packages", pkg: "acme/foo", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher := repositoryMatcher(tt.repoURL)
			if matcher == nil {
				if tt.want {
					return
				}
				t.Fatal("matcher should not be nil")
			}
			if got := matcher(tt.pkg); got != tt.want {
				t.Fatalf("repositoryMatcher(%q)(%q) = %v, want %v", tt.repoURL, tt.pkg, got, tt.want)
			}
		})
	}
}
