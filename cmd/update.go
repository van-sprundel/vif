package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/ui"
)

// newUpdateCmd returns the `vif update` command.
func newUpdateCmd() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Resolve dependencies and update composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd.Context(), verbose)
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")

	return cmd
}

func runUpdate(ctx context.Context, verbose bool) error {
	start := time.Now()
	w := os.Stderr

	// 1. Parse composer.json.
	cj, err := composer.Parse("composer.json")
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}
	fmt.Fprintf(w, "Resolving dependencies for %s...\n", cj.Name)

	// 2. Open the persistent cache (shared with install phase).
	cacheDir, err := cacheDirectory()
	if err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	c, err := cache.New(cacheDir)
	if err != nil {
		return fmt.Errorf("cache init: %w", err)
	}
	defer c.Close()

	// 3. Resolve dependencies.
	client, err := metadataClient(cj, c)
	if err != nil {
		return err
	}
	progress := ui.NewProgress(w, "Resolving", 0, verbose)
	resolved, err := resolver.ResolveWithProgress(ctx, cj, client, func(name string) {
		progress.Increment(name)
	})
	progress.Finish()
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	fmt.Fprintf(w, "Resolved %d packages\n", len(resolved))

	// 4. Install resolved packages first so lockfile updates are atomic:
	// if install fails, we keep the previous composer.lock unchanged.
	if err := installFromResolved(ctx, w, resolved, cj, verbose, c); err != nil {
		return err
	}

	// 5. Write composer.lock after a successful install.
	lockPath := "composer.lock"
	if err := lockfile.Generate(lockPath, resolved, cj); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	fmt.Fprintf(w, "Wrote %s\n", lockPath)

	ui.PrintSummary(w, len(resolved), start)

	return nil
}
