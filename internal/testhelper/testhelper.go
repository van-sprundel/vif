package testhelper

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var testTempBase struct {
	once sync.Once
	dir  string
	err  error
}

// GetTestTempBase returns the base directory for all test temporary directories.
// It defaults to ./tmp/vif-test and can be overridden with VIF_TEST_TEMP_DIR.
func GetTestTempBase() (string, error) {
	testTempBase.once.Do(func() {
		base := os.Getenv("VIF_TEST_TEMP_DIR")
		if base == "" {
			// Also check VIF_COMPAT_CACHE_DIR for backward compatibility/consistency
			base = os.Getenv("VIF_COMPAT_CACHE_DIR")
		}
		if base == "" {
			// Default to a directory in the project root's tmp.
			base = filepath.Join(".", "tmp", "vif-test")
		}

		// Ensure absolute path if it's relative to project root
		if !filepath.IsAbs(base) {
			abs, err := filepath.Abs(base)
			if err != nil {
				testTempBase.err = fmt.Errorf("failed to get absolute path for %s: %w", base, err)
				return
			}
			base = abs
		}

		if err := os.MkdirAll(base, 0o755); err != nil {
			testTempBase.err = fmt.Errorf("failed to create test temp base %s: %w", base, err)
			return
		}
		testTempBase.dir = base
	})
	return testTempBase.dir, testTempBase.err
}

// TempDir creates a temporary directory under the test temp base.
// It automatically cleans up the directory after the test finishes.
func TempDir(tb testing.TB, prefix string) string {
	tb.Helper()

	base, err := GetTestTempBase()
	if err != nil {
		tb.Fatalf("GetTestTempBase: %v", err)
	}

	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		tb.Fatalf("failed to create temp dir %s*: %v", prefix, err)
	}

	tb.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	return dir
}
