package config

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spf13/cobra"
)

func init() {
	tf := flags.TargetFlags{}

	var configGetCmd = &cobra.Command{
		Use:   "get [-m <machine-id>|--global]",
		Short: "Show the global configuration, or the configuration a machine gets (this Mac without --machine)",
		Long: `Show the global configuration, or the configuration a machine gets: its own settings, marked (machine), and
the global value of every other setting, marked (inherited). Without --global or --machine, this Mac's configuration
is shown.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireAtMostOneTarget(); err != nil {
				return err
			}

			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient, err := dynamodb.GetClient(table, region)

			if err != nil {

				return err

			}
			if err := tf.ResolveMachineID(dynamodbClient); err != nil {
				return err
			}
			timeProvider := clock.ConcreteTimeProvider{}

			service := machineconfiguration.GetMachineConfigurationService(dynamodbClient, timeProvider)

			return getConfig(service, dynamodbClient, tf)
		},
	}

	tf.AddTargetFlags(configGetCmd)

	ConfigCmd.AddCommand(configGetCmd)
}

// settingOrigin returns " (machine)" for a setting (named by its attribute) that a machine's own configuration
// stores and the machine gets, " (inherited)" for one that follows the global configuration, and "" for the global configuration
type settingOrigin func(attribute string) string

func noOrigin(string) string { return "" }

func machineOrigin(stored map[string]awstypes.AttributeValue) settingOrigin {
	inUse := machineconfiguration.MachineSettingsInUse(stored)
	return func(attribute string) string {
		decodable, ok := inUse[attribute]
		switch {
		case ok && decodable:
			return " (machine)"
		case ok:
			return " (machine, but the stored value cannot be read, so none is sent)"
		}
		return " (inherited)"
	}
}

func getConfig(service machineconfiguration.MachineConfigurationService, client dynamodb.GetItemAPI, tf flags.TargetFlags) (err error) {
	var subject string
	var config machineconfiguration.MachineConfiguration
	origin := settingOrigin(noOrigin)

	if tf.IsGlobal {
		fmt.Println("Retrieving the global configuration...")
		subject = "All Machines"

		tmpconfig, _, eerr := service.GetIntendedGlobalConfig()
		if eerr != nil {
			err = fmt.Errorf("failed to do DynamoDB get: %w", eerr)
			return
		}
		config = tmpconfig
	} else {
		machineID, eerr := tf.GetMachineID()
		if eerr != nil {
			err = fmt.Errorf("failed to get MachineUUID: %w", eerr)
			return
		}
		if tf.IsTargetSelf() {
			fmt.Printf("Retrieving the configuration of this Mac (%s)\n", machineID)
			subject = fmt.Sprintf("Machine (%s) (This Machine)", machineID)
		} else {
			fmt.Printf("Retrieving the configuration of machine %s\n", machineID)
			subject = fmt.Sprintf("Machine (%s)", machineID)
		}
		tmpconfig, eerr := service.GetIntendedConfig(machineID)
		if eerr != nil {
			err = fmt.Errorf("failed to do dynamodb get: %w", eerr)
			return
		}
		config = tmpconfig
		stored, eerr := client.GetItem(machineconfiguration.MachineConfigurationKey(machineID), false)
		if eerr != nil {
			err = fmt.Errorf("failed to read the machine's own configuration: %w", eerr)
			return
		}
		origin = machineOrigin(stored.Item)
	}

	// A stored client mode Santa does not know is shown, not fatal (preflight leaves it out)
	clientModeText, eerr := config.ClientMode.MarshalText()
	if eerr != nil {
		clientModeText = []byte("INVALID, not sent to Santa")
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 8, 0, '\t', tabwriter.AlignRight)

	fmt.Println("Sync server returned the following configuration")
	fmt.Println()
	fmt.Fprintln(writer, "Config\t Setting")
	fmt.Fprintln(writer, "Target:\t", subject)
	fmt.Fprintf(writer, "ClientMode:\t %d --> ( %s )%s\n", config.ClientMode, clientModeText, origin("ClientMode"))
	if !tf.IsGlobal {
		printTemporaryMode(writer, config, time.Now())
	}
	fmt.Fprintf(writer, "BlockedPathRegex:\t %q%s\n", config.BlockedPathRegex, origin("BlockedPathRegex"))
	fmt.Fprintf(writer, "AllowedPathRegex:\t %q%s\n", config.AllowedPathRegex, origin("AllowedPathRegex"))
	fmt.Fprintf(writer, "BatchSize:\t %d%s\n", config.BatchSize, origin("BatchSize"))
	fmt.Fprintf(writer, "BundlesEnabled:\t %t%s\n", config.EnableBundles, origin("EnableBundles"))
	fmt.Fprintf(writer, "EnabledTransitiveRules:\t %t%s\n", config.EnabledTransitiveRules, origin("EnableTransitiveRules"))
	if tf.IsGlobal && config.CleanSync {
		// Stored by an earlier version, which accepted --global --clean-sync
		fmt.Fprintln(writer, "CleanSync:\t", "true (ignored: a clean sync is requested per machine; remove it with config update --global --clean-sync=false)")
	} else {
		fmt.Fprintln(writer, "CleanSync:\t", config.CleanSync)
	}
	fmt.Fprintf(writer, "FullSyncInterval:\t %d%s\n", config.FullSyncInterval, origin("FullSyncInterval"))
	fmt.Fprintf(writer, "UploadLogUrl:\t %q%s\n", config.UploadLogsURL, origin("UploadLogsUrl"))
	printSantaSettings(writer, config, origin)
	writer.Flush()

	return
}
