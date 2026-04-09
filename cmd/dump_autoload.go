package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/autoload"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/scripts"
	"github.com/van-sprundel/vif/internal/telemetry"
)

// newDumpAutoloadCmd returns the `vif dump-autoload` command.
func newDumpAutoloadCmd() *cobra.Command {
	var (
		optimize      bool
		authoritative bool
		noDev         bool
		noScripts     bool
	)

	cmd := &cobra.Command{
		Use:          "dump-autoload",
		Aliases:      []string{"dumpautoload"},
		Short:        "Regenerate the autoloader",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDumpAutoload(optimize, authoritative, noDev, noScripts)
		},
	}

	cmd.Flags().BoolVarP(&optimize, "optimize", "o", false, "optimize PSR-4/PSR-0 into classmap")
	cmd.Flags().BoolVarP(&authoritative, "classmap-authoritative", "a", false, "only load from classmap (implies --optimize)")
	cmd.Flags().BoolVar(&noDev, "no-dev", false, "exclude autoload-dev rules")
	cmd.Flags().BoolVar(&noScripts, "no-scripts", false, "skip execution of scripts defined in composer.json")

	return cmd
}

func runDumpAutoload(optimize, authoritative, noDev, noScripts bool) (retErr error) {
	start := time.Now()
	w := os.Stderr

	defer func() {
		if retErr != nil && telemetry.Enabled() {
			telemetry.Send(telemetry.Event{
				Command:    "dump-autoload",
				Version:    Version,
				DurationMs: time.Since(start).Milliseconds(),
				ErrorType:  telemetry.ErrorCategory(retErr),
			})
		}
	}()

	if authoritative {
		optimize = true
	}

	// Parse composer.json.
	cj, err := composer.Parse("composer.json")
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}

	// Parse lockfile for package list.
	lf, err := lockfile.Parse("composer.lock")
	if err != nil {
		return fmt.Errorf("failed to read composer.lock: %w", err)
	}

	packages := lf.Packages
	packagesDev := lf.PackagesDev
	if noDev {
		packagesDev = nil
	}
	allPackages := append(packages, packagesDev...)

	// Apply local path packages.
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

	vendorDir := filepath.Join(".", "vendor")

	// Build root autoload config.
	root := &autoload.RootAutoload{
		Name:     cj.Name,
		Autoload: cj.Autoload,
	}
	if !noDev {
		root.AutoloadDev = cj.AutoloadDev
	}

	// Use optimize flag or config setting.
	if cj.Config.OptimizeAutoloader {
		optimize = true
	}

	platformCheckMode := autoload.PlatformCheckFull
	if !cj.Config.PlatformCheck.IsTrue() {
		platformCheckMode = autoload.PlatformCheckDisabled
	} else if cj.Config.PlatformCheck.IsPHPOnly() {
		platformCheckMode = autoload.PlatformCheckPHPOnly
	}

	devNames := make(map[string]bool, len(packagesDev))
	for _, p := range packagesDev {
		devNames[p.Name] = true
	}
	ivCfg := &autoload.InstalledVersionsConfig{
		RootName:        cj.Name,
		RootVersion:     cj.Version,
		RootType:        cj.Type,
		DevMode:         !noDev,
		DevPackageNames: devNames,
	}

	fmt.Fprint(w, "Generating autoload files...")
	if err := autoload.Generate(vendorDir, allPackages, lf.ContentHash, optimize, root, cj.Config.PrependAutoloaderOrDefault(), platformCheckMode, ivCfg); err != nil {
		return fmt.Errorf("autoload: %w", err)
	}

	if optimize {
		fmt.Fprintln(w, " done (optimized)")
	} else {
		fmt.Fprintln(w, " done")
	}

	// Run post-autoload-dump scripts.
	if !noScripts {
		projectDir, _ := filepath.Abs(".")
		runner := scripts.New(projectDir, cj.Scripts, cj.Extra.Symfony.AutoScripts, w)
		if err := runner.Run("post-autoload-dump"); err != nil {
			return err
		}
	}

	fmt.Fprintf(w, "Generated in %s\n", time.Since(start).Round(time.Millisecond))

	if telemetry.Enabled() {
		telemetry.Send(telemetry.Event{
			Command:      "dump-autoload",
			Version:      Version,
			PackageCount: len(allPackages),
			DurationMs:   time.Since(start).Milliseconds(),
			Success:      true,
		})
	}

	return nil
}
