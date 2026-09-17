package config

import (
	"bufio"
	"errors"
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
		clientModeArg         flags.ClientMode
		pathRegexArgs         pathRegexFlags
		santaSettingsArgs     santaSettingsFlags
		confirmGlobalLockdown bool
	)

	tf := flags.TargetFlags{}

	var configUpdateClientModeCmd = &cobra.Command{
		Use:   "update [-m <machine-id>|--global] [-c <ClientMode - 'monitor' or 'lockdown'>|--client-mode] [--allowed-paths <regex>]... [--blocked-paths <regex>]...",
		Short: "Update settings globally or for a specific machine UUID, keeping all settings that are not given",
		Long: `Update settings globally or for a specific machine UUID, keeping all settings that are not given.

A machine without a machine-specific configuration gets one, copied from the global configuration.
Pass --allowed-paths "" or --blocked-paths "" to remove a path regex.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate the flags before connecting to the sync server
			var updateRequest machineconfiguration.MachineConfigurationUpdateRequest
			if cmd.Flags().Changed("client-mode") {
				clientMode := clientModeArg.AsClientMode()
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
			if (updateRequest == machineconfiguration.MachineConfigurationUpdateRequest{}) {
				return errors.New("no settings to update were given")
			}

			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			timeProvider := clock.ConcreteTimeProvider{}

			service := machineconfiguration.GetMachineConfigurationService(dynamodbClient, timeProvider)

			return updateConfig(
				service,
				tf,
				updateRequest,
				confirmGlobalLockdown,
			)
		},
	}

	tf.AddTargetFlags(configUpdateClientModeCmd)

	// client-mode should be one of "monitor" or "lockdown"
	configUpdateClientModeCmd.Flags().VarP(&clientModeArg, "client-mode", "c", `type of client mode being applied. valid options are: "monitor" or "lockdown"`)
	addConfirmGlobalLockdownFlag(configUpdateClientModeCmd, &confirmGlobalLockdown)

	// Flags defining blocked and allowed regex paths
	pathRegexArgs.addFlags(configUpdateClientModeCmd)

	// Flags for optional Santa settings
	santaSettingsArgs.addFlags(configUpdateClientModeCmd)

	ConfigCmd.AddCommand(configUpdateClientModeCmd)
}

func updateConfig(
	service machineconfiguration.MachineConfigurationService,
	tf flags.TargetFlags,
	updateRequest machineconfiguration.MachineConfigurationUpdateRequest,
	confirmGlobalLockdown bool) (err error) {
	// Get machineID from flags
	var machineID string
	if tf.IsGlobal {
		// Fail before asking for confirmation; the service enforces this as well
		if updateRequest.ClientMode != nil && *updateRequest.ClientMode == types.Lockdown && !confirmGlobalLockdown {
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
		_, err = service.UpdateGlobalConfig(updateRequest, globalConfigOptions(confirmGlobalLockdown)...)
	} else {
		_, err = service.UpdateMachineConfig(machineID, updateRequest)
	}

	if err != nil {
		return wrapWriteError(err)
	} else {
		fmt.Println("Success! Configuration was sent properly to DynamoDB...")
	}
	return

}
