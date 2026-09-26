package machinerules

import (
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
)

// @deprecated
func UpdateMachineRule(client dynamodb.UpdateItemAPI, machineID string, sha256 string, ruleType types.RuleType, rulePolicy types.Policy, expires time.Time) (err error) {
	// A policy update cannot provide the expression a CEL rule requires
	if rulePolicy == types.RulePolicyCEL {
		return globalrules.ErrUpdateToCELPolicy
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
