package machinerules

import (
	"errors"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
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
		// In this case, when we REMOVE the rule, we will want a rule entry to show up on the
		// next sync that reflects the state of the global rule.
		//
		// The way we achieve this is to change the machine-specific rule to INHERIT the policy
		// and the fields (e.g. CEL expression, custom message) from the global rule; fields the
		// global rule does not have are cleared. The next time the machine performs a sync, it
		// will reach this machine-specific rule on the final page, updating the state. Finally,
		// the postflight process then delete this record.
		log.Printf("There is a global rule that this machine-rule overwrites. Inheriting...")
		request.Policy = globalRule.Policy
		request.CustomMessage = globalRule.CustomMessage
		request.CustomURL = globalRule.CustomURL
		request.NotificationAppName = globalRule.NotificationAppName
		request.CELExpr = globalRule.CELExpr
		request.EventDetailButtonLabel = globalRule.EventDetailButtonLabel
	} else {
		// There was no associated global rule.
		// Simply change item to a "REMOVE" (without any other fields) and then delete the DynamoDB record after the next sync
		request.Policy = types.Remove
	}

	_, err = updater.UpdateItem(rule.PrimaryKey, request)
	if err != nil {
		return fmt.Errorf("something went wrong changing this rule to a remove rule: %w", err)
	}

	log.Printf("Successfully marked as 'remove'.")
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
