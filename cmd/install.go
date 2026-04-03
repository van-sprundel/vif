package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/autoload"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/ui"
)

// newInstallCmd returns the `vif install` command.
func newInstallCmd() *cobra.Command {
	var verbose bool
	var noDev bool
	var noAutoloader bool

	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install packages from composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstall(cmd.Context(), verbose, noDev, noAutoloader)
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")
	cmd.Flags().BoolVar(&noDev, "no-dev", false, "skip dev dependencies")
	cmd.Flags().BoolVar(&noAutoloader, "no-autoloader", false, "skip autoloader generation")

	return cmd
}

func runInstall(ctx context.Context, verbose, noDev, noAutoloader bool) error {
	start := time.Now()
	w := os.Stderr

	// 1. Parse lockfile.
	lockfilePath := "composer.lock"
	lf, err := lockfile.Parse(lockfilePath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", lockfilePath, err)
	}

	var rootMeta *installer.RootPackage
	optimized := false
	prependAutoloader := true
	platformCheckMode := autoload.PlatformCheckFull
	var root *autoload.RootAutoload
	var cj *composer.ComposerJSON
	if parsed, err := composer.Parse("composer.json"); err == nil {
		cj = parsed
		optimized = cj.Config.OptimizeAutoloader
		prependAutoloader = cj.Config.PrependAutoloaderOrDefault()
		if !cj.Config.PlatformCheck.IsTrue() {
			platformCheckMode = autoload.PlatformCheckDisabled
		} else if cj.Config.PlatformCheck.IsPHPOnly() {
			platformCheckMode = autoload.PlatformCheckPHPOnly
		}
		rootMeta = &installer.RootPackage{
			Name:    cj.Name,
			Version: cj.Version,
			Type:    cj.Type,
		}
		root = &autoload.RootAutoload{
			Name:     cj.Name,
			Autoload: cj.Autoload,
		}
		if !noDev {
			root.AutoloadDev = cj.AutoloadDev
		}
	}

	packages := lf.Packages
	packagesDev := lf.PackagesDev
	if noDev {
		packagesDev = nil
	}

	allPackages := append(packages, packagesDev...)
	projectDir, err := filepath.Abs(".")
	if err != nil {
		return fmt.Errorf("project dir: %w", err)
	}
	if cj != nil {
		allPackages, err = applyLocalPathPackages(allPackages, cj.Repositories, projectDir)
		if err != nil {
			return fmt.Errorf("path repositories: %w", err)
		}
	}
	allPackages = promoteSourceToDist(allPackages, projectDir)

	prodCount := len(packages)
	packages = allPackages[:prodCount]
	packagesDev = allPackages[prodCount:]
	total := len(allPackages)
	if noDev {
		fmt.Fprintf(w, "Found %d packages (prod only, --no-dev)\n", total)
	} else {
		fmt.Fprintf(w, "Found %d packages (%d prod, %d dev)\n", total, len(packages), len(packagesDev))
	}

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
	auth, err := loadComposerAuth()
	if err != nil {
		return err
	}
	dl.SetAuth(auth)
	progress := ui.NewProgress(w, "Downloading", total, verbose)

	results, err := dl.Download(ctx, allPackages)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	var cached, downloaded, skipped, failed int
	var skippedUnsupported []string
	for _, r := range results {
		if r.Err != nil {
			failed++
			progress.Error(fmt.Sprintf("  ERROR %s: %v", r.Package.Name, r.Err))
		} else if r.Skipped {
			if r.Package.Type == "metapackage" {
				progress.Increment(r.Package.Name)
				continue
			}
			skipped++
			if !pkg.IsInstallable(r.Package) {
				skippedUnsupported = append(skippedUnsupported, fmt.Sprintf("%s (type=%s dist=%s url=%q)", r.Package.Name, r.Package.Type, r.Package.Dist.Type, r.Package.Dist.URL))
			}
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
	if len(skippedUnsupported) > 0 {
		sort.Strings(skippedUnsupported)
		return fmt.Errorf(
			"non-downloadable non-metapackage dependencies are not supported yet; blocked on %d package(s): %s",
			len(skippedUnsupported),
			strings.Join(skippedUnsupported, ", "),
		)
	}

	if skipped > 0 {
		fmt.Fprintf(w, "  %d downloaded, %d from cache, %d skipped (non-downloadable)\n", downloaded, cached, skipped)
	} else {
		fmt.Fprintf(w, "  %d downloaded, %d from cache\n", downloaded, cached)
	}

	// 4. Install to vendor/.
	vendorDir := filepath.Join(".", "vendor")
	inst := installer.New(c)

	fmt.Fprintf(w, "Installing to %s...\n", vendorDir)
	if err := inst.Install(packages, packagesDev, vendorDir, rootMeta); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	// 5. Generate autoloader.
	if noAutoloader {
		fmt.Fprintln(w, "Skipping autoload generation (--no-autoloader)")
	} else {
		fmt.Fprint(w, "Generating autoload files...")
		if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash, optimized, root, prependAutoloader, platformCheckMode); err != nil {
			return fmt.Errorf("autoload: %w", err)
		}
		fmt.Fprintln(w, " done")
	}

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
