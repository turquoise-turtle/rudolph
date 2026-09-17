package flags

import (
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/spf13/cobra"
)

type RuleInfoFlags struct {
	RuleType   *RuleType
	Identifier *string
	FilePath   *string
}

func (r *RuleInfoFlags) AddRuleInfoFlags(cmd *cobra.Command) {
	var (
		ruleTypeArg   RuleType
		identifierArg string
		filepathArg   string
	)

	// Flag specifying the binary
	cmd.Flags().StringVarP(&filepathArg, "filepath", "f", "", `The filepath of a binary/application. Provide exactly one of [--filepath|--sha]`)
	cmd.Flags().StringVarP(&identifierArg, "identifier", "i", "", `The Identifier/SHA256 for a file, application, teamID, signingID or CDHash`)

	// rule-type should be one of "binary" or "cert" ("bin" and "certificate" also work)
	cmd.Flags().VarP(&ruleTypeArg, "rule-type", "t", `type of rule being applied. valid options are: "binary", "bin", "certificate", "cert", "teamid", "signingid", "cdhash"`)
	_ = cmd.MarkFlagRequired("rule-type")

	// If we want to make the `rule-type` flag optional with a default (say "binary"),
	// we can remove the previous 2 lines and instead use the following
	// cmd.Flags().VarP(newRuleTypeValue("binary", &ruleTypeArg), "rule-type", "t", `type of rule being applied. valid options are: "binary", "bin", "certificate", or "cert"`)

	// rule-policy is to specify the policy for edit commands

	r.RuleType = &ruleTypeArg
	r.Identifier = &identifierArg
	r.FilePath = &filepathArg
}

// RuleOptionFlags are the optional per-rule fields that are sent to Santa along with a new rule
type RuleOptionFlags struct {
	CustomMessage          *string
	CustomURL              *string
	EventDetailButtonLabel *string
	NotificationAppName    *string
}

func (r *RuleOptionFlags) AddRuleOptionFlags(cmd *cobra.Command) {
	var (
		customMessageArg          string
		customURLArg              string
		eventDetailButtonLabelArg string
		notificationAppNameArg    string
	)

	cmd.Flags().StringVar(&customMessageArg, "custom-msg", "", `For block rules, the message shown to the user instead of the default block message`)
	cmd.Flags().StringVar(&customURLArg, "custom-url", "", `For block rules, the URL opened from the block dialog instead of EventDetailURL (same format values)`)
	cmd.Flags().StringVar(&eventDetailButtonLabelArg, "event-detail-button-label", "", `For block rules, the label of the block dialog's button instead of EventDetailText`)
	cmd.Flags().StringVar(&notificationAppNameArg, "notification-app-name", "", `When set, Santa notifies the user that this application is now allowed (ignored during clean syncs)`)

	r.CustomMessage = &customMessageArg
	r.CustomURL = &customURLArg
	r.EventDetailButtonLabel = &eventDetailButtonLabelArg
	r.NotificationAppName = &notificationAppNameArg
}

// Options returns the rule options for every flag that was set
func (r *RuleOptionFlags) Options() (options []rules.RuleOption) {
	if r.CustomMessage != nil && *r.CustomMessage != "" {
		options = append(options, rules.WithCustomMessage(*r.CustomMessage))
	}
	if r.CustomURL != nil && *r.CustomURL != "" {
		options = append(options, rules.WithCustomURL(*r.CustomURL))
	}
	if r.EventDetailButtonLabel != nil && *r.EventDetailButtonLabel != "" {
		options = append(options, rules.WithEventDetailButtonLabel(*r.EventDetailButtonLabel))
	}
	if r.NotificationAppName != nil && *r.NotificationAppName != "" {
		options = append(options, rules.WithNotificationAppName(*r.NotificationAppName))
	}
	return
}
