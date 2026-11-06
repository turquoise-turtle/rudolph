package globalrules

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrRuleGone is returned by ReplaceGlobalRule when the rule no longer exists, so that nothing was written
var ErrRuleGone = errors.New("the global rule no longer exists")

// ReplaceGlobalRule writes an existing global rule again with new fields, and adds it to the rules feed, in one
// transaction that only succeeds while the row still exists, so a removal since the rule was read is not undone.
// Unlike AddNewGlobalRule it keeps what row carries, such as CollectAllowedEvents.
func ReplaceGlobalRule(time clock.TimeProvider, client dynamodb.TransactWriteItemsAPI, row GlobalRuleRow) error {
	row.GlobalRulePolicy = row.Policy
	isValid, err := row.globalRuleValidation()
	if err != nil {
		return err
	}
	if !isValid {
		return fmt.Errorf("invalid identifier %q for rule type %s", row.Identifier, ruleTypeName(row.RuleType))
	}
	feedRule, err := feedrules.ConstructFeedRuleFromBaseRule(time, row.SantaRule)
	if err != nil {
		return err
	}
	putRule, err := client.CreateTransactPutItem(row)
	if err != nil {
		return err
	}
	putRule.Put.ConditionExpression = aws.String("attribute_exists(PK)")
	putFeed, err := client.CreateTransactPutItem(feedRule)
	if err != nil {
		return err
	}
	_, err = client.TransactWriteItems([]awsdynamodbtypes.TransactWriteItem{*putRule, *putFeed}, nil)
	if dynamodb.IsTransactionConditionalCheckFailed(err) {
		return ErrRuleGone
	}
	return err
}
