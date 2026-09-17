package feedrules

import (
	"github.com/airbnb/rudolph/pkg/model/rules"
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

	if !rules.ValidIdentifier(f.RuleType, f.Identifier) {
		return false, nil
	}

	// All validations have passed
	return true, nil
}
