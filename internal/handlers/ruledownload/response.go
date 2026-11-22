package ruledownload

import (
	"log"

	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

// RuledownloadResponse is the response body returned by /ruledownload endpoints
type RuledownloadResponse struct {
	Rules []RuledownloadRule `json:"rules"`
	// When a cursor is returned by the server, it is an indicator to the Santa sensor that there are
	// additional rules to be paginated through. This cursor is passed to the next request.
	Cursor string `json:"cursor,omitempty"`
}

// RuledownloadRule is a single rule returned in a RuledownloadResponse
// It duck-types to/from the SantaRule struct type
// Documentation: https://santa.dev/development/sync-protocol.html#rules-objects
type RuledownloadRule struct {
	RuleType               types.RuleType `json:"rule_type"`
	Policy                 types.Policy   `json:"policy"`
	SHA256                 string         `json:"sha256,omitempty"`
	Identifier             string         `json:"identifier"`
	CustomMessage          string         `json:"custom_msg,omitempty"`
	CustomURL              string         `json:"custom_url,omitempty"`
	NotificationAppName    string         `json:"notification_app_name,omitempty"`
	CELExpr                string         `json:"cel_expr,omitempty"`
	EventDetailButtonLabel string         `json:"event_detail_button_label,omitempty"`
}

// DDBRulesToResponseRules type converts the DynamoDB representation of a rule to an API
// representation of a Rule, which is returned in an API response.
// Rules with the ALLOWLIST_TRANSITIVE policy (only possible in rows written by older versions of Rudolph) are
// omitted: it is not a policy of the Santa sync protocol and Santa would silently ignore them.
func DDBRulesToResponseRules(rulesList []rules.SantaRule) (responseRules []RuledownloadRule) {
	return ResponseRulesForClient(rulesList, false)
}

// ResponseRulesForClient converts rules like DDBRulesToResponseRules, for the sync's client (see
// responseRuleForClient).
func ResponseRulesForClient(rulesList []rules.SantaRule, silentBlocklistOnly bool) []RuledownloadRule {
	responseRules := make([]RuledownloadRule, 0, len(rulesList))
	for _, rule := range rulesList {
		if responseRule, sent := responseRuleForClient(rule, silentBlocklistOnly); sent {
			responseRules = append(responseRules, responseRule)
		}
	}
	if skipped := len(rulesList) - len(responseRules); skipped > 0 {
		log.Printf("  omitted %d rule(s) with the non-protocol ALLOWLIST_TRANSITIVE policy", skipped)
	}
	return responseRules
}

// responseRuleForClient returns the rule as the sync's client is sent it, and false for a rule that is never sent:
// one with the ALLOWLIST_TRANSITIVE policy. When silentBlocklistOnly (Santa before 2026.6, see
// syncstate.SyncState.SilentBlocklistOnly), SILENT_GUI_BLOCKLIST and SILENT_TTY_BLOCKLIST are sent as
// SILENT_BLOCKLIST: such a client skips a rule whose policy it does not know (SNTSyncRuleDownload.mm in
// testdata/santa-2025.12), which would leave the binary unblocked. Only what is sent changes; delivery tracking keys
// on rule type and identifier, so it is the same whichever policy the Mac got.
func responseRuleForClient(rule rules.SantaRule, silentBlocklistOnly bool) (RuledownloadRule, bool) {
	if rule.Policy == types.RulePolicyAllowlistTransitive {
		return RuledownloadRule{}, false
	}
	responseRule := RuledownloadRule(rule)
	if silentBlocklistOnly {
		responseRule.Policy = responseRule.Policy.WithoutSilentGUITTY()
	}
	return responseRule, true
}
