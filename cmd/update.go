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
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/ui"
	"github.com/van-sprundel/vif/internal/version"
)

// newUpdateCmd returns the `vif update` command.
func newUpdateCmd() *cobra.Command {
	var verbose bool
	var noAutoloader bool

	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Resolve dependencies and update composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd.Context(), verbose, noAutoloader)
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")
	cmd.Flags().BoolVar(&noAutoloader, "no-autoloader", false, "skip autoloader generation")

	return cmd
}

func runUpdate(ctx context.Context, verbose bool, noAutoloader bool) error {
	start := time.Now()
	w := os.Stderr

	// 1. Parse composer.json.
	cj, err := composer.Parse("composer.json")
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}
	fmt.Fprintf(w, "Resolving dependencies for %s...\n", cj.Name)

	// 2. Read existing lockfile for VCS-only packages (if present).
	var lockedEntries map[string]packagist.VersionEntry
	if existingLock, err := lockfile.Parse("composer.lock"); err == nil {
		lockedEntries = existingLock.LockedEntries()
	}

	// 3. Open the persistent cache (shared with install phase).
	cacheDir, err := cacheDirectory()
	if err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	c, err := cache.New(cacheDir)
	if err != nil {
		return fmt.Errorf("cache init: %w", err)
	}
	defer c.Close()

	// 4. Resolve dependencies.
	client, err := metadataClient(cj, c)
	if err != nil {
		return err
	}
	restrictedPackages, restriction, err := resolveRestrictedPackages(ctx, client, cj)
	if err != nil {
		return err
	}
	progress := ui.NewProgress(w, "Resolving", 0, verbose)
	resolved, err := resolver.ResolveWithOptions(ctx, cj, client, resolver.Options{
		RestrictedPackages: restrictedPackages,
		Restriction:        restriction,
		LockedEntries:      lockedEntries,
	}, func(name string) {
		progress.Increment(name)
	})
	progress.Finish()
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	fmt.Fprintf(w, "Resolved %d packages\n", len(resolved))

	// 4. Install resolved packages first so lockfile updates are atomic:
	// if install fails, we keep the previous composer.lock unchanged.
	if err := installFromResolved(ctx, w, resolved, cj, verbose, noAutoloader, c); err != nil {
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

func resolveRestrictedPackages(ctx context.Context, client packagist.Fetcher, cj *composer.ComposerJSON) (map[string]struct{}, string, error) {
	restriction := cj.Extra.Symfony.Require
	if restriction == "" {
		return nil, "", nil
	}

	constraint, err := version.ParseConstraint(restriction)
	if err != nil {
		return nil, "", fmt.Errorf("resolve symfony restriction %q: %w", restriction, err)
	}

	versions, err := client.GetPackage(ctx, "symfony/symfony")
	if err != nil {
		return nil, "", fmt.Errorf("fetch symfony/symfony metadata: %w", err)
	}

	var (
		best          packagist.VersionEntry
		bestVersion   version.Version
		bestVersionOK bool
	)
	for _, entry := range versions {
		v, err := version.Parse(entry.Version)
		if err != nil || !constraint.Matches(v) {
			continue
		}
		if !bestVersionOK || version.Compare(v, bestVersion) > 0 {
			best = entry
			bestVersion = v
			bestVersionOK = true
		}
	}
	if !bestVersionOK {
		return nil, "", fmt.Errorf("resolve symfony restriction %q: no matching symfony/symfony version found", restriction)
	}

	restricted := make(map[string]struct{}, len(best.Replace))
	for name := range best.Replace {
		restricted[name] = struct{}{}
	}
	return restricted, restriction, nil
}
