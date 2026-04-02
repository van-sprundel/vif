package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/packagist"
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

	// 2. Resolve dependencies.
	client := packagist.NewClient("https://repo.packagist.org")
	progress := ui.NewProgress(w, "Resolving", 0, verbose)
	resolved, err := resolver.ResolveWithProgress(ctx, cj, client, func(name string) {
		progress.Increment(name)
	})
	progress.Finish()
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	fmt.Fprintf(w, "Resolved %d packages\n", len(resolved))

	// 3. Write composer.lock.
	lockPath := "composer.lock"
	if err := lockfile.Generate(lockPath, resolved, cj); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	fmt.Fprintf(w, "Wrote %s\n", lockPath)

	// 4. Install resolved packages.
	if err := installFromResolved(ctx, w, resolved, cj, verbose); err != nil {
		return err
	}

	ui.PrintSummary(w, len(resolved), start)

	return nil
}
