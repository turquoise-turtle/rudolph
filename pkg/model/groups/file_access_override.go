package groups

import (
	"github.com/airbnb/rudolph/pkg/dynamodb"
	santatypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A registered group may hold a file access override (Entry.OverrideFileAccessAction), the override_file_access_action
// sent to its member Macs whose own config row sets none; when a Mac's groups hold several, the most relaxed wins (see
// MostRelaxedFileAccessAction). It takes precedence over the global override, so a group can be held at NONE while the
// fleet audits, as well as backed off while the fleet enforces. It is independent of the group's client mode.

// StoredFileAccessAction is a group's OverrideFileAccessAction attribute as stored, kept undecoded for the same reason
// as StoredClientMode: a hand-edited value must make that group count as having none, not make the registry fail to
// decode.
type StoredFileAccessAction struct {
	value types.AttributeValue
}

func (s *StoredFileAccessAction) UnmarshalDynamoDBAttributeValue(value types.AttributeValue) error {
	s.value = value
	return nil
}

func (s StoredFileAccessAction) MarshalDynamoDBAttributeValue() (types.AttributeValue, error) {
	if s.value == nil {
		return &types.AttributeValueMemberNULL{Value: true}, nil
	}
	return s.value, nil
}

// Action returns the stored file access action as the enum name Santa expects (legacy spellings such as "AuditOnly"
// normalised), and false when there is none or it cannot be decoded
func (s *StoredFileAccessAction) Action() (santatypes.FileAccessAction, bool) {
	if s == nil {
		return "", false
	}
	str, ok := s.value.(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	action, err := santatypes.ParseFileAccessAction(str.Value)
	if err != nil {
		return "", false
	}
	return action, true
}

// Stored reports whether the attribute is present, decodable or not
func (s *StoredFileAccessAction) Stored() bool {
	return s != nil && s.value != nil
}

// FileAccessOverrideOf returns the group's file access override, and false when it has none (or one that cannot be
// decoded)
func (e Entry) FileAccessOverrideOf() (santatypes.FileAccessAction, bool) {
	return e.OverrideFileAccessAction.Action()
}

// HasFileAccessOverrides reports whether any registered group holds a file access override that can be decoded.
// Without one, resolving a Mac's override needs no read of its groups item.
func (r *Registry) HasFileAccessOverrides() bool {
	for _, entry := range r.Groups {
		if _, ok := entry.FileAccessOverrideOf(); ok {
			return true
		}
	}
	return false
}

// fileAccessRelaxation orders file access actions, most relaxed highest: DISABLE takes no action, AUDIT_ONLY only logs
// what would be denied, NONE applies the policies as written
var fileAccessRelaxation = map[santatypes.FileAccessAction]int{
	santatypes.FileAccessActionNone:      1,
	santatypes.FileAccessActionAuditOnly: 2,
	santatypes.FileAccessActionDisable:   3,
}

// MoreRelaxedFileAccessAction reports whether a enforces less than b. Unknown actions are stricter than any known one.
func MoreRelaxedFileAccessAction(a, b santatypes.FileAccessAction) bool {
	return fileAccessRelaxation[a] > fileAccessRelaxation[b]
}

// MostRelaxedFileAccessAction returns the most relaxed file access override among the given groups that have one, and
// the group it comes from (between groups with the same override, the one whose name sorts first). ok is false when
// none of them has an override.
//
// This is deliberately the opposite of StrictestClientMode. The per-group override is the safety valve for backing a
// fleet out of FAA enforcement: putting a group back to audit-only must reach every Mac in it at once, including one
// that is also in a group still enforcing.
func MostRelaxedFileAccessAction(registry *Registry, groupNames []string) (action santatypes.FileAccessAction, group string, ok bool) {
	for _, name := range groupNames {
		entry, registered := registry.Groups[name]
		if !registered {
			continue
		}
		a, has := entry.FileAccessOverrideOf()
		if !has {
			continue
		}
		if !ok || MoreRelaxedFileAccessAction(a, action) || (a == action && name < group) {
			action, group, ok = a, name, true
		}
	}
	return action, group, ok
}

// MachineFileAccessOverride returns the file access override a Mac gets from its groups (see Effective and
// MostRelaxedFileAccessAction), and the group it comes from. ok is false when none of its groups has one.
func MachineFileAccessOverride(registry *Registry, membership *MachineGroups) (action santatypes.FileAccessAction, group string, ok bool) {
	return MostRelaxedFileAccessAction(registry, Effective(registry, membership))
}

// UnreadableFileAccessOverrides returns those of the given groups whose stored override cannot be decoded, and so
// counts as none, for callers that report them
func UnreadableFileAccessOverrides(registry *Registry, groupNames []string) []string {
	var unreadable []string
	for _, name := range groupNames {
		entry, registered := registry.Groups[name]
		if !registered {
			continue
		}
		if _, ok := entry.FileAccessOverrideOf(); !ok && entry.OverrideFileAccessAction.Stored() {
			unreadable = append(unreadable, name)
		}
	}
	return unreadable
}

// SetFileAccessOverride stores a group's file access override as its enum name (or removes it, when action is nil) and
// bumps MembershipVersion, in one update that requires the group to be registered. The rest of the group's entry,
// client mode included, is left as it is.
func SetFileAccessOverride(client dynamodb.UpdateExpressionAPI, name string, action *santatypes.FileAccessAction) error {
	return ChangeSettings(client, name, SettingsChange{FileAccessAction: action, RemoveFileAccessAction: action == nil})
}
