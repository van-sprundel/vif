package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/pkg"
)

// newShowCmd returns the `vif show` command.
func newShowCmd() *cobra.Command {
	var (
		nameOnly bool
		showPath bool
		showSelf bool
		showTree bool
		noDev    bool
	)

	cmd := &cobra.Command{
		Use:          "show [package]",
		Aliases:      []string{"info"},
		Short:        "Show information about installed packages",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var pkgName string
			if len(args) > 0 {
				pkgName = args[0]
			}
			return runShow(pkgName, nameOnly, showPath, showSelf, showTree, noDev)
		},
	}

	cmd.Flags().BoolVarP(&nameOnly, "name-only", "N", false, "show only package names")
	cmd.Flags().BoolVarP(&showPath, "path", "P", false, "show install paths")
	cmd.Flags().BoolVarP(&showSelf, "self", "s", false, "show root package info")
	cmd.Flags().BoolVarP(&showTree, "tree", "t", false, "show dependency tree")
	cmd.Flags().BoolVar(&noDev, "no-dev", false, "exclude dev dependencies")

	return cmd
}

func runShow(pkgName string, nameOnly, showPath, showSelf, showTree, noDev bool) error {
	w := os.Stdout

	// Show root package info.
	if showSelf {
		cj, err := composer.Parse("composer.json")
		if err != nil {
			return fmt.Errorf("failed to read composer.json: %w", err)
		}
		fmt.Fprintf(w, "name     : %s\n", cj.Name)
		if cj.Version != "" {
			fmt.Fprintf(w, "version  : %s\n", cj.Version)
		}
		if cj.Type != "" {
			fmt.Fprintf(w, "type     : %s\n", cj.Type)
		}
		if len(cj.Require) > 0 {
			fmt.Fprintln(w, "\nrequires:")
			for name, constraint := range cj.Require {
				fmt.Fprintf(w, "  %s %s\n", name, constraint)
			}
		}
		if len(cj.RequireDev) > 0 {
			fmt.Fprintln(w, "\nrequires (dev):")
			for name, constraint := range cj.RequireDev {
				fmt.Fprintf(w, "  %s %s\n", name, constraint)
			}
		}
		return nil
	}

	// Parse lockfile.
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

	// Single package detail mode.
	if pkgName != "" {
		return showPackageDetail(w, allPackages, pkgName)
	}

	// Tree mode.
	if showTree {
		return showDependencyTree(w, packages, packagesDev, noDev)
	}

	// List mode.
	sort.Slice(allPackages, func(i, j int) bool {
		return allPackages[i].Name < allPackages[j].Name
	})

	devSet := make(map[string]bool, len(packagesDev))
	for _, p := range packagesDev {
		devSet[p.Name] = true
	}

	if nameOnly {
		for _, p := range allPackages {
			fmt.Fprintln(w, p.Name)
		}
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range allPackages {
		version := p.Version
		if showPath {
			installPath, _ := filepath.Abs(filepath.Join("vendor", p.Name))
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, version, installPath)
		} else {
			fmt.Fprintf(tw, "%s\t%s\n", p.Name, version)
		}
	}
	tw.Flush()

	return nil
}

func showPackageDetail(w *os.File, packages []pkg.Package, name string) error {
	var found *pkg.Package
	for i := range packages {
		if packages[i].Name == name {
			found = &packages[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("package %q is not installed", name)
	}

	p := found
	fmt.Fprintf(w, "name     : %s\n", p.Name)
	fmt.Fprintf(w, "version  : %s\n", p.Version)
	if p.Type != "" {
		fmt.Fprintf(w, "type     : %s\n", p.Type)
	}
	if p.Source.URL != "" {
		fmt.Fprintf(w, "source   : %s %s %s\n", p.Source.URL, p.Source.Type, p.Source.Reference)
	}
	if p.Dist.URL != "" {
		fmt.Fprintf(w, "dist     : %s %s %s\n", p.Dist.URL, p.Dist.Type, p.Dist.Reference)
	}

	if len(p.Require) > 0 {
		fmt.Fprintln(w, "\nrequires:")
		keys := sortedStringKeys(p.Require)
		for _, name := range keys {
			fmt.Fprintf(w, "  %s %s\n", name, p.Require[name])
		}
	}
	if len(p.RequireDev) > 0 {
		fmt.Fprintln(w, "\nrequires (dev):")
		keys := sortedStringKeys(p.RequireDev)
		for _, name := range keys {
			fmt.Fprintf(w, "  %s %s\n", name, p.RequireDev[name])
		}
	}
	if len(p.Provide) > 0 {
		fmt.Fprintln(w, "\nprovides:")
		for name, constraint := range p.Provide {
			fmt.Fprintf(w, "  %s %s\n", name, constraint)
		}
	}
	if len(p.Replace) > 0 {
		fmt.Fprintln(w, "\nreplaces:")
		for name, constraint := range p.Replace {
			fmt.Fprintf(w, "  %s %s\n", name, constraint)
		}
	}

	return nil
}

func showDependencyTree(w *os.File, packages, packagesDev []pkg.Package, noDev bool) error {
	// Build a name -> package lookup.
	byName := make(map[string]*pkg.Package, len(packages)+len(packagesDev))
	for i := range packages {
		byName[packages[i].Name] = &packages[i]
	}
	if !noDev {
		for i := range packagesDev {
			byName[packagesDev[i].Name] = &packagesDev[i]
		}
	}

	// Load root package to get direct dependencies.
	cj, err := composer.Parse("composer.json")
	if err != nil {
		return fmt.Errorf("failed to read composer.json: %w", err)
	}

	printTreeNode := func(name, constraint string, depth int, last bool) {
		prefix := ""
		if depth > 0 {
			prefix = strings.Repeat("│  ", depth-1)
			if last {
				prefix += "└──"
			} else {
				prefix += "├──"
			}
		}
		if p, ok := byName[name]; ok {
			fmt.Fprintf(w, "%s%s %s\n", prefix, p.Name, p.Version)
		} else {
			fmt.Fprintf(w, "%s%s %s\n", prefix, name, constraint)
		}
	}

	// Print direct dependencies as trees.
	deps := sortedStringKeys(cj.Require)
	for i, name := range deps {
		if pkg.IsPlatformPackage(name) {
			continue
		}
		printTreeNode(name, cj.Require[name], 0, i == len(deps)-1)
		if p, ok := byName[name]; ok {
			subDeps := sortedStringKeys(p.Require)
			for j, subName := range subDeps {
				if pkg.IsPlatformPackage(subName) {
					continue
				}
				printTreeNode(subName, p.Require[subName], 1, j == len(subDeps)-1)
			}
		}
	}

	return nil
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
