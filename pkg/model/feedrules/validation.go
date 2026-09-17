package feedrules

import (
	"errors"

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
	// Legacy ALLOWLIST_TRANSITIVE rules must stay removable: their removal is built from the stored rule before its
	// policy is switched to REMOVE. New rules with that policy are rejected on creation and never sent to hosts.
	err = rules.ValidatePolicy(f.SantaRule)
	if err != nil && !errors.Is(err, rules.ErrTransitivePolicy) {
		return false, err
	}

	if !rules.ValidIdentifier(f.RuleType, f.Identifier) {
		return false, nil
	}

	// All validations have passed
	return true, nil
}
