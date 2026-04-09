package cmd

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/lockfile"
	"github.com/van-sprundel/vif/internal/pkg"
)

// newBrowseCmd returns the `vif browse` command.
func newBrowseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "browse <package>",
		Aliases:      []string{"home"},
		Short:        "Open a package's repository URL in the browser",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrowse(args[0])
		},
	}

	return cmd
}

func runBrowse(name string) error {
	// Try to find the package in the lockfile first.
	lf, err := lockfile.Parse("composer.lock")
	if err == nil {
		allPackages := append(lf.Packages, lf.PackagesDev...)
		for _, p := range allPackages {
			if p.Name == name {
				return openPackageURL(p)
			}
		}
	}

	// Fallback: construct Packagist URL.
	return openURL("https://packagist.org/packages/" + name)
}

func openPackageURL(p pkg.Package) error {
	// Prefer source URL (repository).
	url := p.Source.URL
	if url == "" {
		url = p.Dist.URL
	}
	if url == "" {
		url = "https://packagist.org/packages/" + p.Name
	}

	// Clean up git URLs for browser.
	url = strings.TrimSuffix(url, ".git")

	return openURL(url)
}

func openURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to open browser: %w", err)
	}
	return nil
}
