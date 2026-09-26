package feedrules

import (
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

func (f *FeedRuleRow) feedRuleRowValidation() (bool, error) {
	// RuleType validation
	_, err := f.RuleType.MarshalText()
	if err != nil {
		return false, err
	}

	// RulePolicy validation
	_, err = f.Policy.MarshalText()
	if err != nil {
		return false, err
	}

	// Policy and the fields it depends on
	err = rules.ValidatePolicy(f.SantaRule)
	if err != nil {
		return false, err
	}

	validIdentifier := rules.ValidIdentifier
	if f.Policy == types.RulePolicyRemove {
		// A removal only has to name the stored rule, which may predate today's identifier validation
		validIdentifier = rules.ValidRemovalIdentifier
	}
	if !validIdentifier(f.RuleType, f.Identifier) {
		return false, nil
	}

	// All validations have passed
	return true, nil
}
