package machinerules

import (
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

const MachineRuleDefaultExpirationHours = 24
const machineRulesPKPrefix = "MachineRules#"

type MachineRuleRow struct {
	dynamodb.PrimaryKey
	rules.SantaRule
	Description      string `dynamodbav:"Description,omitempty"`
	DeleteOnNextSync bool   `dynamodbav:"DeleteOnNextSync,omitempty"`
	ExpiresAfter     int64  `dynamodbav:"ExpiresAfter,omitempty"`
	MachineID        string `dynamodbav:"MachineID,omitempty"` // Broken; don't use this for now
	// CollectAllowedEvents stores the executions this rule allows on its Mac in the events table (see
	// pkg/model/eventcollection). Never sent to Santa, and not part of delivery tracking.
	CollectAllowedEvents bool `dynamodbav:"CollectAllowedEvents,omitempty"`
}

// Fragments
// The rule fields are not omitempty: the machine rule takes exactly the fields of the rule it falls back to
type ruleRemovalRequest struct {
	Policy                 types.Policy `dynamodbav:"Policy"`
	DeleteOnNextSync       bool         `dynamodbav:"DeleteOnNextSync"`
	CustomMessage          string       `dynamodbav:"CustomMessage"`
	CustomURL              string       `dynamodbav:"CustomURL"`
	NotificationAppName    string       `dynamodbav:"NotificationAppName"`
	CELExpr                string       `dynamodbav:"CELExpr"`
	EventDetailButtonLabel string       `dynamodbav:"EventDetailButtonLabel"`
}

type updateRulePolicyRequest struct {
	Policy types.Policy `dynamodbav:"Policy,omitempty"`
	// Not omitempty, so that a policy update clears the expression (only CEL rules may have one)
	CELExpr      string `dynamodbav:"CELExpr"`
	ExpiresAfter int64  `dynamodbav:"ExpiresAfter,omitempty"`
	Description  string `dynamodbav:"Description,omitempty"`
	// Not omitempty: updating a rule that is being removed brings it back with the new policy. Its optional fields
	// are the ones it has now (for a rule that overrode a global rule, those of the global rule; see RemoveMachineRule).
	DeleteOnNextSync bool `dynamodbav:"DeleteOnNextSync"`
}

// AppliesAt reports whether the rule is in force for its machine at the given time.
//
// A rule stops applying when it expires (ExpiresAfter is also the table's TTL attribute, but DynamoDB can take days to
// delete an expired row) or when it is marked for removal. A rule that stopped applying is never sent as-is: if the
// Mac may hold it, ruledownload sends its removal (or the global rule it overrode) instead.
func (r MachineRuleRow) AppliesAt(now time.Time) bool {
	if r.DeleteOnNextSync || r.Policy == types.RulePolicyRemove {
		return false
	}
	return r.ExpiresAfter == 0 || r.ExpiresAfter > now.Unix()
}

func machineRulePK(machineID string) string {
	return fmt.Sprintf("%s%s", machineRulesPKPrefix, machineID)
}
func machineRuleSK(identifier string, ruleType types.RuleType) string {
	return rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)
}
