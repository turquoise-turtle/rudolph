package flags

import (
	"errors"
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/internal/cli/santa_sensor"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

type RuleTargetFlags struct {
	RuleTypeArg  types.RuleType
	IsGlobal     bool
	MachineIdArg string
	SHA256Arg    string
	FilepathArg  string
}

type TargetFlags struct {
	MachineID     string
	IsGlobal      bool
	SelfMachineID string
}

func (t *TargetFlags) AddTargetFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&t.IsGlobal, "global", "g", false, "Apply globally. Provide one of [--global|--machine], or exclude both to apply to current machine.")
	cmd.Flags().StringVarP(&t.MachineID, "machine", "m", "", `The uuid of the machine.`)
}

func (t *TargetFlags) AddTargetFlagsRules(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&t.IsGlobal, "global", "g", false, "Retrieve rules that apply globally.")
	cmd.Flags().StringVarP(&t.MachineID, "machine", "m", "", "Retrieve rules for a single machine. Omit to apply to the current machine.")

}

// NormaliseMachineID checks that a machine ID given on the command line is a UUID in its canonical 8-4-4-4-12 form
// and returns it upper-cased.
//
// The server stores and matches machine IDs exactly, including case, and Santa sends the Mac's hardware UUID in upper
// case. A machine ID typed in lower case would otherwise address a machine that never syncs, and anything that is not
// a UUID could never be a machine at all, since the API refuses such IDs.
func NormaliseMachineID(machineID string) (string, error) {
	if err := types.ValidateMachineID(machineID); err != nil {
		return "", fmt.Errorf("--machine %q is not a machine UUID (8-4-4-4-12 hexadecimal digits, such as 01234567-89AB-CDEF-0123-456789ABCDEF)", machineID)
	}
	return strings.ToUpper(machineID), nil
}

// GetMachineID returns the machine ID given with --machine, upper-cased (see NormaliseMachineID), or else this Mac's
// own machine ID.
func (t TargetFlags) GetMachineID() (string, error) {
	t.getSelfMachineID() // We do some ninja initialization

	if t.IsGlobal {
		return "", errors.New("do not call GetMachineID when IsGlobal is true")
	} else if t.MachineID != "" {
		return NormaliseMachineID(t.MachineID)
	} else {
		return santa_sensor.GetSelfMachineID()
	}
}

// WARNING: This is best-effort as if you don't call getSelfMachineID first, it'll just always return false.
func (t TargetFlags) IsTargetSelf() bool {
	return t.SelfMachineID == t.MachineID
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
