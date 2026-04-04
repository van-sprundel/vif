package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
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
		Use:          "update [packages...]",
		Short:        "Resolve dependencies and update composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd.Context(), args, verbose, noAutoloader)
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")
	cmd.Flags().BoolVar(&noAutoloader, "no-autoloader", false, "skip autoloader generation")

	return cmd
}

func runUpdate(ctx context.Context, packages []string, verbose bool, noAutoloader bool) error {
	start := time.Now()
	w := os.Stderr

	cj, err := composer.Parse("composer.json")
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}
	fmt.Fprintf(w, "Resolving dependencies for %s...\n", cj.Name)

	var lockedEntries map[string]packagist.VersionEntry
	var locked map[string]string
	if existingLock, err := lockfile.Parse("composer.lock"); err == nil {
		lockedEntries = existingLock.LockedEntries()

		if len(packages) > 0 {
			updateSet := make(map[string]struct{}, len(packages))
			for _, p := range packages {
				updateSet[p] = struct{}{}
			}

			locked = make(map[string]string, len(existingLock.Packages)+len(existingLock.PackagesDev))
			for _, p := range existingLock.Packages {
				if _, ok := updateSet[p.Name]; !ok {
					locked[p.Name] = p.Version
				}
			}
			for _, p := range existingLock.PackagesDev {
				if _, ok := updateSet[p.Name]; !ok {
					locked[p.Name] = p.Version
				}
			}
		}
	} else if len(packages) > 0 {
		return fmt.Errorf("cannot partially update without an existing composer.lock")
	}

	cacheDir, err := cacheDirectory()
	if err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	c, err := cache.New(cacheDir)
	if err != nil {
		return fmt.Errorf("cache init: %w", err)
	}
	defer c.Close()

	client, err := metadataClient(cj, c)
	if err != nil {
		return err
	}
	restrictedPackages, restriction, err := resolveRestrictedPackages(ctx, client, cj)
	if err != nil {
		return err
	}

	if len(packages) > 0 {
		fmt.Fprintf(w, "Partially updating: %s\n", formatPackageList(packages))
	}

	progress := ui.NewProgress(w, "Resolving", 0, verbose)
	resolved, err := resolver.ResolveWithOptions(ctx, cj, client, resolver.Options{
		RestrictedPackages: restrictedPackages,
		Restriction:        restriction,
		LockedEntries:      lockedEntries,
		Locked:             locked,
	}, func(name string) {
		progress.Increment(name)
	})
	progress.Finish()
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	fmt.Fprintf(w, "Resolved %d packages\n", len(resolved))

	if err := installFromResolved(ctx, w, resolved, cj, verbose, noAutoloader, c); err != nil {
		return err
	}

	lockPath := "composer.lock"
	if err := lockfile.Generate(lockPath, resolved, cj); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	fmt.Fprintf(w, "Wrote %s\n", lockPath)

	ui.PrintSummary(w, len(resolved), start)

	return nil
}

func formatPackageList(packages []string) string {
	if len(packages) <= 5 {
		return strings.Join(packages, ", ")
	}
	return strings.Join(packages[:5], ", ") + fmt.Sprintf(" and %d more", len(packages)-5)
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
