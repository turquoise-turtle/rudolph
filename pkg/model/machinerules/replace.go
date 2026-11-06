package machinerules

import (
	"errors"
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
)

// ErrRuleChangedOrGone is returned by ReplaceMachineRule when the rule no longer exists or is being removed, so that
// nothing was written
var ErrRuleChangedOrGone = errors.New("the machine rule no longer exists or is being removed")

// ReplaceMachineRule writes an existing machine rule again with new fields in one conditional write: only while the
// row exists and is not being removed (DeleteOnNextSync or the REMOVE policy), so a removal that happened since the
// rule was read is not undone. Unlike AddNewMachineRule it keeps what row carries, such as CollectAllowedEvents.
func ReplaceMachineRule(client dynamodb.PutItemIfAPI, row MachineRuleRow) error {
	machineID := row.PartitionKey[len(machineRulesPKPrefix):]
	expires := time.Time{}
	if row.ExpiresAfter != 0 {
		expires = time.Unix(row.ExpiresAfter, 0)
	}
	if ok, err := ruleValidation(machineID, row.SantaRule, row.Description, expires); err != nil {
		return err
	} else if !ok {
		return errors.New("the rule is not valid")
	}
	deleting := expression.Name("DeleteOnNextSync")
	condition := expression.AttributeExists(expression.Name("PK")).
		And(expression.Or(expression.AttributeNotExists(deleting), expression.Equal(deleting, expression.Value(false)))).
		And(expression.NotEqual(expression.Name("Policy"), expression.Value(types.RulePolicyRemove)))
	_, err := client.PutItemIf(row, condition)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrRuleChangedOrGone
	}
	if err != nil {
		return fmt.Errorf("failed to write machine rule: %w", err)
	}
	return nil
}
