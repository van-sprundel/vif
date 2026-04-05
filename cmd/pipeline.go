package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/van-sprundel/vif/internal/autoload"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/ui"
)

type installProfile struct {
	Download     time.Duration
	Install      time.Duration
	Autoload     time.Duration
	SlowPackages []ui.ProfilePackage
}

// installFromResolved downloads, installs, and generates autoload for resolved packages.
// c is a pre-opened cache shared with the resolution phase; it must not be nil.
func installFromResolved(ctx context.Context, w io.Writer, resolved []resolver.ResolvedPackage, cj *composer.ComposerJSON, verbose bool, noAutoloader bool, c *cache.Cache, profile bool) (*installProfile, error) {
	var prof *installProfile
	if profile {
		prof = &installProfile{}
	}

	var prodPkgs, devPkgs []pkg.Package
	for _, rp := range resolved {
		p := pkg.Package{
			Name:    rp.Name,
			Version: rp.Version,
			Type:    rp.Entry.Type,
			Bin:     rp.Entry.Bin,
			Dist: pkg.Dist{
				Type:      rp.Entry.Dist.Type,
				URL:       rp.Entry.Dist.URL,
				Reference: rp.Entry.Dist.Reference,
				Shasum:    rp.Entry.Dist.Shasum,
			},
			Source: pkg.Dist{
				Type:      rp.Entry.Source.Type,
				URL:       rp.Entry.Source.URL,
				Reference: rp.Entry.Source.Reference,
				Shasum:    rp.Entry.Source.Shasum,
			},
		}
		if len(rp.Entry.Autoload) > 0 {
			if err := json.Unmarshal(rp.Entry.Autoload, &p.Autoload); err != nil {
				return nil, fmt.Errorf("unmarshal autoload for %s: %w", rp.Name, err)
			}
		}
		if len(rp.Entry.AutoloadDev) > 0 {
			if err := json.Unmarshal(rp.Entry.AutoloadDev, &p.AutoloadDev); err != nil {
				return nil, fmt.Errorf("unmarshal autoload-dev for %s: %w", rp.Name, err)
			}
		}
		if rp.Dev {
			devPkgs = append(devPkgs, p)
		} else {
			prodPkgs = append(prodPkgs, p)
		}
	}

	allPackages := append(prodPkgs, devPkgs...)
	projectDir, err := filepath.Abs(".")
	if err != nil {
		return nil, fmt.Errorf("project dir: %w", err)
	}
	allPackages, err = applyLocalPathPackages(allPackages, cj.Repositories, projectDir)
	if err != nil {
		return nil, fmt.Errorf("path repositories: %w", err)
	}
	allPackages = promoteSourceToDist(allPackages, projectDir)
	prodCount := len(prodPkgs)
	prodPkgs = allPackages[:prodCount]
	devPkgs = allPackages[prodCount:]
	total := len(allPackages)

	// Download.
	dl := downloader.New(c, 0)
	auth, err := loadComposerAuth()
	if err != nil {
		return nil, err
	}
	dl.SetAuth(auth)
	progress := ui.NewProgress(w, "Downloading", total, verbose)

	downloadStart := time.Now()
	results, err := dl.Download(ctx, allPackages)
	if prof != nil {
		prof.Download = time.Since(downloadStart)
		prof.SlowPackages = make([]ui.ProfilePackage, 0, len(results))
	}
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}

	var cached, downloaded, skipped, failed int
	var skippedUnsupported []string
	for _, r := range results {
		if prof != nil {
			prof.SlowPackages = append(prof.SlowPackages, ui.ProfilePackage{
				Name:     r.Package.Name,
				Duration: r.Duration,
			})
		}
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
		return nil, fmt.Errorf("%d package(s) failed to download", failed)
	}
	if len(skippedUnsupported) > 0 {
		sort.Strings(skippedUnsupported)
		return nil, fmt.Errorf(
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

	// Install to vendor/.
	vendorDir := filepath.Join(".", "vendor")
	inst := installer.New(c)
	rootMeta := &installer.RootPackage{
		Name:    cj.Name,
		Version: cj.Version,
		Type:    cj.Type,
	}

	fmt.Fprintf(w, "Installing to %s...\n", vendorDir)
	installStart := time.Now()
	if _, err := inst.Install(prodPkgs, devPkgs, vendorDir, rootMeta); err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	if prof != nil {
		prof.Install = time.Since(installStart)
	}

	// Generate autoloader.
	root := &autoload.RootAutoload{
		Name:        cj.Name,
		Autoload:    cj.Autoload,
		AutoloadDev: cj.AutoloadDev,
	}
	if noAutoloader {
		fmt.Fprintln(w, "Skipping autoload generation (--no-autoloader)")
	} else {
		autoloadStart := time.Now()
		fmt.Fprint(w, "Generating autoload files...")
		platformCheckMode := autoload.PlatformCheckFull
		if !cj.Config.PlatformCheck.IsTrue() {
			platformCheckMode = autoload.PlatformCheckDisabled
		} else if cj.Config.PlatformCheck.IsPHPOnly() {
			platformCheckMode = autoload.PlatformCheckPHPOnly
		}

		devNames := make(map[string]bool, len(devPkgs))
		for _, p := range devPkgs {
			devNames[p.Name] = true
		}
		ivCfg := &autoload.InstalledVersionsConfig{
			RootName:        cj.Name,
			RootVersion:     cj.Version,
			RootType:        cj.Type,
			DevMode:         true,
			DevPackageNames: devNames,
		}

		if err := autoload.Generate(vendorDir, allPackages, cj.ContentHash(), cj.Config.OptimizeAutoloader, root, cj.Config.PrependAutoloaderOrDefault(), platformCheckMode, ivCfg); err != nil {
			return nil, fmt.Errorf("autoload: %w", err)
		}
		fmt.Fprintln(w, " done")
		if prof != nil {
			prof.Autoload = time.Since(autoloadStart)
		}
	}

	return prof, nil
}
