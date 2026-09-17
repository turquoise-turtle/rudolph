package globalrules

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	awsdynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// AddNewGlobalRule creates a global rule and adds it to the rules feed.
// Optional rule fields (e.g. custom message, CEL expression) are set with rules.RuleOption values.
func AddNewGlobalRule(
	time clock.TimeProvider,
	client dynamodb.TransactWriteItemsAPI,
	identifier string,
	ruleType types.RuleType,
	policy types.Policy,
	description string,
	options ...rules.RuleOption,
) error {
	rule := &GlobalRuleRow{
		PrimaryKey: dynamodb.PrimaryKey{
			PartitionKey: globalRulesPK,
			SortKey:      globalRulesSK(identifier, ruleType),
		},
		Description: description,
		SantaRule: rules.SantaRule{
			RuleType:   ruleType,
			Policy:     policy,
			Identifier: identifier,
		},
	}
	rule.ApplyOptions(options...)

	// Input Validation
	isValid, err := rule.globalRuleValidation()
	if err != nil {
		return err
	}
	if !isValid {
		return fmt.Errorf("invalid identifier %q for rule type %s", identifier, ruleTypeName(ruleType))
	}

	feedRule := feedrules.ConstructFeedRuleFromBaseRule(time, rule.SantaRule)

	putItem1, err := client.CreateTransactPutItem(rule)
	if err != nil {
		return err
	}
	putItem2, err := client.CreateTransactPutItem(feedRule)
	if err != nil {
		return err
	}

	putItems := []awsdynamodbtypes.TransactWriteItem{
		*putItem1,
		*putItem2,
	}

	_, err = client.TransactWriteItems(putItems, nil)
	return err
}

// ruleTypeName returns the Santa name of a rule type for error messages
func ruleTypeName(ruleType types.RuleType) string {
	name, err := ruleType.MarshalText()
	if err != nil {
		return fmt.Sprintf("%d", ruleType)
	}
	return string(name)
}
