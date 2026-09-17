package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

func init() {
	var (
		clientModeArg            flags.ClientMode
		pathRegexArgs            pathRegexFlags
		batchSizeArg             int
		isEnableBundles          bool
		isEnabledTransitiveRules bool
		isCleanSync              bool
		fullSyncIntervalArg      int
		uploadLogsUrlArgs        string
		santaSettingsArgs        santaSettingsFlags
		confirmGlobalLockdown    bool
	)

	tf := flags.TargetFlags{}

	var configSetCmd = &cobra.Command{
		Use:   "set [-m <machine-id>|--global] [-c <ClientMode - 'monitor' or 'lockdown'>|--client-mode]",
		Short: "Create a configuration and set globally or a specific machine UUID",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate the flags before connecting to the sync server
			blockedPathRegex, err := pathRegexArgs.blockedPathRegex()
			if err != nil {
				return err
			}
			allowedPathRegex, err := pathRegexArgs.allowedPathRegex()
			if err != nil {
				return err
			}

			newConfig := machineconfiguration.MachineConfiguration{
				ClientMode:             clientModeArg.AsClientMode(),
				AllowedPathRegex:       allowedPathRegex,
				BlockedPathRegex:       blockedPathRegex,
				BatchSize:              batchSizeArg,
				EnableBundles:          isEnableBundles,
				EnabledTransitiveRules: isEnabledTransitiveRules,
				CleanSync:              isCleanSync,
				FullSyncInterval:       fullSyncIntervalArg,
				UploadLogsURL:          uploadLogsUrlArgs,
			}
			err = santaSettingsArgs.applyTo(cmd, &newConfig)
			if err != nil {
				return err
			}

			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			timeProvider := clock.ConcreteTimeProvider{}

			service := machineconfiguration.GetMachineConfigurationService(dynamodbClient, timeProvider)

			return applyConfig(
				service,
				tf,
				newConfig,
				confirmGlobalLockdown,
			)
		},
	}

	tf.AddTargetFlags(configSetCmd)

	// client-mode should be one of "monitor" or "lockdown"
	configSetCmd.Flags().VarP(&clientModeArg, "client-mode", "c", `type of client mode being applied. valid options are: "monitor" or "lockdown"`)
	// Mark as a required flag
	_ = configSetCmd.MarkFlagRequired("client-mode")
	addConfirmGlobalLockdownFlag(configSetCmd, &confirmGlobalLockdown)

	// Flags defining blocked and allowed regex paths
	pathRegexArgs.addFlags(configSetCmd)

	// Flags defining batchSize and events to sync back to the server
	configSetCmd.Flags().IntVarP(&batchSizeArg, "batch-size", "s", 50, "Int value to define the number of rules to download per sync request. Defaults to '50'")
	configSetCmd.Flags().BoolVar(&isEnableBundles, "bundles", false, "Enables bundle events to be uploaded back to the sync server")
	configSetCmd.Flags().BoolVar(&isEnabledTransitiveRules, "transitive-rules", false, "Enables the usage of transitive rules")

	// Flags define sync behavior and upload logs url
	configSetCmd.Flags().BoolVar(&isCleanSync, "clean-sync", false, "Enforces that the next sync process will be a clean-sync. Only enforced on machine specific configurations")
	configSetCmd.Flags().StringVarP(&uploadLogsUrlArgs, "upload-logs", "u", "", "Set an upload logs URL link here. If using the sync server, define the API endpoint of the sync server logs upload path")

	configSetCmd.Flags().IntVarP(&fullSyncIntervalArg, "full-sync-interval", "f", machineconfiguration.DefaultFullSyncInterval, "Set full sync interval in seconds (default 600)")

	// Flags for optional Santa settings; settings without a flag are not configured
	santaSettingsArgs.addFlags(configSetCmd)

	ConfigCmd.AddCommand(configSetCmd)
}

func applyConfig(
	service machineconfiguration.MachineConfigurationService,
	tf flags.TargetFlags,
	newConfig machineconfiguration.MachineConfiguration,
	confirmGlobalLockdown bool) (err error) {
	clientMode := newConfig.ClientMode

	// Get machineID from flags
	var machineID string
	if tf.IsGlobal {
		// Fail before asking for confirmation; the service enforces this as well
		if clientMode == types.Lockdown && !confirmGlobalLockdown {
			return wrapWriteError(machineconfiguration.ErrGlobalLockdownNotConfirmed)
		}
		machineID = "(Global)"
	} else {
		machineID, err = tf.GetMachineID()
		if err != nil {
			return fmt.Errorf("failed to get MachineID: %w", err)
		}
	}

	suffix := ""
	if !(tf.IsGlobal) && tf.IsTargetSelf() {
		suffix = "-->( This machine )"
	}

	clientModeText, err := clientMode.MarshalText()
	if err != nil {
		return
	}

	// Print the output for visual confirmation via a nicely tab corrected output
	writer := tabwriter.NewWriter(os.Stdout, 0, 8, 0, '\t', tabwriter.AlignRight)

	fmt.Println("Setting the following configuration")
	fmt.Println()
	fmt.Fprintln(writer, "Config\t Setting")
	fmt.Fprintln(writer, "MachineID:\t", machineID, suffix)
	fmt.Fprintln(writer, "ClientMode:\t", clientMode, "-->(", string(clientModeText), ")")
	fmt.Fprintln(writer, "BlockedPathRegex:\t \"", newConfig.BlockedPathRegex, "\"")
	fmt.Fprintln(writer, "AllowedPathRegex:\t \"", newConfig.AllowedPathRegex, "\"")
	fmt.Fprintln(writer, "BatchSize:\t", newConfig.BatchSize)
	fmt.Fprintln(writer, "BundlesEnabled:\t", newConfig.EnableBundles)
	fmt.Fprintln(writer, "EnabledTransitiveRules:\t", newConfig.EnabledTransitiveRules)
	fmt.Fprintln(writer, "CleanSync:\t", newConfig.CleanSync)
	fmt.Fprintln(writer, "FullSyncInterval:\t", newConfig.FullSyncInterval)
	fmt.Fprintln(writer, "UploadLogUrl:\t \"", newConfig.UploadLogsURL, "\"")
	printSantaSettings(writer, newConfig)
	writer.Flush()
	fmt.Println()
	fmt.Println(`Apply changes? (Enter: "yes" or "ok")`)
	fmt.Print("> ")

	// Read confirmation
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	text = strings.Replace(text, "\n", "", -1)
	if text == "ok" || text == "yes" {
		fmt.Println("Sending the configuration to DynamoDB...")
	} else {
		fmt.Println("Confirmation not successful...")
		return
	}

	if tf.IsGlobal {
		err = service.SetGlobalConfig(newConfig, globalConfigOptions(confirmGlobalLockdown)...)
	} else {
		err = service.SetMachineConfig(machineID, newConfig)
	}

	if err != nil {
		return wrapWriteError(err)
	} else {
		fmt.Println("Success! Configuration was sent properly to DynamoDB...")
	}
	return

}
