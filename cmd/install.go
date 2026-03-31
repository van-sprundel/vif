package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// newInstallCmd returns the `vif install` command.
func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install packages from composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("not implemented")
		},
	}

	return cmd
}
