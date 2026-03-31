package lockfile

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/van-sprundel/vif/internal/pkg"
)

// LockFile represents the top-level structure of a composer.lock file.
type LockFile struct {
	ContentHash string        `json:"content-hash"`
	Packages    []pkg.Package `json:"packages"`
	PackagesDev []pkg.Package `json:"packages-dev"`
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
