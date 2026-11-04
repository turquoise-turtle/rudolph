package machinerules

import (
	"errors"
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
)

// ErrRuleExpiredOrGone is returned by UpdateMachineRulePolicyUnlessExpired when the rule has expired or no longer
// exists, so that nothing was updated
var ErrRuleExpiredOrGone = errors.New("the machine rule has expired or no longer exists")

// UpdateMachineRule sets the policy of an existing machine rule and, unless expires is the zero time, when it expires.
//
// The zero time leaves the rule's expiry as it is: a permanent rule stays permanent and an expiring rule keeps its
// expiry. (Unlike AddNewMachineRule, where the zero time means "never expires", an update cannot take the expiry off a
// rule; create the rule again for that.)
//
// @deprecated
func UpdateMachineRule(client dynamodb.UpdateItemAPI, machineID string, sha256 string, ruleType types.RuleType, rulePolicy types.Policy, expires time.Time) (err error) {
	update, err := policyUpdate(rulePolicy, expires)
	if err != nil {
		return err
	}

	_, err = client.UpdateItem(machineRuleKey(machineID, sha256, ruleType), update)
	if err != nil {
		err = fmt.Errorf("failed to update machine rule: %w", err)
		return
	}

	return
}

// UpdateMachineRulePolicyUnlessExpired sets the policy of a machine rule and keeps its expiry, but only while the rule
// exists and has not expired at now. Otherwise it changes nothing and returns ErrRuleExpiredOrGone: an expired rule
// no longer applies, so updating it would look successful and change nothing on any Mac.
func UpdateMachineRulePolicyUnlessExpired(client dynamodb.UpdateItemIfAPI, now time.Time, machineID string, sha256 string, ruleType types.RuleType, rulePolicy types.Policy) error {
	update, err := policyUpdate(rulePolicy, time.Time{})
	if err != nil {
		return err
	}

	expiresAfter := expression.Name("ExpiresAfter")
	notExpired := expression.Or(
		expression.AttributeNotExists(expiresAfter),
		expression.Equal(expiresAfter, expression.Value(0)),
		expression.GreaterThan(expiresAfter, expression.Value(now.Unix())),
	)
	_, err = client.UpdateItemIf(machineRuleKey(machineID, sha256, ruleType), update, notExpired)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrRuleExpiredOrGone
	}
	if err != nil {
		return fmt.Errorf("failed to update machine rule: %w", err)
	}
	return nil
}

func policyUpdate(rulePolicy types.Policy, expires time.Time) (updateRulePolicyRequest, error) {
	// A policy update cannot provide the expression a CEL rule requires
	if rulePolicy == types.RulePolicyCEL {
		return updateRulePolicyRequest{}, globalrules.ErrUpdateToCELPolicy
	}
	update := updateRulePolicyRequest{Policy: rulePolicy}
	if !expires.IsZero() {
		if expires.Unix() <= 0 {
			return updateRulePolicyRequest{}, fmt.Errorf("invalid expiry %s", expires.Format(time.RFC3339))
		}
		update.ExpiresAfter = expires.Unix()
	}
	return update, nil
}

func machineRuleKey(machineID string, sha256 string, ruleType types.RuleType) dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{
		PartitionKey: machineRulePK(machineID),
		SortKey:      machineRuleSK(sha256, ruleType),
	}
}
