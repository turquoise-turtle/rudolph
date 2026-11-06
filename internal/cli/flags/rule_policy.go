package flags

import (
	"errors"
	"fmt"

	"strings"

	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

const (
	allowlist               = "allowlist"
	allowlistShort          = "allow"
	blocklist               = "blocklist"
	blocklistShort          = "block"
	silentBlocklist         = "silent_blocklist"
	silentBlocklistShort    = "silent_block"
	silentBlocklistShortAlt = "silent-block"
	silentGUIBlocklist      = "silent_gui_blocklist"
	silentGUIBlocklistShort = "silent_gui_block"
	silentGUIBlocklistAlt   = "silent-gui-block"
	silentTTYBlocklist      = "silent_tty_blocklist"
	silentTTYBlocklistShort = "silent_tty_block"
	silentTTYBlocklistAlt   = "silent-tty-block"
	cel                     = "cel"
	remove                  = "remove"
	removeShort             = "delete"
)

const (
	rulePolicyFlag           = "rule-policy"
	collectAllowedEventsFlag = "collect-allowed-events"
)

// RuleUpdateFlags are the changes `rule update` can make to an existing rule
type RuleUpdateFlags struct {
	RulePolicy           *RulePolicy
	CollectAllowedEvents *bool
	Expiry               RuleExpiryFlags
	Options              RuleOptionFlags

	cmd *cobra.Command
}

func (r *RuleUpdateFlags) AddRuleUpdateFlags(cmd *cobra.Command) {
	var rulePolicyArg RulePolicy
	cmd.Flags().VarP(&rulePolicyArg, rulePolicyFlag, "p", `The new policy of the rule: "allowlist", "blocklist", "silent_blocklist", "silent_gui_blocklist" or "silent_tty_blocklist". Without it the policy is kept`)

	r.CollectAllowedEvents = cmd.Flags().Bool(collectAllowedEventsFlag, false, "store the executions this rule allows in the events table read by the web view (true or false); Santa uploads them only with EnableAllEventUpload")
	r.Expiry.AddRuleExpiryFlags(cmd)
	r.Options.AddRuleOptionFlags(cmd)

	r.RulePolicy = &rulePolicyArg
	r.cmd = cmd
}

// PolicyChanged reports whether --rule-policy was given.
func (r *RuleUpdateFlags) PolicyChanged() bool {
	return r.changed(rulePolicyFlag)
}

// CollectAllowedEventsChanged reports whether --collect-allowed-events was given.
func (r *RuleUpdateFlags) CollectAllowedEventsChanged() bool {
	return r.changed(collectAllowedEventsFlag)
}

// ExpiryChanged reports whether --expires or --permanent was given
func (r *RuleUpdateFlags) ExpiryChanged() bool {
	return r.changed("expires") || r.changed("permanent")
}

// OptionsChanged reports whether any of the message and URL flags was given (an empty value clears the field)
func (r *RuleUpdateFlags) OptionsChanged() bool {
	return r.changed(customMsgFlag) || r.changed(customURLFlag) || r.changed(eventDetailButtonLabelFlag) || r.changed(notificationAppNameFlag)
}

// ApplyOptions sets the fields of rule whose message and URL flags were given
func (r *RuleUpdateFlags) ApplyOptions(rule *rules.SantaRule) {
	if r.changed(customMsgFlag) {
		rule.CustomMessage = *r.Options.CustomMessage
	}
	if r.changed(customURLFlag) {
		rule.CustomURL = *r.Options.CustomURL
	}
	if r.changed(eventDetailButtonLabelFlag) {
		rule.EventDetailButtonLabel = *r.Options.EventDetailButtonLabel
	}
	if r.changed(notificationAppNameFlag) {
		rule.NotificationAppName = *r.Options.NotificationAppName
	}
}

func (r *RuleUpdateFlags) changed(flag string) bool {
	return r.cmd != nil && r.cmd.Flags().Changed(flag)
}

// RequireChange returns an error unless something to change is given, and refuses the policies an update cannot set
func (r *RuleUpdateFlags) RequireChange() error {
	if !r.PolicyChanged() && !r.CollectAllowedEventsChanged() && !r.ExpiryChanged() && !r.OptionsChanged() {
		return fmt.Errorf("nothing to change: pass the new policy with --%s (-p), --%s=true|false, --expires or --permanent, or a message or URL flag", rulePolicyFlag, collectAllowedEventsFlag)
	}
	if r.PolicyChanged() {
		switch r.RulePolicy.AsRulePolicy() {
		case types.RulePolicyRemove:
			// A REMOVE policy on a rule row is sent to Santa but never deletes the row
			return errors.New(`a rule is not removed by updating its policy; use "rudolph rule remove"`)
		case types.RulePolicyCEL:
			return errors.New(`a rule cannot be updated to the CEL policy; remove it and create it with "rule cel"`)
		}
	}
	return nil
}

// rulePolicy is a custom type for use as a CLI flag representing the type of rule policy being applied
type RulePolicy types.Policy

func (i *RulePolicy) AsRulePolicy() types.Policy {
	return types.Policy(*i)
}

func (i *RulePolicy) Set(s string) error {
	s = strings.ToLower(s)
	switch s {
	case allowlist, allowlistShort:
		*i = RulePolicy(types.Allowlist)
	case blocklist, blocklistShort:
		*i = RulePolicy(types.Blocklist)
	case silentBlocklist, silentBlocklistShort, silentBlocklistShortAlt:
		*i = RulePolicy(types.SilentBlocklist)
	case silentGUIBlocklist, silentGUIBlocklistShort, silentGUIBlocklistAlt:
		*i = RulePolicy(types.RulePolicySilentGUIBlocklist)
	case silentTTYBlocklist, silentTTYBlocklistShort, silentTTYBlocklistAlt:
		*i = RulePolicy(types.RulePolicySilentTTYBlocklist)
	case cel:
		*i = RulePolicy(types.RulePolicyCEL)
	case remove, removeShort:
		*i = RulePolicy(types.Remove)
	default:
		return fmt.Errorf(`invalid rule policy; must be "allowlist", "blocklist", "silent_blocklist", "silent_gui_blocklist", "silent_tty_blocklist", "cel" or "remove"`)
	}
	return nil
}

func (i *RulePolicy) Type() string {
	return "string"
}

func (i *RulePolicy) String() string {
	v := (types.Policy)(*i)
	switch v {
	case types.Allowlist:
		return allowlist
	case types.Blocklist:
		return blocklist
	case types.SilentBlocklist:
		return silentBlocklist
	case types.RulePolicySilentGUIBlocklist:
		return silentGUIBlocklist
	case types.RulePolicySilentTTYBlocklist:
		return silentTTYBlocklist
	case types.RulePolicyCEL:
		return cel
	case types.Remove:
		return remove
	}

	// No default
	return ""
}
