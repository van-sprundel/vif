package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/telemetry"
	"github.com/van-sprundel/vif/internal/ui"
)

// newRemoveCmd returns the `vif remove` command.
func newRemoveCmd() *cobra.Command {
	var (
		dev          bool
		noUpdate     bool
		verbose      bool
		noAutoloader bool
	)

	cmd := &cobra.Command{
		Use:          "remove [packages...]",
		Aliases:      []string{"rm", "uninstall"},
		Short:        "Remove a package and update composer.lock",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemove(cmd.Context(), args, dev, noUpdate, verbose, noAutoloader)
		},
	}

	cmd.Flags().BoolVar(&dev, "dev", false, "remove packages from require-dev")
	cmd.Flags().BoolVar(&noUpdate, "no-update", false, "only remove from composer.json, skip dependency update")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")
	cmd.Flags().BoolVar(&noAutoloader, "no-autoloader", false, "skip autoloader generation")

	return cmd
}

func runRemove(ctx context.Context, packages []string, dev, noUpdate, verbose, noAutoloader bool) (retErr error) {
	start := time.Now()
	w := os.Stderr

	defer func() {
		if retErr != nil && telemetry.Enabled() {
			telemetry.Send(telemetry.Event{
				Command:    "remove",
				Version:    Version,
				DurationMs: time.Since(start).Milliseconds(),
				ErrorType:  telemetry.ErrorCategory(retErr),
			})
		}
	}()

	composerPath := "composer.json"
	cj, err := composer.Parse(composerPath)
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}

	// Back up original composer.json for rollback on failure.
	backupData, err := os.ReadFile(composerPath)
	if err != nil {
		return fmt.Errorf("failed to back up composer.json: %w", err)
	}

	var removed []string
	for _, name := range packages {
		found := false
		if dev {
			if _, ok := cj.RequireDev[name]; ok {
				cj.RemoveRequireDev(name)
				found = true
			}
		} else {
			if _, ok := cj.Require[name]; ok {
				cj.RemoveRequire(name)
				found = true
			}
			// Also check the other section.
			if !found {
				if _, ok := cj.RequireDev[name]; ok {
					cj.RemoveRequireDev(name)
					found = true
				}
			}
		}
		if !found {
			fmt.Fprintf(w, "Package %s is not required, skipping.\n", name)
			continue
		}
		removed = append(removed, name)
		fmt.Fprintf(w, "Removing %s\n", name)
	}

	if len(removed) == 0 {
		return fmt.Errorf("no packages to remove")
	}

	if err := cj.Write(composerPath); err != nil {
		return fmt.Errorf("write composer.json: %w", err)
	}

	if noUpdate {
		fmt.Fprintln(w, "Skipping dependency update (--no-update)")
		return nil
	}

	// Resolve dependencies with the removed packages gone.
	cacheDir, err := cacheDirectory()
	if err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	c, err := cache.New(cacheDir)
	if err != nil {
		return fmt.Errorf("cache init: %w", err)
	}
	defer c.Close()

	fmt.Fprintf(w, "Resolving dependencies...\n")
	client, err := metadataClient(cj, c)
	if err != nil {
		// Rollback composer.json on failure.
		_ = os.WriteFile(composerPath, backupData, 0o644)
		return err
	}

	var (
		lockedEntries map[string]packagist.VersionEntry
		locked        map[string]string
		fixed         map[string]string
	)
	removedSet := make(map[string]struct{}, len(removed))
	for _, name := range removed {
		removedSet[name] = struct{}{}
	}

	if existingLock, err := lockfile.Parse("composer.lock"); err == nil {
		lockedEntries = existingLock.LockedEntries()
		locked = make(map[string]string, len(existingLock.Packages)+len(existingLock.PackagesDev))
		fixed = make(map[string]string, len(existingLock.Packages)+len(existingLock.PackagesDev))
		for _, p := range existingLock.Packages {
			if _, isRemoved := removedSet[p.Name]; !isRemoved {
				locked[p.Name] = p.Version
				fixed[p.Name] = p.Version
			}
		}
		for _, p := range existingLock.PackagesDev {
			if _, isRemoved := removedSet[p.Name]; !isRemoved {
				locked[p.Name] = p.Version
				fixed[p.Name] = p.Version
			}
		}
	}

	progress := ui.NewProgress(w, "Resolving", 0, verbose)
	if traced, ok := client.(interface {
		SetLookupTrace(func(packagist.LookupTrace))
	}); ok {
		traced.SetLookupTrace(func(trace packagist.LookupTrace) {
			if !verbose {
				return
			}
			progress.Error(formatRepositoryLookupLog(trace))
		})
	}
	var (
		solveMu      sync.Mutex
		solveLast    time.Time
		solveCounter int
	)
	onSolveProgress := func(name string) {
		if !verbose {
			return
		}
		solveMu.Lock()
		solveCounter++
		emit := solveCounter%50000 == 0 || time.Since(solveLast) >= 10*time.Second
		if emit {
			solveLast = time.Now()
		}
		solveMu.Unlock()
		if emit {
			progress.Error(fmt.Sprintf("  Solving... last=%s states=%d", name, solveCounter))
		}
	}
	onLookupDone := func(name string, d time.Duration, err error) {
		if !verbose {
			return
		}
		progress.Error(formatLookupLog(name, d, err))
	}
	resolved, err := resolver.ResolveWithOptions(ctx, cj, client, resolver.Options{
		Fixed:         fixed,
		LockedEntries: lockedEntries,
		Locked:        locked,
		LookupDone:    onLookupDone,
		SolveProgress: onSolveProgress,
	}, func(name string) {
		progress.Increment(name)
	})
	progress.Finish()
	if err != nil {
		// Rollback composer.json on failure.
		_ = os.WriteFile(composerPath, backupData, 0o644)
		return fmt.Errorf("resolve: %w", err)
	}

	fmt.Fprintf(w, "Resolved %d packages\n", len(resolved))

	if _, err := installFromResolved(ctx, w, resolved, cj, verbose, noAutoloader, c, false); err != nil {
		_ = os.WriteFile(composerPath, backupData, 0o644)
		return err
	}

	lockPath := "composer.lock"
	if err := lockfile.Generate(lockPath, resolved, cj); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	fmt.Fprintf(w, "Wrote %s\n", lockPath)

	ui.PrintSummary(w, len(resolved), start)

	if telemetry.Enabled() {
		telemetry.Send(telemetry.Event{
			Command:      "remove",
			Version:      Version,
			PackageCount: len(resolved),
			DurationMs:   time.Since(start).Milliseconds(),
			Success:      true,
		})
	}

	return nil
}

// formatPackageListForRemove formats a list for display.
func formatPackageListForRemove(names []string) string {
	return strings.Join(names, ", ")
}
