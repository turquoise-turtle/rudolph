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
}

type updateRulePolicyRequest struct {
	Policy types.Policy `dynamodbav:"Policy"`
	// Not omitempty, so that an update clears the expression
	CELExpr string `dynamodbav:"CELExpr"`
}

func globalRulesSK(identifier string, ruleType types.RuleType) string {
	return rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)
}
