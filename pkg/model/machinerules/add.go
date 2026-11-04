package machinerules

import (
	"errors"
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

// AddNewMachineRule creates a rule for a single machine.
// Optional rule fields (e.g. custom message, CEL expression) are set with rules.RuleOption values.
// The rule expires at expires; the zero time.Time creates a rule that never expires, stored without
// ExpiresAfter (the table's TTL attribute).
func AddNewMachineRule(
	client dynamodb.PutItemAPI,
	machineID string,
	identifier string,
	ruleType types.RuleType,
	policy types.Policy,
	description string,
	expires time.Time,
	options ...rules.RuleOption,
) error {
	santaRule := rules.SantaRule{
		RuleType:   ruleType,
		Policy:     policy,
		Identifier: identifier,
	}
	santaRule.ApplyOptions(options...)

	// Input Validation
	isValid, err := ruleValidation(
		machineID,
		santaRule,
		description,
		expires,
	)
	if err != nil {
		return err
	}
	if !isValid {
		return errors.New("no errors occurred during the rule validation check but the provided rule is not valid")
	}

	rule := MachineRuleRow{
		PrimaryKey: dynamodb.PrimaryKey{
			PartitionKey: machineRulePK(machineID),
			SortKey:      machineRuleSK(identifier, ruleType),
		},
		Description: description,
		SantaRule:   santaRule,
	}
	if !expires.IsZero() {
		rule.ExpiresAfter = expires.Unix()
	}

	_, err = client.PutItem(rule)
	if err != nil {
		return err
	}
	return nil
}

func ruleValidation(
	machineID string,
	rule rules.SantaRule,
	description string,
	expires time.Time,
) (bool, error) {
	// MachineID validation
	err := types.ValidateMachineID(machineID)
	if err != nil {
		return false, err
	}

	// RuleType validation
	ruleTypeName, err := rule.RuleType.MarshalText()
	if err != nil {
		return false, err
	}

	// RulePolicy validation
	_, err = rule.Policy.MarshalText()
	if err != nil {
		return false, err
	}

	// Policy and the fields it depends on
	err = rules.ValidatePolicy(rule)
	if err != nil {
		return false, err
	}

	// Bounded optional fields
	err = rules.ValidateFieldSizes(rule)
	if err != nil {
		return false, err
	}

	// Identifier validation
	if !rules.ValidIdentifier(rule.RuleType, rule.Identifier) {
		return false, fmt.Errorf("invalid identifier %q for rule type %s", rule.Identifier, ruleTypeName)
	}

	// Expires validation: zero means permanent, anything else must be a positive Unix time
	if !expires.IsZero() && expires.Unix() <= 0 {
		err = errors.New("expires time is not a positive time")
		return false, err
	}

	// All validations have passed
	return true, nil
}
