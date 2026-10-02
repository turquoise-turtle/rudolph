package rules

import (
	"fmt"
	"log"
	"strings"

	"github.com/airbnb/rudolph/pkg/types"
)

// @deprecated
func RuleSortKeyFromTypeSHA(sha256 string, ruleType types.RuleType) string {
	return RuleSortKeyFromTypeIdentifier(sha256, ruleType)
}

func RuleSortKeyFromTypeIdentifier(identifier string, ruleType types.RuleType) string {
	switch ruleType {
	case types.RuleTypeBinary:
		return fmt.Sprintf("%s%s", binaryRuleSKPrefix, identifier)
	case types.RuleTypeCertificate:
		return fmt.Sprintf("%s%s", certificateRuleSKPrefix, identifier)
	case types.RuleTypeTeamID:
		return fmt.Sprintf("%s%s", teamIDRuleSKPrefix, identifier)
	case types.RuleTypeSigningID:
		return fmt.Sprintf("%s%s", signingIDRuleSKPrefix, identifier)
	case types.RuleTypeCDHash:
		return fmt.Sprintf("%s%s", cdhashRuleSKPrefix, identifier)
	default:
		log.Printf("error (recovered): encountered unknown ruleType: (%+v)", ruleType)
		return ""
	}
}

// RuleTypeIdentifierFromSortKey is the inverse of RuleSortKeyFromTypeIdentifier: it returns the rule type and
// identifier a rule sort key was built from, and false when the key is not a rule sort key.
func RuleTypeIdentifierFromSortKey(sortKey string) (ruleType types.RuleType, identifier string, ok bool) {
	for _, candidate := range []struct {
		prefix   string
		ruleType types.RuleType
	}{
		{binaryRuleSKPrefix, types.RuleTypeBinary},
		{certificateRuleSKPrefix, types.RuleTypeCertificate},
		{teamIDRuleSKPrefix, types.RuleTypeTeamID},
		{signingIDRuleSKPrefix, types.RuleTypeSigningID},
		{cdhashRuleSKPrefix, types.RuleTypeCDHash},
	} {
		if identifier, found := strings.CutPrefix(sortKey, candidate.prefix); found && identifier != "" {
			return candidate.ruleType, identifier, true
		}
	}
	return 0, "", false
}
