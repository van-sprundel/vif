package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/telemetry"
)

var Version = "dev"

// newRootCmd builds and returns the root cobra command with all subcommands attached.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vif",
		Short:         "Fast PHP package manager",
		Long:          "vif is a fast PHP package manager that reads composer.lock and populates vendor/.",
		SilenceErrors: true,
	}

	root.Version = Version
	root.AddCommand(newInstallCmd())
	root.AddCommand(newUpdateCmd())
	root.AddCommand(newRequireCmd())
	root.AddCommand(newTelemetryCmd())
	root.AddCommand(newSetupCompletionsCmd())
	root.AddCommand(newReportCmd())

	return root
}

// Execute builds the command tree and runs it, printing errors to stderr and
// exiting with code 1 on failure.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telemetry.Prompt()

	err := newRootCmd().ExecuteContext(ctx)
	telemetry.Wait()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
