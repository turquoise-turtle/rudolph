package rules

import (
	"errors"
	"regexp"
	"strings"

	"github.com/airbnb/rudolph/pkg/types"
)

var sha256Regexp = regexp.MustCompile(`^[a-f0-9]{64}$`)
var teamIDRegexp = regexp.MustCompile(`^([A-Z0-9]{1,10})$`)
var signingIDRegexp = regexp.MustCompile(`^([A-Z0-9]{1,10}|platform)(:[\w\-\.]+)$`)
var cdhashRegexp = regexp.MustCompile(`^[a-f0-9]{40}$`)

var (
	// ErrTransitivePolicy is returned for rules with the ALLOWLIST_TRANSITIVE policy. The Santa sync protocol has no
	// such policy (Santa silently ignores these rules): transitive rules are created locally by Santa for files
	// written by binaries allowed with ALLOWLIST_COMPILER.
	ErrTransitivePolicy = errors.New("rules with the ALLOWLIST_TRANSITIVE policy cannot be sent to Santa; transitive rules are created locally by Santa for files written by ALLOWLIST_COMPILER binaries")
	// ErrCELExprRequired is returned for rules with the CEL policy but no CEL expression
	ErrCELExprRequired = errors.New("rules with the CEL policy require a CEL expression")
	// ErrCELExprWithoutCELPolicy is returned for rules with a CEL expression but another policy
	ErrCELExprWithoutCELPolicy = errors.New("a CEL expression can only be set on rules with the CEL policy")
)

func ValidSha256(sha256 string) bool {
	return sha256Regexp.MatchString(sha256)
}

func ValidTeamID(teamID string) bool {
	return teamIDRegexp.MatchString(teamID)
}

func ValidSigningID(signingID string) bool {
	return signingIDRegexp.MatchString(signingID)
}

func ValidCDHash(cdhash string) bool {
	return cdhashRegexp.MatchString(cdhash)
}

// ValidIdentifier reports whether the identifier is well-formed for the rule type
func ValidIdentifier(ruleType types.RuleType, identifier string) bool {
	switch ruleType {
	case types.RuleTypeBinary, types.RuleTypeCertificate:
		return ValidSha256(identifier)
	case types.RuleTypeTeamID:
		return ValidTeamID(identifier)
	case types.RuleTypeSigningID:
		return ValidSigningID(identifier)
	case types.RuleTypeCDHash:
		return ValidCDHash(identifier)
	default:
		return false
	}
}

// ValidatePolicy returns an error when the rule's policy, together with the fields it depends on, is not one
// Santa can apply
func ValidatePolicy(rule SantaRule) error {
	switch rule.Policy {
	case types.RulePolicyAllowlistTransitive:
		return ErrTransitivePolicy
	case types.RulePolicyCEL:
		if strings.TrimSpace(rule.CELExpr) == "" {
			return ErrCELExprRequired
		}
	default:
		if rule.CELExpr != "" {
			return ErrCELExprWithoutCELPolicy
		}
	}
	return nil
}
