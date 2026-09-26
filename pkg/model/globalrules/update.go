package globalrules

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/airbnb/rudolph/pkg/types"
	awsdynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrUpdateToCELPolicy is returned when updating a rule to the CEL policy, which requires an expression that a policy
// update cannot provide
var ErrUpdateToCELPolicy = errors.New("a rule cannot be updated to the CEL policy; remove it and create it again with a CEL expression")

// GlobalRuleUpdaterAPI is what UpdateGlobalRule needs: the existing rule is read so the feed keeps its fields
type GlobalRuleUpdaterAPI interface {
	dynamodb.GetItemAPI
	dynamodb.TransactWriteItemsAPI
}

// UpdateGlobalRule changes the policy of an existing global rule and adds the change to the rules feed.
// The rule's other fields (custom message, URL, ...) are kept, and its CEL expression is cleared.
func UpdateGlobalRule(
	time clock.TimeProvider,
	client GlobalRuleUpdaterAPI,
	identifier string,
	ruleType types.RuleType,
	rulePolicy types.Policy,
) error {
	if rulePolicy == types.RulePolicyCEL {
		return ErrUpdateToCELPolicy
	}

	// Get the PK/SK values
	pk := globalRulesPK
	sk := globalRulesSK(identifier, ruleType)

	primaryKey := dynamodb.PrimaryKey{
		PartitionKey: pk,
		SortKey:      sk,
	}

	// Incrementally syncing hosts replace the whole rule with the feed row, so it must carry the rule's current fields
	existingRule, err := getItemAsGlobalRule(client, pk, sk)
	if err != nil {
		return fmt.Errorf("failed to read global rule to update: %w", err)
	}
	if existingRule == nil {
		return fmt.Errorf("no global rule with sk (%s) exists", sk)
	}

	// UpdatedRule for the ruleFeed Update
	updatedRule := existingRule.SantaRule
	updatedRule.Policy = rulePolicy
	// Only CEL rules may have an expression
	updatedRule.CELExpr = ""

	updateFeedRuleItem, err := feedrules.ConstructFeedRuleFromBaseRule(time, updatedRule)
	if err != nil {
		return err
	}

	// Updated rulePolicy request
	updateItem := updateRulePolicyRequest{
		Policy:  rulePolicy,
		CELExpr: updatedRule.CELExpr,
	}

	// Update the rule only if it still exists: a rule removed since it was read above must not be recreated as a
	// partial row (no RuleType or Identifier), which would break every clean sync
	updateItem1, err := client.CreateTransactUpdateExistingItem(primaryKey, updateItem)
	if err != nil {
		return err
	}

	// Update the ruleFeed by creating a TransactPutItem
	putItem1, err := client.CreateTransactPutItem(updateFeedRuleItem)
	if err != nil {
		return err
	}

	// Build the transactWriteItems
	transactItems := []awsdynamodbtypes.TransactWriteItem{
		*updateItem1,
		*putItem1,
	}

	// Send the TransactWriteRequest
	_, err = client.TransactWriteItems(transactItems, nil)
	if dynamodb.IsTransactionConditionalCheckFailed(err) {
		return fmt.Errorf("no global rule with sk (%s) exists: it was removed while it was being updated", sk)
	}
	if err != nil {
		return fmt.Errorf("failed to update global rule: %w", err)
	}

	return nil
}
