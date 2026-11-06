package config

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

const (
	flagFor              = "for"
	flagUntil            = "until"
	flagEndTemporaryMode = "end-temporary-mode"
)

// temporaryModeFlags are the flags of `config update` that set or end a temporary client mode of a machine
type temporaryModeFlags struct {
	duration time.Duration
	until    string
	end      bool
}

func (f *temporaryModeFlags) addFlags(cmd *cobra.Command) {
	cmd.Flags().DurationVar(&f.duration, flagFor, 0, "With -c, sets the client mode only for this long (e.g. 2h, 90m; at most 30 days), leaving the machine's own client mode as it is. Only for a machine (--machine)")
	cmd.Flags().StringVar(&f.until, flagUntil, "", "With -c, sets the client mode only until this time (RFC 3339, e.g. 2026-11-07T17:00:00+11:00; at most 30 days ahead), leaving the machine's own client mode as it is. Only for a machine (--machine)")
	cmd.Flags().BoolVar(&f.end, flagEndTemporaryMode, false, "Ends the temporary client mode of a machine now; it returns to its usual client mode at its next sync")
}

// given reports whether any temporary mode flag was given
func (f *temporaryModeFlags) given(cmd *cobra.Command) bool {
	return cmd.Flags().Changed(flagFor) || cmd.Flags().Changed(flagUntil) || cmd.Flags().Changed(flagEndTemporaryMode)
}

// temporaryModeRequest is a validated request to set (mode and expires) or end a temporary client mode
type temporaryModeRequest struct {
	end     bool
	mode    types.ClientMode
	expires time.Time
}

// request validates the temporary mode flags against the rest of the update. A temporary mode is a request of its
// own: it cannot be combined with other settings, so a refused or declined part never leaves the other half written.
func (f *temporaryModeFlags) request(cmd *cobra.Command, tf flags.TargetFlags, clientMode *optionalClientMode, otherSettings bool, now time.Time) (temporaryModeRequest, error) {
	changed := cmd.Flags().Changed
	if tf.IsGlobal {
		return temporaryModeRequest{}, errors.New("a temporary client mode is only for a machine (--machine), not for the global configuration")
	}
	if otherSettings {
		return temporaryModeRequest{}, fmt.Errorf("--%s, --%s and --%s cannot be combined with other settings; run a separate config update for them", flagFor, flagUntil, flagEndTemporaryMode)
	}
	if changed(flagEndTemporaryMode) {
		if changed(flagFor) || changed(flagUntil) || clientMode.set {
			return temporaryModeRequest{}, fmt.Errorf("--%s takes no client mode, --%s or --%s", flagEndTemporaryMode, flagFor, flagUntil)
		}
		if !f.end {
			return temporaryModeRequest{}, fmt.Errorf("--%s=false does nothing", flagEndTemporaryMode)
		}
		return temporaryModeRequest{end: true}, nil
	}
	if changed(flagFor) && changed(flagUntil) {
		return temporaryModeRequest{}, fmt.Errorf("give either --%s or --%s, not both", flagFor, flagUntil)
	}
	if !clientMode.set {
		return temporaryModeRequest{}, fmt.Errorf("--%s and --%s need the temporary client mode (-c monitor, lockdown or standalone)", flagFor, flagUntil)
	}
	var expires time.Time
	if changed(flagFor) {
		if f.duration <= 0 {
			return temporaryModeRequest{}, fmt.Errorf("--%s must be a positive duration, e.g. 2h", flagFor)
		}
		expires = now.Add(f.duration)
	} else {
		until, err := time.Parse(time.RFC3339, f.until)
		if err != nil {
			return temporaryModeRequest{}, fmt.Errorf("--%s must be an RFC 3339 time, e.g. 2026-11-07T17:00:00+11:00: %w", flagUntil, err)
		}
		expires = until
	}
	mode := clientMode.mode.AsClientMode()
	if err := machineconfiguration.ValidateTemporaryClientMode(mode, expires, now); err != nil {
		return temporaryModeRequest{}, err
	}
	return temporaryModeRequest{mode: mode, expires: expires}, nil
}

