package syncstate

import (
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// MachineRuleKeys is a set of machine-specific rules a Mac holds (or is being sent), identified by their rule sort key
// (e.g. "Binary#<sha256>", see rules.RuleSortKeyFromTypeIdentifier).
//
// The set can be unknown: sync state rows written before Rudolph tracked machine rule delivery have no such
// attribute. An unknown set is stored as NULL (or is absent); a known set, even an empty one, is stored as a list of
// strings, so that "nothing delivered" and "not tracked yet" are never confused.
type MachineRuleKeys struct {
	known bool
	keys  []string
}

// NewMachineRuleKeys returns a known set holding the given keys.
func NewMachineRuleKeys(keys ...string) MachineRuleKeys {
	set := MachineRuleKeys{known: true}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !seen[key] {
			seen[key] = true
			set.keys = append(set.keys, key)
		}
	}
	sort.Strings(set.keys)
	return set
}

// Known reports whether the set is tracked. Rows written by older versions of Rudolph have unknown sets.
func (m MachineRuleKeys) Known() bool {
	return m.known
}

// Keys returns the keys in the set, sorted. It is empty for an unknown set.
func (m MachineRuleKeys) Keys() []string {
	return append([]string(nil), m.keys...)
}

// Equal reports whether both sets are known and hold the same keys.
func (m MachineRuleKeys) Equal(other MachineRuleKeys) bool {
	if !m.known || !other.known || len(m.keys) != len(other.keys) {
		return false
	}
	for i := range m.keys {
		if m.keys[i] != other.keys[i] {
			return false
		}
	}
	return true
}

// Union returns the keys in either set. It is unknown if either set is.
func (m MachineRuleKeys) Union(other MachineRuleKeys) MachineRuleKeys {
	if !m.known || !other.known {
		return MachineRuleKeys{}
	}
	return NewMachineRuleKeys(append(m.Keys(), other.keys...)...)
}

// MarshalDynamoDBAttributeValue stores a known set as a list of strings and an unknown set as NULL.
func (m MachineRuleKeys) MarshalDynamoDBAttributeValue() (types.AttributeValue, error) {
	if !m.known {
		return &types.AttributeValueMemberNULL{Value: true}, nil
	}
	list := make([]types.AttributeValue, len(m.keys))
	for i, key := range m.keys {
		list[i] = &types.AttributeValueMemberS{Value: key}
	}
	return &types.AttributeValueMemberL{Value: list}, nil
}

// UnmarshalDynamoDBAttributeValue reads a set written by MarshalDynamoDBAttributeValue.
func (m *MachineRuleKeys) UnmarshalDynamoDBAttributeValue(av types.AttributeValue) error {
	switch v := av.(type) {
	case nil, *types.AttributeValueMemberNULL:
		*m = MachineRuleKeys{}
	case *types.AttributeValueMemberL:
		keys := make([]string, 0, len(v.Value))
		for _, item := range v.Value {
			s, ok := item.(*types.AttributeValueMemberS)
			if !ok {
				return fmt.Errorf("machine rule key set holds a %T, want strings", item)
			}
			keys = append(keys, s.Value)
		}
		*m = NewMachineRuleKeys(keys...)
	case *types.AttributeValueMemberSS:
		*m = NewMachineRuleKeys(v.Value...)
	default:
		return fmt.Errorf("machine rule key set is stored as a %T, want a list of strings", av)
	}
	return nil
}
