package globalrules

import (
	"github.com/airbnb/rudolph/pkg/model/rules"
)

func (g *GlobalRuleRow) globalRuleValidation() (bool, error) {
	// RuleType validation
	_, err := g.RuleType.MarshalText()
	if err != nil {
		return false, err
	}

	// RulePolicy validation
	_, err = g.Policy.MarshalText()
	if err != nil {
		return false, err
	}

	// Policy and the fields it depends on
	err = rules.ValidatePolicy(g.SantaRule)
	if err != nil {
		return false, err
	}

	if !rules.ValidIdentifier(g.RuleType, g.Identifier) {
		return false, nil
	}

	// All validations have passed
	return true, nil
}
