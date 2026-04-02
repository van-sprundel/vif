package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/van-sprundel/vif/internal/autoload"
	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/downloader"
	"github.com/van-sprundel/vif/internal/installer"
	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/ui"
)

// installFromResolved downloads, installs, and generates autoload for resolved packages.
// c is a pre-opened cache shared with the resolution phase; it must not be nil.
func installFromResolved(ctx context.Context, w io.Writer, resolved []resolver.ResolvedPackage, cj *composer.ComposerJSON, verbose bool, c *cache.Cache) error {
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
		}
		if len(rp.Entry.Autoload) > 0 {
			if err := json.Unmarshal(rp.Entry.Autoload, &p.Autoload); err != nil {
				return fmt.Errorf("unmarshal autoload for %s: %w", rp.Name, err)
			}
		}
		if len(rp.Entry.AutoloadDev) > 0 {
			if err := json.Unmarshal(rp.Entry.AutoloadDev, &p.AutoloadDev); err != nil {
				return fmt.Errorf("unmarshal autoload-dev for %s: %w", rp.Name, err)
			}
		}
		if rp.Dev {
			devPkgs = append(devPkgs, p)
		} else {
			prodPkgs = append(prodPkgs, p)
		}
	}

	allPackages := append(prodPkgs, devPkgs...)
	total := len(allPackages)

	// Download.
	dl := downloader.New(c, 0)
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
	var skippedPathPackages []string
	for _, r := range results {
		if r.Err != nil {
			failed++
			progress.Error(fmt.Sprintf("  ERROR %s: %v", r.Package.Name, r.Err))
		} else if r.Skipped {
			skipped++
			if r.Package.Dist.Type == "path" {
				skippedPathPackages = append(skippedPathPackages, r.Package.Name)
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
	if len(skippedPathPackages) > 0 {
		sort.Strings(skippedPathPackages)
		return fmt.Errorf(
			"path repositories are not supported yet; skipped %d path package(s): %s",
			len(skippedPathPackages),
			strings.Join(skippedPathPackages, ", "),
		)
	}

	fmt.Fprintf(w, "  %d downloaded, %d from cache, %d skipped (non-downloadable)\n", downloaded, cached, skipped)

	// Install to vendor/.
	vendorDir := filepath.Join(".", "vendor")
	inst := installer.New(c)
	rootMeta := &installer.RootPackage{
		Name:    cj.Name,
		Version: cj.Version,
		Type:    cj.Type,
	}

	fmt.Fprintf(w, "Installing to %s...\n", vendorDir)
	if err := inst.Install(prodPkgs, devPkgs, vendorDir, rootMeta); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	// Generate autoloader.
	root := &autoload.RootAutoload{
		Name:        cj.Name,
		Autoload:    cj.Autoload,
		AutoloadDev: cj.AutoloadDev,
	}
	fmt.Fprint(w, "Generating autoload files...")
	if err := autoload.Generate(vendorDir, allPackages, cj.ContentHash(), cj.Config.OptimizeAutoloader, root, cj.Config.PrependAutoloaderOrDefault()); err != nil {
		return fmt.Errorf("autoload: %w", err)
	}
	fmt.Fprintln(w, " done")

	return nil
}
