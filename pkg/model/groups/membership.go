package groups

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The machine's groups item lives in the machine's own partition, next to its sensor data and sync state, so the sync
// Lambdas may write it (Reported) and the CLI writes the other attribute (Assigned).
const (
	machinePartitionPrefix = "Machine#"
	machineGroupsSK        = "Groups"
	machineGroupsDataType  = "MachineGroups"
)

// MachineGroups is a Mac's group membership as stored
type MachineGroups struct {
	// Assigned are the groups assigned in Rudolph (rudolph group add)
	Assigned []string `dynamodbav:"Assigned,stringset,omitempty"`
	// Reported are the registered groups that accept reported membership among those the Mac reported, normalised;
	// written by preflight (see ReportedBasis)
	Reported  []string `dynamodbav:"Reported,stringset,omitempty"`
	UpdatedAt string   `dynamodbav:"UpdatedAt,omitempty"`
}

func machineGroupsKey(machineID string) dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{PartitionKey: machinePartitionPrefix + machineID, SortKey: machineGroupsSK}
}

// GetMachineGroups reads a Mac's groups item; a Mac without one is in no group
func GetMachineGroups(client dynamodb.GetItemAPI, machineID string, consistent bool) (*MachineGroups, error) {
	out, err := client.GetItem(machineGroupsKey(machineID), consistent)
	if err != nil {
		return nil, fmt.Errorf("failed to read the machine's groups: %w", err)
	}
	groups := &MachineGroups{}
	if len(out.Item) > 0 {
		if err := attributevalue.UnmarshalMap(out.Item, groups); err != nil {
			return nil, fmt.Errorf("failed to unmarshal the machine's groups: %w", err)
		}
	}
	return groups, nil
}

// ReportedMembership returns the groups of a reported list that count: normalised, registered and accepting reported
// membership, sorted
func ReportedMembership(registry *Registry, reported []string) []string {
	normalised, _ := NormaliseList(reported)
	var counted []string
	for _, name := range normalised {
		if entry, ok := registry.Groups[name]; ok && entry.AcceptReported {
			counted = append(counted, name)
		}
	}
	return counted
}

// NoReportedBasis is the basis of no reported membership. It is not empty, the value of a sync state that never
// recorded one (expired, or written by a Lambda that predates groups), so that such a sync state makes preflight clear
// a Reported attribute the item may still hold.
const NoReportedBasis = "none"

// ReportedBasis is a short digest of the reported membership stored on the groups item, kept on the sync state so
// that preflight only writes the item when it changes.
func ReportedBasis(reported []string) string {
	if len(reported) == 0 {
		return NoReportedBasis
	}
	sum := sha256.Sum256([]byte(strings.Join(reported, "\n")))
	return hex.EncodeToString(sum[:8])
}

// SetReported stores the Mac's reported membership. Assigned membership is left as it is. No membership removes the
// attribute, and creates no item where there is none (a Mac never in a reported group needs none).
func SetReported(timeProvider clock.TimeProvider, client dynamodb.UpdateExpressionAPI, machineID string, reported []string) error {
	values := map[string]types.AttributeValue{
		":t":  &types.AttributeValueMemberS{Value: clock.RFC3339(timeProvider.Now())},
		":dt": &types.AttributeValueMemberS{Value: machineGroupsDataType},
	}
	update, condition := "SET UpdatedAt = :t, DataType = :dt REMOVE Reported", "attribute_exists(Reported)"
	if len(reported) > 0 {
		update, condition = "SET UpdatedAt = :t, DataType = :dt, Reported = :r", ""
		values[":r"] = &types.AttributeValueMemberSS{Value: reported}
	}
	_, err := client.UpdateItemExpression(machineGroupsKey(machineID), update, condition, nil, values)
	if err != nil && !(condition != "" && dynamodb.IsConditionalCheckFailed(err)) {
		return fmt.Errorf("failed to save the machine's reported groups: %w", err)
	}
	return nil
}

// AddAssigned assigns a registered group to a Mac
func AddAssigned(timeProvider clock.TimeProvider, client RegistryWriter, machineID, group string) error {
	registry, err := GetRegistry(client, true)
	if err != nil {
		return err
	}
	if _, ok := registry.Groups[group]; !ok || group == Ungrouped {
		return ErrGroupNotFound
	}
	return changeAssigned(timeProvider, client, machineID, "ADD", group)
}

// RemoveAssigned removes an assigned group from a Mac. Groups the Mac reports are not affected.
func RemoveAssigned(timeProvider clock.TimeProvider, client RegistryWriter, machineID, group string) error {
	return changeAssigned(timeProvider, client, machineID, "DELETE", group)
}

func changeAssigned(timeProvider clock.TimeProvider, client RegistryWriter, machineID, op, group string) error {
	_, err := client.UpdateItemExpression(machineGroupsKey(machineID),
		"SET UpdatedAt = :t, DataType = :dt "+op+" Assigned :g", "", nil,
		map[string]types.AttributeValue{
			":t":  &types.AttributeValueMemberS{Value: clock.RFC3339(timeProvider.Now())},
			":dt": &types.AttributeValueMemberS{Value: machineGroupsDataType},
			":g":  &types.AttributeValueMemberSS{Value: []string{group}},
		})
	if err != nil {
		return fmt.Errorf("failed to change the machine's assigned groups: %w", err)
	}
	return bumpMembershipVersion(client)
}

// Effective returns the groups whose rules apply to a Mac, sorted: its assigned and reported groups that are
// registered (reported ones only while their group accepts reported membership), or Ungrouped when there are none.
// Ungrouped is returned even when it is not registered, in which case it has no rules.
func Effective(registry *Registry, membership *MachineGroups) []string {
	set := map[string]bool{}
	for _, name := range membership.Assigned {
		if _, ok := registry.Groups[name]; ok && !strings.HasPrefix(name, ReservedPrefix) {
			set[name] = true
		}
	}
	for _, name := range membership.Reported {
		if entry, ok := registry.Groups[name]; ok && entry.AcceptReported && !strings.HasPrefix(name, ReservedPrefix) {
			set[name] = true
		}
	}
	if len(set) == 0 {
		return []string{Ungrouped}
	}
	effective := make([]string, 0, len(set))
	for name := range set {
		effective = append(effective, name)
	}
	sort.Strings(effective)
	return effective
}
