package groups

import (
	"fmt"
	"strconv"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	santatypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A registered group may hold a client mode (Entry.ClientMode), sent to its member Macs that have neither a temporary
// client mode nor a client mode of their own; when a Mac's groups hold several, the strictest wins (see
// StrictestClientMode). It takes precedence over the global client mode, so a group can loosen it as well as tighten it.

// StoredClientMode is a group's ClientMode attribute as stored, kept undecoded: a hand-edited value that is not a client
// mode (a string, or a number other than 1 to 3) makes that group count as having no mode, instead of making the whole
// registry, and so every preflight and rule download, fail to decode.
type StoredClientMode struct {
	value types.AttributeValue
}

func (s *StoredClientMode) UnmarshalDynamoDBAttributeValue(value types.AttributeValue) error {
	s.value = value
	return nil
}

func (s StoredClientMode) MarshalDynamoDBAttributeValue() (types.AttributeValue, error) {
	if s.value == nil {
		return &types.AttributeValueMemberNULL{Value: true}, nil
	}
	return s.value, nil
}

// Mode returns the stored client mode, and false when there is none or it cannot be decoded
func (s *StoredClientMode) Mode() (santatypes.ClientMode, bool) {
	if s == nil {
		return 0, false
	}
	n, ok := s.value.(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	mode, err := strconv.Atoi(n.Value)
	if err != nil {
		return 0, false
	}
	if _, known := clientModeStrictness[santatypes.ClientMode(mode)]; !known {
		return 0, false
	}
	return santatypes.ClientMode(mode), true
}

// Stored reports whether the attribute is present, decodable or not
func (s *StoredClientMode) Stored() bool {
	return s != nil && s.value != nil
}

// ClientModeOf returns the group's client mode, and false when it has none (or one that cannot be decoded)
func (e Entry) ClientModeOf() (santatypes.ClientMode, bool) {
	return e.ClientMode.Mode()
}

// clientModeStrictness orders client modes, strictest highest: LOCKDOWN blocks every unknown binary, STANDALONE lets
// the user approve one locally, MONITOR blocks nothing unknown
var clientModeStrictness = map[santatypes.ClientMode]int{
	santatypes.Monitor:    1,
	santatypes.Standalone: 2,
	santatypes.Lockdown:   3,
}

// StricterClientMode reports whether a is stricter than b. Unknown modes are less strict than any known one.
func StricterClientMode(a, b santatypes.ClientMode) bool {
	return clientModeStrictness[a] > clientModeStrictness[b]
}

// HasClientModes reports whether any registered group holds a client mode that can be decoded. Without one, resolving
// a Mac's client mode needs no read of its groups item.
func (r *Registry) HasClientModes() bool {
	for _, entry := range r.Groups {
		if _, ok := entry.ClientModeOf(); ok {
			return true
		}
	}
	return false
}

// StrictestClientMode returns the strictest client mode among the given groups that have one, and the group it comes
// from (between groups with the same mode, the one whose name sorts first). ok is false when none of them has a mode.
func StrictestClientMode(registry *Registry, groupNames []string) (mode santatypes.ClientMode, group string, ok bool) {
	for _, name := range groupNames {
		entry, registered := registry.Groups[name]
		if !registered {
			continue
		}
		m, has := entry.ClientModeOf()
		if !has {
			continue
		}
		if !ok || StricterClientMode(m, mode) || (m == mode && name < group) {
			mode, group, ok = m, name, true
		}
	}
	return mode, group, ok
}

// MachineClientMode returns the client mode a Mac gets from its groups (see Effective and StrictestClientMode), and the
// group it comes from. ok is false when none of its groups has a mode.
func MachineClientMode(registry *Registry, membership *MachineGroups) (mode santatypes.ClientMode, group string, ok bool) {
	return StrictestClientMode(registry, Effective(registry, membership))
}

// ReportedMachineClientMode is MachineClientMode for a Mac that has just reported its groups: its assigned groups,
// read from its groups item, with the reported list judged against the registry rather than the Reported attribute,
// so that joining or leaving a group by reporting it takes effect in the same sync. The groups item is only read when
// some registered group has a client mode, so a fleet without group modes pays nothing for them.
func ReportedMachineClientMode(client dynamodb.GetItemAPI, registry *Registry, machineID string, reported []string) (mode santatypes.ClientMode, group string, ok bool, err error) {
	if !registry.HasClientModes() {
		return 0, "", false, nil
	}
	stored, err := GetMachineGroups(client, machineID, false)
	if err != nil {
		return 0, "", false, err
	}
	membership := &MachineGroups{Assigned: stored.Assigned, Reported: ReportedMembership(registry, reported)}
	mode, group, ok = MachineClientMode(registry, membership)
	return mode, group, ok, nil
}

// SetClientMode stores a group's client mode (or removes it, when mode is nil) and bumps MembershipVersion, in one
// update that requires the group to be registered. The rest of the group's entry is left as it is.
func SetClientMode(client dynamodb.UpdateExpressionAPI, name string, mode *santatypes.ClientMode) error {
	attrs := names("#g", "Groups", "#n", name, "#c", "ClientMode", "#m", "MembershipVersion")
	values := map[string]types.AttributeValue{":one": number(1)}
	update := "REMOVE #g.#n.#c ADD #m :one"
	if mode != nil {
		if _, known := clientModeStrictness[*mode]; !known {
			return fmt.Errorf("unknown client mode %d; must be MONITOR (1), LOCKDOWN (2) or STANDALONE (3)", *mode)
		}
		update = "SET #g.#n.#c = :mode ADD #m :one"
		values[":mode"] = number(int(*mode))
	}
	_, err := client.UpdateItemExpression(registryKey(), update, "attribute_exists(#g.#n)", attrs, values)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrGroupNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to change the group's client mode: %w", err)
	}
	return nil
}
