package flags

import (
	"errors"

	"github.com/airbnb/rudolph/internal/cli/santa_sensor"
	"github.com/spf13/cobra"
)

type TargetFlags struct {
	MachineID     string
	IsGlobal      bool
	SelfMachineID string
	// Group is the device group a rule targets (--group, or groups.Ungrouped for --ungrouped), normalised by
	// RequireExplicitTarget; only commands that add the rule target flags set it
	Group     string
	Ungrouped bool
}

// IsGroup reports whether the target is a device group (--group or --ungrouped)
func (t TargetFlags) IsGroup() bool {
	return t.Group != "" || t.Ungrouped
}

// AddTargetFlags adds --global and --machine to a command that reads or writes a configuration. Without either, the
// command applies to this Mac; call RequireAtMostOneTarget before using them.
func (t *TargetFlags) AddTargetFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&t.IsGlobal, "global", "g", false, "Apply to the global configuration. Pass at most one of --global and --machine; without either, the command applies to this Mac.")
	cmd.Flags().StringVarP(&t.MachineID, "machine", "m", "", "Apply to the machine with this UUID. Pass at most one of --global and --machine; without either, the command applies to this Mac.")
}

// AddTargetFlagsRules adds --global and --machine to the command that lists rules; call RequireAtMostOneTarget
// before using them
func (t *TargetFlags) AddTargetFlagsRules(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&t.IsGlobal, "global", "g", false, "List the global rules. Pass at most one of --global and --machine; without either, the rules of this Mac are listed.")
	cmd.Flags().StringVarP(&t.MachineID, "machine", "m", "", "List the rules of the machine with this UUID. Pass at most one of --global and --machine; without either, the rules of this Mac are listed.")
}

// RequireAtMostOneTarget refuses --global together with --machine
func (t *TargetFlags) RequireAtMostOneTarget() error {
	if t.IsGlobal && t.MachineID != "" {
		return errors.New("pass only one of --global and --machine <machine-id>")
	}
	if t.MachineID != "" {
		return ValidateMachineIDFlag(t.MachineID)
	}
	return nil
}

// GetMachineID returns the machine ID given with --machine, or else this Mac's own machine ID. Call ResolveMachineID
// first, so that a machine ID typed in another case than the one it is stored under still addresses that machine.
// It also records this Mac's machine ID, when it can be read, for IsTargetSelf.
func (t *TargetFlags) GetMachineID() (string, error) {
	if t.IsGlobal {
		return "", errors.New("do not call GetMachineID when IsGlobal is true")
	}
	if t.MachineID != "" {
		if err := ValidateMachineIDFlag(t.MachineID); err != nil {
			return "", err
		}
		// Only to mark "(This machine)": a Mac without Santa can still address other machines
		_, _ = t.getSelfMachineID()
		return t.MachineID, nil
	}
	return t.getSelfMachineID()
}

// IsTargetSelf reports whether the target is this Mac. It is only known after GetMachineID.
func (t TargetFlags) IsTargetSelf() bool {
	return t.SelfMachineID != "" && (t.MachineID == "" || t.SelfMachineID == t.MachineID)
}

func (t *TargetFlags) getSelfMachineID() (string, error) {
	if t.SelfMachineID != "" {
		return t.SelfMachineID, nil
	}

	selfMachineId, err := santa_sensor.GetSelfMachineID()
	if err != nil {
		return "", err
	}
	t.SelfMachineID = selfMachineId
	return t.SelfMachineID, nil
}
