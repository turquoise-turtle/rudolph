package flags

import (
	"errors"

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

	cmd.Flags().StringVarP(&filepathArg, "filepath", "f", "", `The path of a binary or application bundle; santactl fileinfo reads the rule's identifier from it. Pass exactly one of --filepath and --identifier`)
	cmd.Flags().StringVarP(&identifierArg, "identifier", "i", "", `The rule's identifier: a SHA-256 (binary, certificate), Team ID, signing ID or CDHash. Pass exactly one of --filepath and --identifier`)

	cmd.Flags().VarP(&ruleTypeArg, "rule-type", "t", `The type of the rule: "binary" ("bin"), "certificate" ("cert"), "teamid", "signingid" or "cdhash"`)
	_ = cmd.MarkFlagRequired("rule-type")

	r.RuleType = &ruleTypeArg
	r.Identifier = &identifierArg
	r.FilePath = &filepathArg
}

// RequireOne returns an error unless exactly one of --filepath and --identifier was given
func (r *RuleInfoFlags) RequireOne() error {
	switch {
	case *r.FilePath != "" && *r.Identifier != "":
		return errors.New("pass either --filepath or --identifier, not both")
	case *r.FilePath == "" && *r.Identifier == "":
		return errors.New("pass the rule's --identifier, or --filepath to read it from a file")
	}
	return nil
}

const (
	customMsgFlag              = "custom-msg"
	customURLFlag              = "custom-url"
	eventDetailButtonLabelFlag = "event-detail-button-label"
	notificationAppNameFlag    = "notification-app-name"
)

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

	cmd.Flags().StringVar(&customMessageArg, customMsgFlag, "", `For block rules, the message shown to the user instead of the default block message`)
	cmd.Flags().StringVar(&customURLArg, customURLFlag, "", `For block rules, the URL opened from the block dialog instead of EventDetailURL (same format values)`)
	cmd.Flags().StringVar(&eventDetailButtonLabelArg, eventDetailButtonLabelFlag, "", `For block rules, the label of the block dialog's button instead of EventDetailText`)
	cmd.Flags().StringVar(&notificationAppNameArg, notificationAppNameFlag, "", `When set, Santa notifies the user that this application is now allowed (ignored during clean syncs)`)

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
