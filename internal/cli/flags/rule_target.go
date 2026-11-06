package flags

import (
	"errors"
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/spf13/cobra"
)

// AddRuleTargetFlags adds --global, --machine, --group and --ungrouped to a command that writes rules. Unlike
// AddTargetFlags there is no default: a rule written to "this machine" by accident (the operator's own Mac) is easy to
// miss, so the command must call RequireExplicitTarget.
func (t *TargetFlags) AddRuleTargetFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&t.IsGlobal, "global", "g", false, "Apply to every machine. Exactly one of --global, --machine, --group or --ungrouped is required.")
	cmd.Flags().StringVarP(&t.MachineID, "machine", "m", "", "Apply to the machine with this UUID only. Exactly one of --global, --machine, --group or --ungrouped is required.")
	cmd.Flags().StringVar(&t.Group, "group", "", "Apply to the Macs in this device group (see rudolph group). Exactly one of --global, --machine, --group or --ungrouped is required.")
	cmd.Flags().BoolVar(&t.Ungrouped, "ungrouped", false, "Apply to the Macs in no device group. Exactly one of --global, --machine, --group or --ungrouped is required.")
}

// RequireExplicitTarget returns an error unless exactly one of --global, --machine, --group and --ungrouped was given,
// the machine ID is a UUID (see ValidateMachineIDFlag) and the group name is acceptable (see groups.Normalise). It
// normalises the group name, and sets it to groups.Ungrouped for --ungrouped.
func (t *TargetFlags) RequireExplicitTarget() error {
	given := 0
	for _, set := range []bool{t.IsGlobal, t.MachineID != "", t.Group != "", t.Ungrouped} {
		if set {
			given++
		}
	}
	if given > 1 {
		return errors.New("pass only one of --global, --machine <machine-id>, --group <group> or --ungrouped")
	}
	if given == 0 {
		return errors.New("pass --global to apply the rule to every machine, --machine <machine-id> to apply it to one machine, or --group <group> or --ungrouped to apply it to a device group")
	}
	switch {
	case t.MachineID != "":
		return ValidateMachineIDFlag(t.MachineID)
	case t.Ungrouped:
		t.Group = groups.Ungrouped
	case t.Group != "":
		group, err := groups.Normalise(t.Group)
		if err != nil {
			return fmt.Errorf("--group: %w", err)
		}
		t.Group = group
	}
	return nil
}

// RuleExpiryFlags set when a machine rule expires
type RuleExpiryFlags struct {
	Expires   string
	Permanent bool
}

// DefaultMachineRuleExpiry is how long a machine rule lasts when neither --expires nor --permanent is given
const DefaultMachineRuleExpiry = 24 * time.Hour

// AddRuleExpiryFlags adds --expires and --permanent to a command that creates rules
func (e *RuleExpiryFlags) AddRuleExpiryFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&e.Expires, "expires", "", fmt.Sprintf(`Machine rules only: when the rule expires, as a duration from now ("48h", "30m") or an RFC 3339 time ("2026-12-31T18:00:00Z"). Default %s.`, DefaultMachineRuleExpiry))
	cmd.Flags().BoolVar(&e.Permanent, "permanent", false, "Machine rules only: the rule never expires.")
}

// RefuseForGroup returns an error when expiry flags were given for a group rule: group rules never expire
func (e RuleExpiryFlags) RefuseForGroup() error {
	if e.Expires != "" || e.Permanent {
		return errors.New("--expires and --permanent only apply to machine rules; group rules never expire")
	}
	return nil
}

// Expiry returns when a rule created now expires: the zero time for a permanent machine rule and for global rules
// (which never expire). It refuses expiry flags on a global rule, --expires together with --permanent, and an expiry
// that is unparseable or not in the future.
func (e RuleExpiryFlags) Expiry(now time.Time, global bool) (time.Time, error) {
	if global {
		if e.Expires != "" || e.Permanent {
			return time.Time{}, errors.New("--expires and --permanent only apply to machine rules; global rules never expire")
		}
		return time.Time{}, nil
	}
	if e.Permanent {
		if e.Expires != "" {
			return time.Time{}, errors.New("pass either --expires or --permanent, not both")
		}
		return time.Time{}, nil
	}
	if e.Expires == "" {
		return now.Add(DefaultMachineRuleExpiry).UTC(), nil
	}

	var expires time.Time
	if d, err := time.ParseDuration(e.Expires); err == nil {
		expires = now.Add(d)
	} else if t, err := time.Parse(time.RFC3339, e.Expires); err == nil {
		expires = t
	} else {
		return time.Time{}, fmt.Errorf(`--expires %q is neither a duration ("48h") nor an RFC 3339 time ("2026-12-31T18:00:00Z")`, e.Expires)
	}
	if !expires.After(now) {
		return time.Time{}, fmt.Errorf("--expires %q is not in the future", e.Expires)
	}
	return expires.UTC(), nil
}
