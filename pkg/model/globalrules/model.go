package globalrules

import (
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

const (
	globalRulesPK = "GlobalRules"
)

type GlobalRuleRow struct {
	dynamodb.PrimaryKey
	rules.SantaRule
	Description string `dynamodbav:"Description,omitempty"`
	// GlobalRulePolicy mirrors Policy, so that the web view can list global rules by policy through the
	// GlobalRulePolicy_SK index. It lives here rather than on SantaRule because machine rules and rules feed rows
	// carry Policy too and must stay out of that index.
	GlobalRulePolicy types.Policy `dynamodbav:"GlobalRulePolicy,omitempty"`
}

type updateRulePolicyRequest struct {
	Policy types.Policy `dynamodbav:"Policy"`
	// Not omitempty, so that an update clears the expression
	CELExpr string `dynamodbav:"CELExpr"`
	// Kept equal to Policy for the GlobalRulePolicy_SK index
	GlobalRulePolicy types.Policy `dynamodbav:"GlobalRulePolicy"`
}

func globalRulesSK(identifier string, ruleType types.RuleType) string {
	return rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)
}
