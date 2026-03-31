package pkg

// Package represents a single entry from composer.lock packages or packages-dev.
type Package struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Type        string   `json:"type"`
	Dist        Dist     `json:"dist"`
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
// Custom unmarshaling to handle composer's mixed string/[]string format is
// handled by the lockfile parser.
type Autoload struct {
	PSR4     map[string][]string `json:"psr-4"`
	PSR0     map[string][]string `json:"psr-0"`
	Classmap []string            `json:"classmap"`
	Files    []string            `json:"files"`
}
