package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// newRootCmd builds and returns the root cobra command with all subcommands attached.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vif",
		Short:         "Fast PHP package manager",
		Long:          "vif is a fast PHP package manager that reads composer.lock and populates vendor/.",
		SilenceErrors: true,
	}

	root.AddCommand(newInstallCmd())

	return root
}

// Execute builds the command tree and runs it, printing errors to stderr and
// exiting with code 1 on failure.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
