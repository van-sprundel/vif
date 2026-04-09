package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// newClearCacheCmd returns the `vif clear-cache` command.
func newClearCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "clear-cache",
		Aliases:      []string{"clearcache", "cc"},
		Short:        "Clear the vif cache directory",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClearCache()
		},
	}

	return cmd
}

func runClearCache() error {
	w := os.Stderr

	cacheDir, err := cacheDirectory()
	if err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}

	info, err := os.Stat(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "Cache directory does not exist, nothing to clear.")
			return nil
		}
		return fmt.Errorf("stat cache directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cache path %q is not a directory", cacheDir)
	}

	if err := os.RemoveAll(cacheDir); err != nil {
		return fmt.Errorf("failed to remove cache directory: %w", err)
	}

	fmt.Fprintf(w, "Cleared cache at %s\n", cacheDir)
	return nil
}
