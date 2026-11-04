package rules

import "fmt"

// Upper bounds, in bytes (UTF-8), of the free-form fields a rule can carry. Every rule a Mac may hold is sent to it in
// rule download pages and kept in the rules table, so a single oversized field makes every sync of every Mac that
// receives the rule larger. The limits are far above what Santa's block dialog and notifications can usefully show:
// a custom message is a few sentences, a button label a couple of words, an app name a single name.
const (
	MaxCustomMessageBytes          = 4096
	MaxCustomURLBytes              = 4096
	MaxEventDetailButtonLabelBytes = 256
	MaxNotificationAppNameBytes    = 256
	MaxCELExprBytes                = 16384
)

// ValidateFieldSizes returns an error naming the first optional field of the rule that is longer than its limit
func ValidateFieldSizes(rule SantaRule) error {
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"custom_msg", rule.CustomMessage, MaxCustomMessageBytes},
		{"custom_url", rule.CustomURL, MaxCustomURLBytes},
		{"event_detail_button_label", rule.EventDetailButtonLabel, MaxEventDetailButtonLabelBytes},
		{"notification_app_name", rule.NotificationAppName, MaxNotificationAppNameBytes},
		{"cel_expr", rule.CELExpr, MaxCELExprBytes},
	} {
		if len(field.value) > field.limit {
			return fmt.Errorf("%s is %d bytes long; the limit is %d bytes", field.name, len(field.value), field.limit)
		}
	}
	return nil
}
