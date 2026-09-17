package rules

import (
	"github.com/airbnb/rudolph/pkg/types"
)

// SantaRule is the trimmed down concept of a rule
// These fields are commonly shared across different sub-types of rules:
//
// - global rules:   Source of truth for rules that are intended to be distributed to all endpoints
// - feed rules:     A timeline-based feed of rule "diffs" that endpoints can download
// - machine rules:  Rules intended only to be deployed to specific endpoints
type SantaRule struct {
	RuleType      types.RuleType `dynamodbav:"RuleType" json:"rule_type"`
	Policy        types.Policy   `dynamodbav:"Policy" json:"policy"`
	SHA256        string         `dynamodbav:"SHA256,omitempty" json:"sha256,omitempty"` // @deprecated - Use Identifier instead
	Identifier    string         `dynamodbav:"Identifier" json:"identifier"`
	CustomMessage string         `dynamodbav:"CustomMessage,omitempty" json:"custom_msg,omitempty"`
	// CustomURL overrides the URL opened from the block dialog; it supports the EventDetailURL format values
	CustomURL string `dynamodbav:"CustomURL,omitempty" json:"custom_url,omitempty"`
	// NotificationAppName makes Santa notify the user that this app is now allowed; ignored during clean syncs
	NotificationAppName string `dynamodbav:"NotificationAppName,omitempty" json:"notification_app_name,omitempty"`
	// CELExpr is the expression Santa evaluates for rules with the CEL policy
	CELExpr string `dynamodbav:"CELExpr,omitempty" json:"cel_expr,omitempty"`
	// EventDetailButtonLabel overrides the text of the block dialog's button
	EventDetailButtonLabel string `dynamodbav:"EventDetailButtonLabel,omitempty" json:"event_detail_button_label,omitempty"`
}

// RuleOption sets an optional field of a rule when it is created
type RuleOption func(*SantaRule)

func WithCustomMessage(customMessage string) RuleOption {
	return func(r *SantaRule) { r.CustomMessage = customMessage }
}

func WithCustomURL(customURL string) RuleOption {
	return func(r *SantaRule) { r.CustomURL = customURL }
}

func WithNotificationAppName(notificationAppName string) RuleOption {
	return func(r *SantaRule) { r.NotificationAppName = notificationAppName }
}

func WithCELExpr(celExpr string) RuleOption {
	return func(r *SantaRule) { r.CELExpr = celExpr }
}

func WithEventDetailButtonLabel(eventDetailButtonLabel string) RuleOption {
	return func(r *SantaRule) { r.EventDetailButtonLabel = eventDetailButtonLabel }
}

// ApplyOptions sets every given option on the rule
func (r *SantaRule) ApplyOptions(options ...RuleOption) {
	for _, option := range options {
		option(r)
	}
}
