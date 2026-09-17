package machinerules

import (
	"fmt"

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
}

func machineRulePK(machineID string) string {
	return fmt.Sprintf("%s%s", machineRulesPKPrefix, machineID)
}
func machineRuleSK(identifier string, ruleType types.RuleType) string {
	return rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)
}
