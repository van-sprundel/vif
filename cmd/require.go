package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/ui"
)

// newRequireCmd returns the `vif require` command.
func newRequireCmd() *cobra.Command {
	var (
		dev     bool
		verbose bool
	)

	cmd := &cobra.Command{
		Use:          "require [package:constraint ...]",
		Short:        "Add a package and update composer.lock",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRequire(cmd.Context(), args, dev, verbose)
		},
	}

	cmd.Flags().BoolVar(&dev, "dev", false, "add packages to require-dev")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")

	return cmd
}

func runRequire(ctx context.Context, args []string, dev, verbose bool) error {
	start := time.Now()
	w := os.Stderr

	// 1. Parse composer.json.
	composerPath := "composer.json"
	cj, err := composer.Parse(composerPath)
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}

	// 2. Parse arguments and add to composer.json.
	for _, arg := range args {
		name, constraint := parseRequireArg(arg)

		// If no constraint given, default to latest via "*" — the resolver
		// will pick the highest stable version.
		if constraint == "" {
			constraint = "*"
		}

		if dev {
			cj.AddRequireDev(name, constraint)
		} else {
			cj.AddRequire(name, constraint)
		}

		fmt.Fprintf(w, "Adding %s %s\n", name, constraint)
	}

	// 3. Write updated composer.json.
	if err := cj.Write(composerPath); err != nil {
		return fmt.Errorf("write composer.json: %w", err)
	}

	// 4. Resolve dependencies.
	fmt.Fprintf(w, "Resolving dependencies...\n")
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

	// 5. Write composer.lock.
	lockPath := "composer.lock"
	if err := lockfile.Generate(lockPath, resolved, cj); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	fmt.Fprintf(w, "Wrote %s\n", lockPath)

	// 6. Run install pipeline (same as update).
	if err := installFromResolved(ctx, w, resolved, cj, verbose); err != nil {
		return err
	}

	ui.PrintSummary(w, len(resolved), start)

	return nil
}

// parseRequireArg splits "vendor/package:^1.0" into name and constraint.
func parseRequireArg(arg string) (name, constraint string) {
	if idx := strings.LastIndex(arg, ":"); idx > 0 {
		return arg[:idx], arg[idx+1:]
	}
	return arg, ""
}
