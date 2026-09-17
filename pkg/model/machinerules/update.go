package machinerules

import (
	"errors"
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
)

// @deprecated
func UpdateMachineRule(client dynamodb.UpdateItemAPI, machineID string, sha256 string, ruleType types.RuleType, rulePolicy types.Policy, expires time.Time) (err error) {
	// A policy update cannot provide the expression a CEL rule requires
	if rulePolicy == types.RulePolicyCEL {
		return errors.New("a rule cannot be updated to the CEL policy; remove it and create it again with a CEL expression")
	}

	pk := machineRulePK(machineID)
	sk := machineRuleSK(sha256, ruleType)

	_, err = client.UpdateItem(
		dynamodb.PrimaryKey{
			PartitionKey: pk,
			SortKey:      sk,
		},
		updateRulePolicyRequest{
			Policy:       rulePolicy,
			ExpiresAfter: expires.Unix(),
		},
	)

	if err != nil {
		err = fmt.Errorf("failed to update machine rule: %w", err)
		return
	}

	return
}
