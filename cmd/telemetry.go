package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/telemetry"
)

func newTelemetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "telemetry [on|off]",
		Short: "Manage anonymous telemetry",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if telemetry.Enabled() {
					fmt.Fprintln(os.Stderr, "Telemetry is enabled.")
				} else {
					fmt.Fprintln(os.Stderr, "Telemetry is disabled.")
				}
				return nil
			}

			switch args[0] {
			case "on":
				telemetry.SetEnabled(true)
				fmt.Fprintln(os.Stderr, "Telemetry enabled.")
			case "off":
				telemetry.SetEnabled(false)
				fmt.Fprintln(os.Stderr, "Telemetry disabled.")
			default:
				return fmt.Errorf("usage: vif telemetry [on|off]")
			}
			return nil
		},
	}
	return cmd
}