// applyTemporaryMode sets or ends the temporary client mode of a machine. A temporary mode that blocks unknown
// executions asks for the same confirmation as setting that mode on the machine; declining it is an error, so a
// script does not take it for done. A temporary monitor mode and ending one are applied without asking. A dry run
// writes neither.
func applyTemporaryMode(store machineconfiguration.TemporaryClientModeStore, service machineconfiguration.MachineConfigurationService, tf flags.TargetFlags, request temporaryModeRequest, now time.Time, approval flags.Approval) error {
	machineID, err := tf.GetMachineID()
	if err != nil {
		return fmt.Errorf("failed to get MachineID: %w", err)
	}
	if request.end {
		return endTemporaryMode(store, service, machineID, approval)
	}

	modeText := clientModeName(request.mode)
	fmt.Printf("Temporary client mode for machine %s: %s until %s.\n", machineID, modeText, formatTemporaryModeEnd(request.expires))
	fmt.Println("The machine's own client mode is left as it is. The Mac switches at its next sync and returns to its usual mode at its first sync after the end (each up to its sync interval later).")
	if machineconfiguration.RequiresGlobalLockdownConfirmation(request.mode) {
		fmt.Printf("Warning: in %s mode Santa blocks unknown binaries on this Mac.\n", modeText)
		fmt.Println()
		apply, err := approval.Confirm()
		if err != nil {
			return fmt.Errorf("temporary %s mode %w", modeText, err)
		}
		if !apply {
			return nil
		}
	} else if !approval.Write() {
		return nil
	}
	if err := machineconfiguration.SetTemporaryClientMode(store, machineID, request.mode, request.expires, now); err != nil {
		return wrapWriteError(err)
	}
	fmt.Println("Success! The temporary client mode was sent to DynamoDB...")
	return nil
}

func endTemporaryMode(store machineconfiguration.TemporaryClientModeStore, service machineconfiguration.MachineConfigurationService, machineID string, approval flags.Approval) error {
	if approval.DryRun {
		snapshot, err := machineconfiguration.ReadTemporaryClientModeSnapshot(store, machineID)
		if err != nil {
			return err
		}
		if snapshot.Empty() {
			fmt.Printf("Machine %s has no temporary client mode; nothing would change.\n", machineID)
			return nil
		}
		fmt.Printf("Ending the temporary client mode of machine %s.\n", machineID)
		approval.Write()
		return nil
	}
	err := machineconfiguration.EndTemporaryClientMode(store, machineID)
	if errors.Is(err, machineconfiguration.ErrNoTemporaryClientMode) {
		fmt.Printf("Machine %s has no temporary client mode; nothing was changed.\n", machineID)
		return nil
	}
	if err != nil {
		return wrapWriteError(err)
	}
	fmt.Printf("Ended the temporary client mode of machine %s.\n", machineID)
	config, err := service.GetIntendedConfig(machineID)
	if err != nil {
		return fmt.Errorf("failed to read the machine's usual client mode: %w", err)
	}
	fmt.Printf("It returns to its usual client mode, %s, at its next sync.\n", clientModeName(config.ClientMode))
	return nil
}

// printTemporaryMode prints the temporary client mode of a machine config, if it has one
func printTemporaryMode(writer io.Writer, config machineconfiguration.MachineConfiguration, now time.Time) {
	if config.TemporaryClientMode == 0 && config.TemporaryClientModeExpiresAt == 0 {
		return
	}
	mode := clientModeName(config.TemporaryClientMode)
	switch _, active := config.TemporaryClientModeAt(now); {
	case active:
		fmt.Fprintf(writer, "TemporaryClientMode:\t %s until %s (sent to Santa instead of ClientMode)\n", mode, formatTemporaryModeEnd(config.TemporaryClientModeEnd()))
	case config.TemporaryClientModeExpiresAt <= 0:
		fmt.Fprintf(writer, "TemporaryClientMode:\t %s without an end (ignored)\n", mode)
	default:
		fmt.Fprintf(writer, "TemporaryClientMode:\t %s, ended %s (ignored)\n", mode, formatTemporaryModeEnd(config.TemporaryClientModeEnd()))
	}
}

// formatTemporaryModeEnd shows the end of a temporary mode in local time and in UTC
func formatTemporaryModeEnd(end time.Time) string {
	return fmt.Sprintf("%s (%s)", end.Local().Format("2006-01-02 15:04:05 MST"), end.UTC().Format(time.RFC3339))
}

// clientModeName names a client mode, or shows the number of one that is not known
func clientModeName(mode types.ClientMode) string {
	text, err := mode.MarshalText()
	if err != nil {
		return fmt.Sprintf("%d (INVALID)", mode)
	}
	return string(text)
}
