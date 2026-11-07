package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
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
		clientModeArg         optionalClientMode
		pathRegexArgs         pathRegexFlags
		santaSettingsArgs     santaSettingsFlags
		confirmGlobalLockdown bool
		cleanSyncArg          bool
		inheritArgs           []string
		temporaryModeArgs     temporaryModeFlags
		baseSettingsArgs      baseSettingsFlags
	)

	tf := flags.TargetFlags{}

	var configUpdateClientModeCmd = &cobra.Command{
		Use:   "update [-m <machine-id>|--global] [-c <client-mode>] [<setting flags>] [--clean-sync] [--inherit <setting>]... [-c <mode> (--for <duration>|--until <time>)] [--end-temporary-mode]",
		Short: "Update settings globally or for a specific machine UUID, keeping all settings that are not given",
		Long: `Update settings globally or for a specific machine UUID, keeping all settings that are not given.

A machine-specific configuration overrides only the settings given to it; every other setting follows the global
configuration, including later global changes. Giving a setting that equals the current global value still makes it a
setting of the machine, which keeps it when the global configuration changes (e.g. to exempt a machine from a later
global lockdown).
Pass --allowed-paths "" or --blocked-paths "" to remove a path regex.

--clean-sync requests one clean sync of a machine and changes nothing else. --inherit <setting> removes a setting
from a machine-specific configuration, so that the machine follows the global value of it again.

-c <mode> with --for <duration> or --until <RFC 3339 time> sets a temporary client mode for a machine, for at most 30
days: the Mac gets it at its next sync, and its usual client mode (its own or the global one, which this leaves as it
is) again at its first sync after the end. --end-temporary-mode ends it early. A temporary lockdown or standalone mode
asks for confirmation; Santa blocks unknown binaries in them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireAtMostOneTarget(); err != nil {
				return err
			}
			// Validate the flags before connecting to the sync server
			var updateRequest machineconfiguration.MachineConfigurationUpdateRequest
			if clientModeArg.set && !temporaryModeArgs.given(cmd) {
				clientMode := clientModeArg.mode.AsClientMode()
				updateRequest.ClientMode = &clientMode
			}
			if cmd.Flags().Changed(flagBlockedPaths) {
				blockedPathRegex, err := pathRegexArgs.blockedPathRegex()
				if err != nil {
					return err
				}
				updateRequest.BlockedPathRegex = &blockedPathRegex
			}
			if cmd.Flags().Changed(flagAllowedPaths) {
				allowedPathRegex, err := pathRegexArgs.allowedPathRegex()
				if err != nil {
					return err
				}
				updateRequest.AllowedPathRegex = &allowedPathRegex
			}
			err := santaSettingsArgs.updateRequest(cmd, &updateRequest)
			if err != nil {
				return err
			}
			baseSettingsArgs.updateRequest(cmd, &updateRequest)
			if cmd.Flags().Changed(flagCleanSync) {
				updateRequest.CleanSync = &cleanSyncArg
			}
			updateRequest.Inherit, err = inheritedSettings(inheritArgs)
			if err != nil {
				return err
			}
			var temporaryMode *temporaryModeRequest
			if temporaryModeArgs.given(cmd) {
				request, err := temporaryModeArgs.request(cmd, tf, &clientModeArg, !updateRequest.IsEmpty(), time.Now())
				if err != nil {
					return err
				}
				temporaryMode = &request
			} else if updateRequest.IsEmpty() {
				return errors.New("no settings to update were given")
			}
			// Refuse invalid values before connecting to the sync server; the service enforces this as well
			if err := machineconfiguration.ValidateUpdateRequest(updateRequest, tf.IsGlobal); err != nil {
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

			if temporaryMode != nil {
				return applyTemporaryMode(dynamodbClient, service, tf, *temporaryMode, timeProvider.Now(), flags.ApprovalOf(cmd))
			}

			return updateConfig(
				service,
				tf,
				updateRequest,
				confirmGlobalLockdown,
				flags.ApprovalOf(cmd),
			)
		},
	}

	tf.AddTargetFlags(configUpdateClientModeCmd)

	// client-mode should be one of "monitor", "lockdown" or "standalone"; it is only changed when given
	configUpdateClientModeCmd.Flags().VarP(&clientModeArg, "client-mode", "c", `type of client mode being applied. valid options are: "monitor", "lockdown" or "standalone"`)
	addConfirmGlobalLockdownFlag(configUpdateClientModeCmd, &confirmGlobalLockdown)
	temporaryModeArgs.addFlags(configUpdateClientModeCmd)

	// Flags defining blocked and allowed regex paths
	pathRegexArgs.addFlags(configUpdateClientModeCmd)

	// Flags for the settings that config set also sets, and for optional Santa settings
	baseSettingsArgs.addFlags(configUpdateClientModeCmd)
	santaSettingsArgs.addFlags(configUpdateClientModeCmd)

	configUpdateClientModeCmd.Flags().BoolVar(&cleanSyncArg, flagCleanSync, false, "Makes the next sync of the machine a clean sync, once, and changes no other setting. Only for a machine (--machine); refused with --global. --clean-sync=false cancels a requested clean sync (with --global, it removes a flag stored by an earlier version, which is ignored)")
	configUpdateClientModeCmd.Flags().StringArrayVar(&inheritArgs, flagInherit, nil, "Removes a setting from the machine-specific configuration, so that the machine follows the global value of it again. Repeat for several settings. Only for a machine (--machine). Settings: "+strings.Join(inheritableSettingFlags(), ", "))

	ConfigCmd.AddCommand(configUpdateClientModeCmd)
}

func updateConfig(
	service machineconfiguration.MachineConfigurationService,
	tf flags.TargetFlags,
	updateRequest machineconfiguration.MachineConfigurationUpdateRequest,
	confirmGlobalLockdown bool,
	approval flags.Approval) (err error) {
	// Refuse invalid values before asking for confirmation; the service enforces this as well
	if err = machineconfiguration.ValidateUpdateRequest(updateRequest, tf.IsGlobal); err != nil {
		return err
	}

	// Get machineID from flags
	var machineID string
	if tf.IsGlobal {
		// Fail before asking for confirmation; the service enforces this as well
		if updateRequest.ClientMode != nil && machineconfiguration.RequiresGlobalLockdownConfirmation(*updateRequest.ClientMode) && !confirmGlobalLockdown {
			return wrapWriteError(machineconfiguration.ErrGlobalLockdownNotConfirmed)
		}
		machineID = "(Global)"
	} else {
		machineID, err = tf.GetMachineID()
		if err != nil {
			return fmt.Errorf("failed to get MachineID: %w", err)
		}
	}

	// While a temporary client mode runs, a change of the machine's own mode only shows once it ends
	var runningTemporaryMode string
	if !tf.IsGlobal && (updateRequest.ClientMode != nil || slices.Contains(updateRequest.Inherit, "ClientMode")) {
		current, eerr := service.GetIntendedConfig(machineID)
		if eerr != nil {
			return fmt.Errorf("failed to read the current configuration of the machine: %w", eerr)
		}
		if mode, active := current.TemporaryClientModeAt(time.Now()); active {
			runningTemporaryMode = fmt.Sprintf("\nWarning: the machine is in temporary %s mode until %s. Its new client mode applies only after that; end the temporary mode now with --%s.\n", clientModeName(mode), formatTemporaryModeEnd(current.TemporaryClientModeEnd()), flagEndTemporaryMode)
		}
	}

	// What the machine inherits is shown as it is set directly, so a global lockdown is as visible as when it is set
	var global machineconfiguration.MachineConfiguration
	if len(updateRequest.Inherit) > 0 {
		global, _, err = service.GetIntendedGlobalConfig()
		if err != nil {
			return fmt.Errorf("failed to read the global configuration: %w", err)
		}
	}

	suffix := ""
	if !(tf.IsGlobal) && tf.IsTargetSelf() {
		suffix = "-->( This machine )"
	}

	// Print the output for visual confirmation via a nicely tab corrected output
	writer := tabwriter.NewWriter(os.Stdout, 0, 8, 0, '\t', tabwriter.AlignRight)

	fmt.Println("Setting the following configuration")
	fmt.Println()
	fmt.Fprintln(writer, "Config\t Setting")
	fmt.Fprintln(writer, "MachineID:\t", machineID, suffix)
	if updateRequest.ClientMode != nil {
		clientMode := *updateRequest.ClientMode
		clientModeText, eerr := clientMode.MarshalText()
		if eerr != nil {
			return eerr
		}
		fmt.Fprintln(writer, "ClientMode:\t", clientMode, "-->(", string(clientModeText), ")")
	}
	if updateRequest.BlockedPathRegex != nil {
		fmt.Fprintln(writer, "BlockedPathRegex:\t \"", *updateRequest.BlockedPathRegex, "\"")
	}
	if updateRequest.AllowedPathRegex != nil {
		fmt.Fprintln(writer, "AllowedPathRegex:\t \"", *updateRequest.AllowedPathRegex, "\"")
	}
	if updateRequest.BatchSize != nil {
		fmt.Fprintln(writer, "BatchSize:\t", *updateRequest.BatchSize)
	}
	if updateRequest.EnableBundles != nil {
		fmt.Fprintln(writer, "BundlesEnabled:\t", *updateRequest.EnableBundles)
	}
	if updateRequest.EnableTransitiveRules != nil {
		fmt.Fprintln(writer, "EnabledTransitiveRules:\t", *updateRequest.EnableTransitiveRules)
	}
	if updateRequest.FullSyncInterval != nil {
		fmt.Fprintln(writer, "FullSyncInterval:\t", *updateRequest.FullSyncInterval)
	}
	if updateRequest.UploadLogsURL != nil {
		fmt.Fprintln(writer, "UploadLogUrl:\t \"", *updateRequest.UploadLogsURL, "\"")
	}
	if updateRequest.EnableAllEventUpload != nil {
		fmt.Fprintln(writer, "EnableAllEventUpload:\t", *updateRequest.EnableAllEventUpload)
	}
	if updateRequest.DisableUnknownEventUpload != nil {
		fmt.Fprintln(writer, "DisableUnknownEventUpload:\t", *updateRequest.DisableUnknownEventUpload)
	}
	if updateRequest.OverrideFileAccessAction != nil {
		fmt.Fprintln(writer, "OverrideFileAccessAction:\t", *updateRequest.OverrideFileAccessAction)
	}
	if updateRequest.BlockUsbMount != nil {
		fmt.Fprintln(writer, "BlockUsbMount:\t", *updateRequest.BlockUsbMount)
	}
	if updateRequest.RemountUsbMode != nil {
		fmt.Fprintln(writer, "RemountUsbMode:\t", strings.Join(*updateRequest.RemountUsbMode, ","))
	}
	if updateRequest.EventDetailURL != nil {
		fmt.Fprintln(writer, "EventDetailURL:\t \"", *updateRequest.EventDetailURL, "\"")
	}
	if updateRequest.EventDetailText != nil {
		fmt.Fprintln(writer, "EventDetailText:\t \"", *updateRequest.EventDetailText, "\"")
	}
	if updateRequest.CleanSync != nil {
		fmt.Fprintln(writer, "CleanSync:\t", *updateRequest.CleanSync)
	}
	for _, setting := range updateRequest.Inherit {
		fmt.Fprintln(writer, setting+":\t", inheritedSettingText(global, setting), "(inherited from the global configuration)")
	}
	writer.Flush()
	for _, setting := range updateRequest.Inherit {
		if setting == "ClientMode" && machineconfiguration.RequiresGlobalLockdownConfirmation(global.ClientMode) {
			clientModeText, _ := global.ClientMode.MarshalText()
			fmt.Printf("\nWarning: the global client mode is %s, which blocks unknown executions; the machine follows it from its next sync.\n", clientModeText)
		}
	}
	fmt.Print(runningTemporaryMode)
	fmt.Println()
	if apply, err := approval.Confirm(); !apply {
		return err
	}

	if tf.IsGlobal {
		_, err = service.UpdateGlobalConfig(updateRequest, globalConfigOptions(confirmGlobalLockdown)...)
	} else {
		_, err = service.UpdateMachineConfig(machineID, updateRequest)
	}

	if err != nil {
		return wrapWriteError(err)
	} else {
		fmt.Printf("Updated the configuration of %s\n", configTarget(tf, machineID))
	}
	return

}
