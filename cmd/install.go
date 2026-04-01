package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/autoload"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/ui"
)

// newInstallCmd returns the `vif install` command.
func newInstallCmd() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install packages from composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstall(cmd.Context(), verbose)
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")

	return cmd
}

func runInstall(ctx context.Context, verbose bool) error {
	start := time.Now()
	w := os.Stderr

	// 1. Parse lockfile.
	lockfilePath := "composer.lock"
	lf, err := lockfile.Parse(lockfilePath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", lockfilePath, err)
	}

	allPackages := append(lf.Packages, lf.PackagesDev...)
	total := len(allPackages)
	fmt.Fprintf(w, "Found %d packages (%d prod, %d dev)\n", total, len(lf.Packages), len(lf.PackagesDev))

	// 2. Init cache.
	cacheDir, err := cacheDirectory()
	if err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	c, err := cache.New(cacheDir)
	if err != nil {
		return fmt.Errorf("cache init: %w", err)
	}
	defer c.Close()

	// 3. Download.
	dl := downloader.New(c, 0) // 0 = auto workers
	progress := ui.NewProgress(w, "Downloading", total, verbose)

	results, err := dl.Download(ctx, allPackages)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	var cached, downloaded, skipped, failed int
	for _, r := range results {
		if r.Err != nil {
			failed++
			progress.Error(fmt.Sprintf("  ERROR %s: %v", r.Package.Name, r.Err))
		} else if r.Skipped {
			skipped++
			progress.Increment(r.Package.Name)
		} else if r.FromCache {
			cached++
			progress.Increment(r.Package.Name)
		} else {
			downloaded++
			progress.Increment(r.Package.Name)
		}
	}
	progress.Finish()

	if failed > 0 {
		return fmt.Errorf("%d package(s) failed to download", failed)
	}

	fmt.Fprintf(w, "  %d downloaded, %d from cache, %d skipped (path)\n", downloaded, cached, skipped)

	// 4. Install to vendor/.
	vendorDir := filepath.Join(".", "vendor")
	inst := installer.New(c)

	fmt.Fprintf(w, "Installing to %s...\n", vendorDir)
	if err := inst.Install(lf.Packages, lf.PackagesDev, vendorDir); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	// 5. Generate autoloader.
	// Check composer.json for optimize-autoloader config.
	optimized := false
	if cj, err := composer.Parse("composer.json"); err == nil {
		optimized = cj.Config.OptimizeAutoloader
	}
	fmt.Fprint(w, "Generating autoload files...")
	if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash, optimized); err != nil {
		return fmt.Errorf("autoload: %w", err)
	}
	fmt.Fprintln(w, " done")

	// 6. Summary.
	ui.PrintSummary(w, total, start)

	return nil
}

// cacheDirectory returns the vif cache directory, respecting $XDG_CACHE_HOME.
func cacheDirectory() (string, error) {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "vif"), nil
}
