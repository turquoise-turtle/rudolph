package machinerules

import (
	"errors"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
)

// ErrRuleNotFound is returned by RemoveMachineRule when the target rule does not exist in DynamoDB.
var ErrRuleNotFound = errors.New("no such rule exists")

// @deprecated
// RemoveMachineRule executes the flow to "remove" a rule for the given machine. Upon next sync,
// the santa sensor will receive instructions to remove the rule from the database. If there is a
// global rule for the same Binary/Cert, the policy of the global will be "inherited" instead.
func RemoveMachineRule(getter dynamodb.GetItemAPI, updater dynamodb.UpdateItemAPI, machineID string, ruleSortKey string) error {
	rule, err := getItemAsMachineRule(
		getter,
		machineRulePK(machineID),
		ruleSortKey,
	)

	if err != nil {
		return fmt.Errorf("failed to retrieve existing rule")
	}
	if rule == nil {
		return ErrRuleNotFound
	}

	// First pull the associated global rule if any
	globalRule, err := globalrules.GetGlobalRuleBySortKey(getter, ruleSortKey)

	if err != nil {
		return fmt.Errorf("something went wrong during pulling global rule")
	}

	request := ruleRemovalRequest{
		DeleteOnNextSync: true,
	}
	if globalRule != nil && rules.ValidatePolicy(globalRule.SantaRule) != nil {
		// A global rule Santa cannot apply (e.g. the legacy ALLOWLIST_TRANSITIVE policy) is never sent to Macs, so
		// there is nothing to fall back to: the Mac must drop the override.
		log.Printf("The global rule this machine rule overrides cannot be sent to Santa; removing the rule instead")
		globalRule = nil
	}
	if globalRule != nil {
		// There is an associated global rule.
		//
		// In this case, when we REMOVE the rule, the Mac falls back to the global rule rather than
		// losing it: a machine rule marked for deletion no longer applies, and ruledownload sends the
		// Mac the global rule for the same identifier and type instead (read when it is sent, so a
		// later change to the global rule is honoured), until two completed syncs have sent it (see
		// the delivery tracking in delivery.go).
		//
		// The marked row records what the machine falls back to: it INHERITS the policy and the
		// fields (e.g. CEL expression, custom message) of the global rule; fields the global rule
		// does not have are cleared.
		log.Printf("There is a global rule that this machine-rule overwrites. Inheriting...")
		request.Policy = globalRule.Policy
		request.CustomMessage = globalRule.CustomMessage
		request.CustomURL = globalRule.CustomURL
		request.NotificationAppName = globalRule.NotificationAppName
		request.CELExpr = globalRule.CELExpr
		request.EventDetailButtonLabel = globalRule.EventDetailButtonLabel
	} else {
		// There was no associated global rule.
		// Simply change item to a "REMOVE" (without any other fields); ruledownload sends the Mac a REMOVE until two
		// completed syncs have sent it, whether or not this record still exists by then (see delivery.go)
		request.Policy = types.Remove
	}

	_, err = updater.UpdateItem(rule.PrimaryKey, request)
	if err != nil {
		return fmt.Errorf("something went wrong changing this rule to a remove rule: %w", err)
	}

	log.Printf("Successfully marked as 'remove'.")
	return nil

}

// DeleteRuleMarkedForDeletion deletes a machine rule row that RemoveMachineRule marked for deletion. The row is only
// deleted while it is still marked, so a rule added again in the meantime is kept; it is not an error if it is not.
//
// Deleting the row does not lose the removal: which rules a Mac may hold is tracked by the delivery items (see
// GetDeliveries), and a rule the Mac may hold that no longer applies, row or not, is removed from it (or replaced by
// the global rule) on its next syncs until two completed syncs have sent that.
func DeleteRuleMarkedForDeletion(client dynamodb.DeleteItemIfAPI, key dynamodb.PrimaryKey) error {
	_, err := client.DeleteItemIf(key, expression.Equal(expression.Name("DeleteOnNextSync"), expression.Value(true)))
	if err != nil && !dynamodb.IsConditionalCheckFailed(err) {
		return fmt.Errorf("failed to delete a machine rule marked for deletion: %w", err)
	}
	return nil
}

// @deprecated
type RuleRemovalService interface {
	RemoveMachineRule(machineID string, ruleSortKey string) (err error)
}

// @deprecated
type ConcreteRuleRemovalService struct {
	Getter  dynamodb.GetItemAPI
	Updater dynamodb.UpdateItemAPI
}

// @deprecated
func (c ConcreteRuleRemovalService) RemoveMachineRule(machineID string, ruleSortKey string) (err error) {
	return RemoveMachineRule(c.Getter, c.Updater, machineID, ruleSortKey)
}
