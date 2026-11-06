package config

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
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
		Use:   "set [-m <machine-id>|--global] -c <client-mode> [<setting flags>]",
		Short: "Replace the global configuration, or a machine's own configuration, with the settings given",
		Long: `Create a configuration and set globally or a specific machine UUID.

The configuration replaces the whole stored configuration. For a machine, this also cancels a clean sync requested
for it that has not been performed yet (which, for a Mac that is offline, can stay pending for weeks), unless
--clean-sync is given again; use "config update" to change settings without touching a requested clean sync.
A temporary client mode of the machine that has not ended is kept (see config update --for); end it with
config update --end-temporary-mode.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireAtMostOneTarget(); err != nil {
				return err
			}
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
			if err := tf.ResolveMachineID(dynamodbClient); err != nil {
				return err
			}
			timeProvider := clock.ConcreteTimeProvider{}

			service := machineconfiguration.GetMachineConfigurationService(dynamodbClient, timeProvider)

			return applyConfig(
				service,
				tf,
				newConfig,
				confirmGlobalLockdown,
				flags.AssumeYes(cmd),
			)
		},
	}

	tf.AddTargetFlags(configSetCmd)

	// client-mode should be one of "monitor", "lockdown" or "standalone"
	configSetCmd.Flags().VarP(&clientModeArg, "client-mode", "c", `type of client mode being applied. valid options are: "monitor", "lockdown" or "standalone"`)
	// Mark as a required flag
	_ = configSetCmd.MarkFlagRequired("client-mode")
	addConfirmGlobalLockdownFlag(configSetCmd, &confirmGlobalLockdown)

	// Flags defining blocked and allowed regex paths
	pathRegexArgs.addFlags(configSetCmd)

	// Flags defining batchSize and events to sync back to the server
	configSetCmd.Flags().IntVarP(&batchSizeArg, "batch-size", "s", machineconfiguration.DefaultBatchSize, "Number of events Santa uploads per request, between 1 and 500")
	configSetCmd.Flags().BoolVar(&isEnableBundles, "bundles", false, "Enables bundle events to be uploaded back to the sync server")
	configSetCmd.Flags().BoolVar(&isEnabledTransitiveRules, "transitive-rules", false, "Enables the usage of transitive rules")

	// Flags define sync behavior and upload logs url
	configSetCmd.Flags().BoolVar(&isCleanSync, flagCleanSync, false, "Makes the next sync of the machine a clean sync, once. Only for a machine (--machine); refused with --global. Without it, a clean sync requested earlier and not yet performed is cancelled")
	configSetCmd.Flags().StringVarP(&uploadLogsUrlArgs, "upload-logs", "u", "", "Set an upload logs URL link here. If using the sync server, define the API endpoint of the sync server logs upload path")

	// -s and -u are kept for scripts written for earlier versions; -f, the old full sync interval shorthand, now
	// always means a file path, so it is gone
	_ = configSetCmd.Flags().MarkShorthandDeprecated("batch-size", "use --batch-size")
	_ = configSetCmd.Flags().MarkShorthandDeprecated("upload-logs", "use --upload-logs")
	configSetCmd.Flags().IntVar(&fullSyncIntervalArg, "full-sync-interval", machineconfiguration.DefaultFullSyncInterval, "Full sync interval in seconds, at least 60")

	// Flags for optional Santa settings; settings without a flag are not configured
	santaSettingsArgs.addFlags(configSetCmd)

	ConfigCmd.AddCommand(configSetCmd)
}

func applyConfig(
	service machineconfiguration.MachineConfigurationService,
	tf flags.TargetFlags,
	newConfig machineconfiguration.MachineConfiguration,
	confirmGlobalLockdown bool,
	yes bool) (err error) {
	clientMode := newConfig.ClientMode

	// Refuse invalid values before asking for confirmation; the service enforces this as well
	if err = machineconfiguration.ValidateConfig(newConfig, tf.IsGlobal); err != nil {
		return err
	}

	// Get machineID from flags
	var machineID string
	var temporaryModeSnapshot *machineconfiguration.TemporaryClientModeSnapshot
	if tf.IsGlobal {
		// Fail before asking for confirmation; the service enforces this as well
		if machineconfiguration.RequiresGlobalLockdownConfirmation(clientMode) && !confirmGlobalLockdown {
			return wrapWriteError(machineconfiguration.ErrGlobalLockdownNotConfirmed)
		}
		machineID = "(Global)"
	} else {
		machineID, err = tf.GetMachineID()
		if err != nil {
			return fmt.Errorf("failed to get MachineID: %w", err)
		}
		// Read before the configuration, so a temporary mode set or ended after either read fails the write below
		snapshot, eerr := service.ReadTemporaryClientModeSnapshot(machineID)
		if eerr != nil {
			return eerr
		}
		temporaryModeSnapshot = &snapshot
		current, eerr := service.GetIntendedConfig(machineID)
		if eerr != nil {
			return fmt.Errorf("failed to read the current configuration of the machine: %w", eerr)
		}
		if !newConfig.CleanSync && current.CleanSync {
			fmt.Println("Warning: the machine has a requested clean sync that has not been performed yet; replacing its configuration without --clean-sync cancels it.")
			fmt.Println()
		}
		// Replacing the configuration is about the machine's usual settings: a temporary client mode still running
		// is kept, so it still ends when it was meant to. An ended one is dropped with the old row.
		if _, active := current.TemporaryClientModeAt(time.Now()); active {
			newConfig.TemporaryClientMode = current.TemporaryClientMode
			newConfig.TemporaryClientModeExpiresAt = current.TemporaryClientModeExpiresAt
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
	printSantaSettings(writer, newConfig, noOrigin)
	if !tf.IsGlobal {
		printTemporaryMode(writer, newConfig, time.Now())
	}
	writer.Flush()
	if newConfig.TemporaryClientMode != 0 {
		fmt.Printf("\nThe temporary client mode is kept: ClientMode %s applies once it ends. End it now with config update --machine %s --end-temporary-mode.\n", clientModeName(clientMode), machineID)
	}
	fmt.Println()
	if err := flags.Confirm(yes); err != nil {
		return err
	}

	if tf.IsGlobal {
		err = service.SetGlobalConfig(newConfig, globalConfigOptions(confirmGlobalLockdown)...)
	} else {
		// The prompt may have waited a long time: the row is only replaced while its temporary client mode is still the
		// one read above, so one ended or set meanwhile is neither brought back nor lost
		err = service.SetMachineConfigIfTemporaryClientMode(machineID, newConfig, *temporaryModeSnapshot)
		if errors.Is(err, machineconfiguration.ErrTemporaryClientModeChanged) {
			return fmt.Errorf("%w; run the command again", err)
		}
	}

	if err != nil {
		return wrapWriteError(err)
	} else {
		fmt.Printf("Replaced the configuration of %s\n", configTarget(tf, machineID))
	}
	return

}
