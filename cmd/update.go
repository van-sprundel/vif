package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"path/filepath"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/packagist"
	"github.com/van-sprundel/vif/internal/resolver"
	"github.com/van-sprundel/vif/internal/scripts"
	"github.com/van-sprundel/vif/internal/telemetry"
	"github.com/van-sprundel/vif/internal/ui"
	"github.com/van-sprundel/vif/internal/version"
)

// newUpdateCmd returns the `vif update` command.
func newUpdateCmd() *cobra.Command {
	var verbose bool
	var noDev bool
	var noAutoloader bool
	var noScripts bool
	var ignorePlatformReqs bool
	var profile bool

	cmd := &cobra.Command{
		Use:          "update [packages...]",
		Short:        "Resolve dependencies and update composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd.Context(), args, verbose, noDev, noAutoloader, noScripts, ignorePlatformReqs, profile)
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-package output")
	cmd.Flags().BoolVar(&noDev, "no-dev", false, "skip dev dependencies")
	cmd.Flags().BoolVar(&noAutoloader, "no-autoloader", false, "skip autoloader generation")
	cmd.Flags().BoolVar(&noScripts, "no-scripts", false, "skip execution of scripts defined in composer.json")
	cmd.Flags().BoolVar(&ignorePlatformReqs, "ignore-platform-reqs", false, "ignore php/ext platform requirements while resolving")
	cmd.Flags().BoolVar(&profile, "profile", false, "print per-phase timings and slowest packages")

	return cmd
}

func runUpdate(ctx context.Context, packages []string, verbose bool, noDev bool, noAutoloader bool, noScripts bool, ignorePlatformReqs bool, profile bool) (retErr error) {
	start := time.Now()
	w := os.Stderr

	defer func() {
		if retErr != nil && telemetry.Enabled() {
			telemetry.Send(telemetry.Event{
				Command:    "update",
				Version:    Version,
				DurationMs: time.Since(start).Milliseconds(),
				ErrorType:  telemetry.ErrorCategory(retErr),
			})
		}
	}()

	parseComposerStart := time.Now()
	cj, err := composer.Parse("composer.json")
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}
	parseComposerDuration := time.Since(parseComposerStart)
	fmt.Fprintf(w, "Resolving dependencies for %s...\n", cj.Name)

	var lockedEntries map[string]packagist.VersionEntry
	var locked map[string]string
	var fixed map[string]string
	var oldLock map[string]string // preserved for delta reporting
	lockfileDuration := time.Duration(0)
	lockfileReadStart := time.Now()
	if existingLock, err := lockfile.Parse("composer.lock"); err == nil {
		lockedEntries = existingLock.LockedEntries()
		fixed = make(map[string]string, len(existingLock.Packages)+len(existingLock.PackagesDev))
		for _, p := range existingLock.Packages {
			fixed[p.Name] = p.Version
		}
		for _, p := range existingLock.PackagesDev {
			fixed[p.Name] = p.Version
		}
		oldLock = make(map[string]string, len(fixed))
		for k, v := range fixed {
			oldLock[k] = v
		}

		if len(packages) > 0 {
			updateSet := make(map[string]struct{}, len(packages))
			for _, p := range packages {
				updateSet[p] = struct{}{}
			}

			locked = make(map[string]string, len(existingLock.Packages)+len(existingLock.PackagesDev))
			for _, p := range existingLock.Packages {
				if _, ok := updateSet[p.Name]; !ok {
					locked[p.Name] = p.Version
					fixed[p.Name] = p.Version
				} else {
					delete(fixed, p.Name)
				}
			}
			for _, p := range existingLock.PackagesDev {
				if _, ok := updateSet[p.Name]; !ok {
					locked[p.Name] = p.Version
					fixed[p.Name] = p.Version
				} else {
					delete(fixed, p.Name)
				}
			}
		} else {
			// Full update should not pin to existing lock versions.
			fixed = nil
		}
	} else if len(packages) > 0 {
		return fmt.Errorf("cannot partially update without an existing composer.lock")
	}
	lockfileDuration = time.Since(lockfileReadStart)

	cacheInitStart := time.Now()
	cacheDir, err := cacheDirectory()
	if err != nil {
		return fmt.Errorf("cache directory: %w", err)
	}
	c, err := cache.New(cacheDir)
	if err != nil {
		return fmt.Errorf("cache init: %w", err)
	}
	defer c.Close()
	cacheInitDuration := time.Since(cacheInitStart)

	metadataClientStart := time.Now()
	client, err := metadataClient(cj, c)
	if err != nil {
		return err
	}
	metadataClientDuration := time.Since(metadataClientStart)

	restrictionsStart := time.Now()
	restrictedPackages, restriction, err := resolveRestrictedPackages(ctx, client, cj)
	if err != nil {
		return err
	}
	restrictionsDuration := time.Since(restrictionsStart)

	if len(packages) > 0 {
		fmt.Fprintf(w, "Partially updating: %s\n", formatPackageList(packages))
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
		if !profile {
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
	var resolveLookups []ui.ProfilePackage
	var resolveLookupsMu sync.Mutex
	onLookupDone := func(name string, d time.Duration, err error) {
		if verbose {
			progress.Error(formatLookupLog(name, d, err))
		}
		if profile {
			displayName := name
			if err != nil {
				displayName = fmt.Sprintf("%s (error)", name)
			}
			resolveLookupsMu.Lock()
			resolveLookups = append(resolveLookups, ui.ProfilePackage{Name: displayName, Duration: d})
			resolveLookupsMu.Unlock()
		}
	}
	resolveStart := time.Now()
	resolved, err := resolver.ResolveWithOptions(ctx, cj, client, resolver.Options{
		Fixed:              fixed,
		IgnorePlatformReqs: ignorePlatformReqs,
		RestrictedPackages: restrictedPackages,
		Restriction:        restriction,
		LockedEntries:      lockedEntries,
		Locked:             locked,
		NoDev:              noDev,
		LookupDone:         onLookupDone,
		SolveProgress:      onSolveProgress,
	}, func(name string) {
		progress.Increment(name)
	})
	progress.Finish()
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	resolveDuration := time.Since(resolveStart)

	fmt.Fprintf(w, "Resolved %d packages\n", len(resolved))

	// Print lock file deltas when an existing lock was present.
	if len(oldLock) > 0 {
		newResolved := make(map[string]string, len(resolved))
		for _, rp := range resolved {
			newResolved[rp.Name] = rp.Version
		}
		var added, updated, removed int
		for _, rp := range resolved {
			if oldVer, ok := oldLock[rp.Name]; !ok {
				added++
			} else if oldVer != rp.Version {
				updated++
			}
		}
		for name := range oldLock {
			if _, ok := newResolved[name]; !ok {
				removed++
			}
		}
		unchanged := len(resolved) - added - updated
		fmt.Fprintf(w, "Lock file operations: %d installs, %d updates, %d removals, %d unchanged\n",
			added, updated, removed, unchanged)
	}

	composerWriteDuration := time.Duration(0)
	if changed := applyBumpAfterUpdate(cj, resolved, noDev); changed {
		composerWriteStart := time.Now()
		if err := cj.Write("composer.json"); err != nil {
			return fmt.Errorf("write composer.json: %w", err)
		}
		composerWriteDuration = time.Since(composerWriteStart)
		fmt.Fprintln(w, "Updated composer.json")
	}

	installProfile, err := installFromResolved(ctx, w, resolved, cj, verbose, noAutoloader, c, profile)
	if err != nil {
		return err
	}

	lockPath := "composer.lock"
	lockfileWriteStart := time.Now()
	if err := lockfile.Generate(lockPath, resolved, cj); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	lockfileWriteDuration := time.Since(lockfileWriteStart)
	fmt.Fprintf(w, "Wrote %s\n", lockPath)

	// Run post-update-cmd scripts.
	if !noScripts {
		projectDir, _ := filepath.Abs(".")
		runner := scripts.New(projectDir, cj.Scripts, cj.Extra.Symfony.AutoScripts, w)
		if err := runner.Run("post-autoload-dump"); err != nil {
			return err
		}
		if err := runner.Run("post-update-cmd"); err != nil {
			return err
		}
	}

	ui.PrintSummary(w, len(resolved), start)

	if telemetry.Enabled() {
		ev := telemetry.Event{
			Command:      "update",
			Version:      Version,
			PackageCount: len(resolved),
			DurationMs:   time.Since(start).Milliseconds(),
			Success:      true,
			Phases: map[string]int64{
				"parse":      parseComposerDuration.Milliseconds(),
				"lockfile":   lockfileDuration.Milliseconds(),
				"cache_init": cacheInitDuration.Milliseconds(),
				"metadata":   metadataClientDuration.Milliseconds(),
				"restrict":   restrictionsDuration.Milliseconds(),
				"resolve":    resolveDuration.Milliseconds(),
				"write_json": composerWriteDuration.Milliseconds(),
				"write_lock": lockfileWriteDuration.Milliseconds(),
			},
		}
		if installProfile != nil {
			ev.Phases["download"] = installProfile.Download.Milliseconds()
			ev.Phases["install"] = installProfile.Install.Milliseconds()
			ev.Phases["autoload"] = installProfile.Autoload.Milliseconds()
		}
		telemetry.Send(ev)
	}

	if profile {
		sections := []ui.ProfileSection{
			{Name: "parse composer.json", Duration: parseComposerDuration},
			{Name: "read lockfile", Duration: lockfileDuration},
			{Name: "init cache", Duration: cacheInitDuration},
			{Name: "init metadata client", Duration: metadataClientDuration},
			{Name: "resolve restrictions", Duration: restrictionsDuration},
			{Name: "resolve", Duration: resolveDuration},
			{Name: "write composer.json", Duration: composerWriteDuration},
			{Name: "write lockfile", Duration: lockfileWriteDuration},
		}
		var slowPackages []ui.ProfilePackage
		if installProfile != nil {
			sections = append(sections,
				ui.ProfileSection{Name: "download", Duration: installProfile.Download},
				ui.ProfileSection{Name: "install", Duration: installProfile.Install},
			)
			if !noAutoloader {
				sections = append(sections, ui.ProfileSection{Name: "autoload", Duration: installProfile.Autoload})
			}
			slowPackages = installProfile.SlowPackages
		}
		ui.PrintProfile(w, time.Since(start), sections, slowPackages)
		if len(resolveLookups) > 0 {
			sort.Slice(resolveLookups, func(i, j int) bool {
				if resolveLookups[i].Duration == resolveLookups[j].Duration {
					return resolveLookups[i].Name < resolveLookups[j].Name
				}
				return resolveLookups[i].Duration > resolveLookups[j].Duration
			})
			if len(resolveLookups) > 8 {
				resolveLookups = resolveLookups[:8]
			}
			fmt.Fprintln(w, "  slowest metadata lookups:")
			for i, lookup := range resolveLookups {
				fmt.Fprintf(w, "    %d. %s (%s)\n", i+1, lookup.Name, profileDuration(lookup.Duration))
			}
		}
	}

	return nil
}

func formatPackageList(packages []string) string {
	if len(packages) <= 5 {
		return strings.Join(packages, ", ")
	}
	return strings.Join(packages[:5], ", ") + fmt.Sprintf(" and %d more", len(packages)-5)
}

func profileDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func applyBumpAfterUpdate(cj *composer.ComposerJSON, resolved []resolver.ResolvedPackage, noDev bool) bool {
	mode := cj.BumpAfterUpdateMode()
	if mode == "" {
		return false
	}

	resolvedByName := make(map[string]string, len(resolved))
	for _, rp := range resolved {
		resolvedByName[rp.Name] = rp.Version
	}

	changed := false
	if mode == "all" || mode == "no-dev" {
		for name, current := range cj.Require {
			resolvedVersion, ok := resolvedByName[name]
			if !ok {
				continue
			}
			bumped, ok := bumpConstraint(current, resolvedVersion)
			if !ok || bumped == current {
				continue
			}
			cj.AddRequire(name, bumped)
			changed = true
		}
	}

	if !noDev && (mode == "all" || mode == "dev") {
		for name, current := range cj.RequireDev {
			resolvedVersion, ok := resolvedByName[name]
			if !ok {
				continue
			}
			bumped, ok := bumpConstraint(current, resolvedVersion)
			if !ok || bumped == current {
				continue
			}
			cj.AddRequireDev(name, bumped)
			changed = true
		}
	}

	return changed
}

func bumpConstraint(current, resolvedVersion string) (string, bool) {
	version := normalizeBumpVersion(resolvedVersion)
	if version == "" {
		return "", false
	}

	base, suffix := splitStabilitySuffix(current)
	base = strings.TrimSpace(base)
	if base == "" || strings.Contains(base, "||") || strings.Contains(base, ",") {
		return "", false
	}

	switch {
	case strings.HasPrefix(base, "^"):
		if !isSimpleVersion(strings.TrimSpace(base[1:])) {
			return "", false
		}
		return "^" + version + suffix, true
	case strings.HasPrefix(base, "~"):
		if !isSimpleVersion(strings.TrimSpace(base[1:])) {
			return "", false
		}
		return "~" + version + suffix, true
	case strings.HasPrefix(base, ">="):
		if !isSimpleVersion(strings.TrimSpace(base[2:])) {
			return "", false
		}
		return ">=" + version + suffix, true
	case isSimpleVersion(base):
		prefix := ""
		if strings.HasPrefix(base, "v") || strings.HasPrefix(base, "V") {
			prefix = "v"
		}
		return prefix + version + suffix, true
	default:
		return "", false
	}
}

func splitStabilitySuffix(constraint string) (string, string) {
	i := strings.Index(constraint, "@")
	if i == -1 {
		return constraint, ""
	}
	return constraint[:i], constraint[i:]
}

func normalizeBumpVersion(version string) string {
	v := strings.TrimSpace(version)
	if len(v) >= 2 && (v[0] == 'v' || v[0] == 'V') && isDigit(v[1]) {
		v = v[1:]
	}
	if !isSimpleVersion(v) {
		return ""
	}
	return v
}

func isSimpleVersion(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	dotCount := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '.' {
			dotCount++
			if dotCount > 3 {
				return false
			}
			continue
		}
		if !isDigit(ch) {
			return false
		}
	}
	return true
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

func formatLookupLog(name string, d time.Duration, err error) string {
	if err != nil {
		return fmt.Sprintf("  Lookup %s (%s, error: %v)", name, profileDuration(d), err)
	}
	return fmt.Sprintf("  Lookup %s (%s)", name, profileDuration(d))
}

func formatRepositoryLookupLog(trace packagist.LookupTrace) string {
	status := "hit"
	switch {
	case trace.Err == nil:
		status = "hit"
	case errors.Is(trace.Err, packagist.ErrPackageNotFound):
		status = "not found"
	case errors.Is(trace.Err, packagist.ErrAuthRequired):
		status = "auth required"
	case isTransientMetadataError(trace.Err):
		status = "transient error"
	default:
		status = "error"
	}

	if trace.Err != nil && status != "not found" && status != "auth required" && status != "transient error" {
		return fmt.Sprintf("  Repo %s %s (%s, %s: %v)", trace.Source, trace.Package, profileDuration(trace.Duration), status, trace.Err)
	}
	if trace.Err != nil && status == "transient error" {
		return fmt.Sprintf("  Repo %s %s (%s, %s: %v)", trace.Source, trace.Package, profileDuration(trace.Duration), status, trace.Err)
	}
	return fmt.Sprintf("  Repo %s %s (%s, %s)", trace.Source, trace.Package, profileDuration(trace.Duration), status)
}

func isTransientMetadataError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
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
