package machinerules

import (
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
)

// UpdateMachineRule sets the policy of an existing machine rule and, unless expires is the zero time, when it expires.
//
// The zero time leaves the rule's expiry as it is: a permanent rule stays permanent and an expiring rule keeps its
// expiry. (Unlike AddNewMachineRule, where the zero time means "never expires", an update cannot take the expiry off a
// rule; create the rule again for that.)
//
// @deprecated
func UpdateMachineRule(client dynamodb.UpdateItemAPI, machineID string, sha256 string, ruleType types.RuleType, rulePolicy types.Policy, expires time.Time) (err error) {
	// A policy update cannot provide the expression a CEL rule requires
	if rulePolicy == types.RulePolicyCEL {
		return globalrules.ErrUpdateToCELPolicy
	}

	update := updateRulePolicyRequest{Policy: rulePolicy}
	if !expires.IsZero() {
		if expires.Unix() <= 0 {
			return fmt.Errorf("invalid expiry %s", expires.Format(time.RFC3339))
		}
		update.ExpiresAfter = expires.Unix()
	}

	_, err = client.UpdateItem(
		dynamodb.PrimaryKey{
			PartitionKey: machineRulePK(machineID),
			SortKey:      machineRuleSK(sha256, ruleType),
		},
		update,
	)

	if err != nil {
		err = fmt.Errorf("failed to update machine rule: %w", err)
		return
	}

	return
}
