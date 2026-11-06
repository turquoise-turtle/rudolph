package config

import (
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/spf13/cobra"
)

var (
	ConfigCmd *cobra.Command
)

func init() {
	ConfigCmd = &cobra.Command{
		Use:   "config",
		Short: "Show and change the Santa configuration Rudolph sends, globally or for one machine",
	}
	flags.AddYesFlag(ConfigCmd)
	flags.AddDryRunFlag(ConfigCmd)
}
