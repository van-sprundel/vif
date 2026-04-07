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
	"github.com/van-sprundel/vif/internal/ui"
	versionPkg "github.com/van-sprundel/vif/internal/version"
)

// newRequireCmd returns the `vif require` command.
func newRequireCmd() *cobra.Command {
	var (
		dev          bool
		verbose      bool
		noAutoloader bool
	)

	cmd := &cobra.Command{
		Use:          "require [package:constraint ...]",
		Short:        "Add a package and update composer.lock",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRequire(cmd.Context(), args, dev, verbose, noAutoloader)
		},
	}

	cmd.Flags().BoolVar(&dev, "dev", false, "add packages to require-dev")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")
	cmd.Flags().BoolVar(&noAutoloader, "no-autoloader", false, "skip autoloader generation")

	return cmd
}

func runRequire(ctx context.Context, args []string, dev, verbose, noAutoloader bool) error {
	start := time.Now()
	w := os.Stderr

	composerPath := "composer.json"
	cj, err := composer.Parse(composerPath)
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}

	guessedConstraints := make(map[string]string)

	for _, arg := range args {
		name, constraint := parseRequireArg(arg)

		if constraint == "" {
			constraint = "*"
			guessedConstraints[name] = ""
		}

		if dev {
			cj.AddRequireDev(name, constraint)
		} else {
			cj.AddRequire(name, constraint)
		}

		fmt.Fprintf(w, "Adding %s %s\n", name, constraint)
	}

	if err := cj.Write(composerPath); err != nil {
		return fmt.Errorf("write composer.json: %w", err)
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

	fmt.Fprintf(w, "Resolving dependencies...\n")
	client, err := metadataClient(cj, c)
	if err != nil {
		return err
	}
	var (
		lockedEntries map[string]packagist.VersionEntry
		locked        map[string]string
		fixed         map[string]string
	)
	if existingLock, err := lockfile.Parse("composer.lock"); err == nil {
		lockedEntries = existingLock.LockedEntries()
		locked = make(map[string]string, len(existingLock.Packages)+len(existingLock.PackagesDev))
		fixed = make(map[string]string, len(existingLock.Packages)+len(existingLock.PackagesDev))
		for _, p := range existingLock.Packages {
			locked[p.Name] = p.Version
			fixed[p.Name] = p.Version
		}
		for _, p := range existingLock.PackagesDev {
			locked[p.Name] = p.Version
			fixed[p.Name] = p.Version
		}
		for _, arg := range args {
			name, _ := parseRequireArg(arg)
			delete(locked, name)
			delete(fixed, name)
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
		return fmt.Errorf("resolve: %w", err)
	}

	fmt.Fprintf(w, "Resolved %d packages\n", len(resolved))

	if len(guessedConstraints) > 0 {
		updated := false
		for i := range resolved {
			rp := &resolved[i]
			if _, ok := guessedConstraints[rp.Name]; ok {
				guessed := recommendedConstraint(rp.Version)
				if guessed != "" && guessed != "*" {
					if dev {
						cj.AddRequireDev(rp.Name, guessed)
					} else {
						cj.AddRequire(rp.Name, guessed)
					}
					guessedConstraints[rp.Name] = guessed
					updated = true
				}
			}
		}
		if updated {
			if err := cj.Write(composerPath); err != nil {
				return fmt.Errorf("write composer.json: %w", err)
			}
			for name, c := range guessedConstraints {
				if c != "" {
					fmt.Fprintf(w, "  Using version %s for %s\n", c, name)
				}
			}
		}
	}

	if _, err := installFromResolved(ctx, w, resolved, cj, verbose, noAutoloader, c, false); err != nil {
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

func recommendedConstraint(ver string) string {
	v, err := versionPkg.Parse(ver)
	if err != nil {
		return "*"
	}
	if v.Dev || v.Major == 0 {
		return "*"
	}
	return fmt.Sprintf("^%d.%d", v.Major, v.Minor)
}

// parseRequireArg splits "vendor/package:^1.0" into name and constraint.
func parseRequireArg(arg string) (name, constraint string) {
	if idx := strings.LastIndex(arg, ":"); idx > 0 {
		return arg[:idx], arg[idx+1:]
	}
	return arg, ""
}
