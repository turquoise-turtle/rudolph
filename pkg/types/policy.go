package types

import (
	"fmt"

	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Policy represents the Santa Rule Policy.
type Policy int

const (
	// @deprecated
	Allowlist           = RulePolicyAllowlist
	Blocklist           = RulePolicyBlocklist
	SilentBlocklist     = RulePolicySilentBlocklist
	Remove              = RulePolicyRemove
	AllowlistCompiler   = RulePolicyAllowlistCompiler
	AllowlistTransitive = RulePolicyAllowlistTransitive
)

const (
	RulePolicyAllowlist Policy = iota + 1
	RulePolicyBlocklist
	RulePolicySilentBlocklist
	// Remove is a "special" rule in that, when it is sent by the server, it instructs the sensor
	// to delete any associated rule.
	RulePolicyRemove
	// AllowlistCompiler is a Transitive Allowlist policy which allows binaries created by
	// a specific compiler. EnabledTransitiveRules must be set to true in the Preflight first.
	RulePolicyAllowlistCompiler
	// Transitive rules are created by the santa sensor itself; it is never created by the server.
	// Transitive rules are destroyed upon every clean sync.
	// This is not a policy of the Santa sync protocol: rules with this policy must never be created or sent to
	// clients (Santa silently ignores them). It is kept only so that legacy rows can still be read.
	RulePolicyAllowlistTransitive
	// CEL rules decide the outcome by evaluating the CEL expression attached to the rule.
	RulePolicyCEL
	// SilentGUIBlocklist blocks without showing GUI notifications; TTY notifications are still shown.
	RulePolicySilentGUIBlocklist
	// SilentTTYBlocklist blocks without showing TTY notifications; GUI notifications are still shown.
	RulePolicySilentTTYBlocklist
)

// UnmarshalText for JSON marshalling interface
// Use Santa defined constants
// https://github.com/google/santa/blob/main/Source/santactl/Commands/sync/SNTCommandSyncConstants.m#L98-L109
func (p *Policy) UnmarshalText(text []byte) error {
	switch t := string(text); t {
	case "ALLOWLIST":
		*p = RulePolicyAllowlist
	case "BLOCKLIST":
		*p = RulePolicyBlocklist
	case "SILENT_BLOCKLIST":
		*p = RulePolicySilentBlocklist
	case "REMOVE":
		*p = RulePolicyRemove
	case "ALLOWLIST_COMPILER":
		*p = RulePolicyAllowlistCompiler
	case "ALLOWLIST_TRANSITIVE":
		*p = RulePolicyAllowlistTransitive
	case "CEL":
		*p = RulePolicyCEL
	case "SILENT_GUI_BLOCKLIST":
		*p = RulePolicySilentGUIBlocklist
	case "SILENT_TTY_BLOCKLIST":
		*p = RulePolicySilentTTYBlocklist
	default:
		return fmt.Errorf("unknown policy value %q", t)
	}
	return nil
}

// MarshalText for JSON marshalling interface
func (p Policy) MarshalText() ([]byte, error) {
	switch p {
	case RulePolicyAllowlist:
		return []byte("ALLOWLIST"), nil
	case RulePolicyBlocklist:
		return []byte("BLOCKLIST"), nil
	case RulePolicySilentBlocklist:
		return []byte("SILENT_BLOCKLIST"), nil
	case RulePolicyRemove:
		return []byte("REMOVE"), nil
	case RulePolicyAllowlistCompiler:
		return []byte("ALLOWLIST_COMPILER"), nil
	case RulePolicyAllowlistTransitive:
		return []byte("ALLOWLIST_TRANSITIVE"), nil
	case RulePolicyCEL:
		return []byte("CEL"), nil
	case RulePolicySilentGUIBlocklist:
		return []byte("SILENT_GUI_BLOCKLIST"), nil
	case RulePolicySilentTTYBlocklist:
		return []byte("SILENT_TTY_BLOCKLIST"), nil
	default:
		return nil, fmt.Errorf("unknown policy %d", p)
	}
}

// MarshalDynamoDBAttributeValue for ddb
func (p Policy) MarshalDynamoDBAttributeValue() (awstypes.AttributeValue, error) {
	var s string
	switch p {
	case RulePolicyAllowlist:
		s = "1"
	case RulePolicyBlocklist:
		s = "2"
	case RulePolicySilentBlocklist:
		s = "3"
	case RulePolicyRemove:
		s = "4"
	case RulePolicyAllowlistCompiler:
		s = "5"
	case RulePolicyAllowlistTransitive:
		s = "6"
	case RulePolicyCEL:
		s = "7"
	case RulePolicySilentGUIBlocklist:
		s = "8"
	case RulePolicySilentTTYBlocklist:
		s = "9"
	default:
		return nil, fmt.Errorf("unknown policy value %d", p)
	}
	return &awstypes.AttributeValueMemberN{Value: s}, nil
}

// UnmarshalDynamoDBAttributeValue implements the Unmarshaler interface

func (p *Policy) UnmarshalDynamoDBAttributeValue(av awstypes.AttributeValue) error {
	// return attributevalue.Unmarshal(av, p)
	v, ok := av.(*awstypes.AttributeValueMemberN)
	if !ok {
		return fmt.Errorf("unexpected policy value type %T", av)
	}

	switch t := v.Value; t {
	case "1":
		*p = RulePolicyAllowlist
	case "2":
		*p = RulePolicyBlocklist
	case "3":
		*p = RulePolicySilentBlocklist
	case "4":
		*p = RulePolicyRemove
	case "5":
		*p = RulePolicyAllowlistCompiler
	case "6":
		*p = RulePolicyAllowlistTransitive
	case "7":
		*p = RulePolicyCEL
	case "8":
		*p = RulePolicySilentGUIBlocklist
	case "9":
		*p = RulePolicySilentTTYBlocklist
	default:
		return fmt.Errorf("unknown policy value %q", t)
	}

	return nil
}
