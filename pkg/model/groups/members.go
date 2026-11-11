package groups

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A group's member rows index the groups items by group, so that a group's Macs can be listed with a Query. They
// mirror the groups items exactly, whatever the registry says, and every writer of a groups item changes them in the
// same transaction.
const (
	// GroupMembersPKPrefix starts the partition of a group's member rows: GroupMembers#<group> / <machine id>. The
	// Lambdas' IAM LeadingKeys condition must cover it (preflight writes reported membership).
	GroupMembersPKPrefix  = "GroupMembers#"
	groupMemberDataType   = "GroupMember"
	maxMembershipAttempts = 3
)

// GroupMember is a member row
type GroupMember struct {
	PK        string `dynamodbav:"PK"`
	SK        string `dynamodbav:"SK"`
	Assigned  bool   `dynamodbav:"Assigned,omitempty"`
	Reported  bool   `dynamodbav:"Reported,omitempty"`
	UpdatedAt string `dynamodbav:"UpdatedAt,omitempty"`
	DataType  string `dynamodbav:"DataType,omitempty"`
}

// MemberKey is the key of a Mac's member row in a group
func MemberKey(group, machineID string) dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{PartitionKey: GroupMembersPKPrefix + group, SortKey: machineID}
}

// MemberKeys are the keys of every member row a groups item backs
func MemberKeys(machineID string, membership *MachineGroups) []dynamodb.PrimaryKey {
	var keys []dynamodb.PrimaryKey
	for _, group := range unionOf(membership.Assigned, membership.Reported) {
		keys = append(keys, MemberKey(group, machineID))
	}
	return keys
}

// MembershipWriter is what changing a Mac's groups item, and its member rows with it, needs
type MembershipWriter interface {
	dynamodb.GetItemAPI
	dynamodb.TransactWriteItemsAPI
	dynamodb.TransactUpdateExpressionAPI
}

// ErrMembershipChanged is returned when a Mac's groups item changed between being read and being written
var ErrMembershipChanged = errors.New("the machine's groups changed while they were being updated; try again")

type memberFlags struct{ assigned, reported bool }

func flagsOf(assigned, reported []string) map[string]memberFlags {
	flags := map[string]memberFlags{}
	for _, g := range assigned {
		f := flags[g]
		f.assigned = true
		flags[g] = f
	}
	for _, g := range reported {
		f := flags[g]
		f.reported = true
		flags[g] = f
	}
	return flags
}

func unionOf(a, b []string) []string {
	set := map[string]bool{}
	for _, g := range a {
		set[g] = true
	}
	for _, g := range b {
		set[g] = true
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// memberRowWrite returns the transaction item that makes a member row match flags: deleted when the Mac is neither
// assigned nor reports the group, otherwise created or updated with exactly those flags
func memberRowWrite(client dynamodb.TransactWriteItemsAPI, update dynamodb.TransactUpdateExpressionAPI, group, machineID string, f memberFlags, now string) (*types.TransactWriteItem, error) {
	key := MemberKey(group, machineID)
	if !f.assigned && !f.reported {
		return client.CreateTransactDeleteItem(key)
	}
	values := map[string]types.AttributeValue{
		":t":  &types.AttributeValueMemberS{Value: now},
		":dt": &types.AttributeValueMemberS{Value: groupMemberDataType},
	}
	set, remove := "SET UpdatedAt = :t, DataType = :dt", ""
	for _, flag := range []struct {
		name string
		on   bool
	}{{"Assigned", f.assigned}, {"Reported", f.reported}} {
		if flag.on {
			set += ", " + flag.name + " = :true"
			values[":true"] = &types.AttributeValueMemberBOOL{Value: true}
		} else if remove == "" {
			remove = " REMOVE " + flag.name
		} else {
			remove += ", " + flag.name
		}
	}
	return update.CreateTransactUpdateExpression(key, set+remove, "", nil, values)
}

// unchangedCondition holds while the groups item still has the Assigned and Reported sets it was read with. A set
// equality, not UpdatedAt, which has a resolution of one second.
func unchangedCondition(read *MachineGroups, values map[string]types.AttributeValue) string {
	condition := ""
	for _, attr := range []struct {
		name, placeholder string
		set               []string
	}{{"Assigned", ":oldAssigned", read.Assigned}, {"Reported", ":oldReported", read.Reported}} {
		part := "attribute_not_exists(" + attr.name + ")"
		if len(attr.set) > 0 {
			part = attr.name + " = " + attr.placeholder
			values[attr.placeholder] = &types.AttributeValueMemberSS{Value: attr.set}
		}
		if condition != "" {
			condition += " AND "
		}
		condition += part
	}
	return condition
}

// writeMembership writes the groups item update (update, with its values) conditioned on the item being as read, and
// the member rows of the groups in touched whose flags differ between read and the new sets, or every one of touched
// when always is set, in one transaction. A group appears in it once: the groups item and the rows are distinct items,
// and at most MaxGroups+1 rows change, within the transaction limit of 100 items.
func writeMembership(timeProvider clock.TimeProvider, client MembershipWriter, machineID string, read *MachineGroups, update string, values map[string]types.AttributeValue, newAssigned, newReported []string, always []string) error {
	now := clock.RFC3339(timeProvider.Now())
	values[":t"] = &types.AttributeValueMemberS{Value: now}
	values[":dt"] = &types.AttributeValueMemberS{Value: machineGroupsDataType}
	condition := unchangedCondition(read, values)
	groupsItem, err := client.CreateTransactUpdateExpression(machineGroupsKey(machineID), update, condition, nil, values)
	if err != nil {
		return err
	}
	items := []types.TransactWriteItem{*groupsItem}
	before, after := flagsOf(read.Assigned, read.Reported), flagsOf(newAssigned, newReported)
	forced := map[string]bool{}
	for _, g := range always {
		forced[g] = true
	}
	for _, group := range unionOf(unionOf(read.Assigned, read.Reported), unionOf(unionOf(newAssigned, newReported), always)) {
		if before[group] == after[group] && !forced[group] {
			continue
		}
		item, err := memberRowWrite(client, client, group, machineID, after[group], now)
		if err != nil {
			return err
		}
		items = append(items, *item)
	}
	if _, err := client.TransactWriteItems(items, nil); err != nil {
		if dynamodb.IsTransactionConditionalCheckFailed(err) {
			return ErrMembershipChanged
		}
		return err
	}
	return nil
}

// SetReported stores the Mac's reported membership, with its member rows, in one transaction. Assigned membership is
// left as it is. No membership removes the attribute, and creates no item where there is none (a Mac never in a
// reported group needs none). It reads the groups item first, strongly consistent, to know which rows change, and the
// write fails with ErrMembershipChanged if the item changed in the meantime.
func SetReported(timeProvider clock.TimeProvider, client MembershipWriter, machineID string, reported []string) error {
	read, err := GetMachineGroups(client, machineID, true)
	if err != nil {
		return err
	}
	if sameSet(read.Reported, reported) {
		return nil
	}
	values := map[string]types.AttributeValue{}
	update := "SET UpdatedAt = :t, DataType = :dt REMOVE Reported"
	if len(reported) > 0 {
		update = "SET UpdatedAt = :t, DataType = :dt, Reported = :r"
		values[":r"] = &types.AttributeValueMemberSS{Value: reported}
	}
	if err := writeMembership(timeProvider, client, machineID, read, update, values, read.Assigned, reported, nil); err != nil {
		return fmt.Errorf("failed to save the machine's reported groups: %w", err)
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, g := range a {
		set[g] = true
	}
	for _, g := range b {
		if !set[g] {
			return false
		}
	}
	return true
}

func withGroup(set []string, group string) []string {
	return unionOf(set, []string{group})
}

func withoutGroup(set []string, group string) []string {
	var out []string
	for _, g := range set {
		if g != group {
			out = append(out, g)
		}
	}
	return out
}

// ListMembers returns a group's member rows, by machine ID: a paged, strongly consistent Query of its partition
func ListMembers(client dynamodb.QueryAPI, group string) ([]GroupMember, error) {
	var members []GroupMember
	var start map[string]types.AttributeValue
	for {
		out, err := client.Query(&awsdynamodb.QueryInput{
			KeyConditionExpression:    aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: GroupMembersPKPrefix + group}},
			ConsistentRead:            aws.Bool(true),
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list the members of group %q: %w", group, err)
		}
		var page []GroupMember
		if err := attributevalue.UnmarshalListOfMaps(out.Items, &page); err != nil {
			return nil, fmt.Errorf("failed to unmarshal the members of group %q: %w", group, err)
		}
		members = append(members, page...)
		if len(out.LastEvaluatedKey) == 0 {
			return members, nil
		}
		start = out.LastEvaluatedKey
	}
}

// MembersRebuild is what rebuilding the member rows from the groups items changes
type MembersRebuild struct {
	// Macs is the number of groups items read, Rows the number of member rows read
	Macs, Rows int
	// Writes are the rows that are missing or wrong, as they should be; Deletes the keys of rows no groups item backs
	Writes  []GroupMember
	Deletes []dynamodb.PrimaryKey
}

// Empty says whether every member row is right
func (r *MembersRebuild) Empty() bool {
	return len(r.Writes) == 0 && len(r.Deletes) == 0
}

// PlanMembersRebuild reads every groups item and every member row (one paged Scan of the table) and returns what makes
// the rows mirror the groups items
func PlanMembersRebuild(timeProvider clock.TimeProvider, client dynamodb.ScanAPI) (*MembersRebuild, error) {
	want := map[dynamodb.PrimaryKey]memberFlags{}
	have := map[dynamodb.PrimaryKey]GroupMember{}
	plan := &MembersRebuild{}
	var start map[string]types.AttributeValue
	for {
		out, err := client.Scan(&awsdynamodb.ScanInput{
			FilterExpression: aws.String("(begins_with(PK, :machine) AND SK = :groups) OR begins_with(PK, :members)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":machine": &types.AttributeValueMemberS{Value: machinePartitionPrefix},
				":groups":  &types.AttributeValueMemberS{Value: machineGroupsSK},
				":members": &types.AttributeValueMemberS{Value: GroupMembersPKPrefix},
			},
			ConsistentRead:    aws.Bool(true),
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to scan the groups items and member rows: %w", err)
		}
		for _, item := range out.Items {
			pk, _ := item["PK"].(*types.AttributeValueMemberS)
			if pk == nil {
				continue
			}
			if strings.HasPrefix(pk.Value, GroupMembersPKPrefix) {
				var row GroupMember
				if err := attributevalue.UnmarshalMap(item, &row); err != nil {
					return nil, fmt.Errorf("failed to unmarshal the member row %s: %w", pk.Value, err)
				}
				have[dynamodb.PrimaryKey{PartitionKey: row.PK, SortKey: row.SK}] = row
				plan.Rows++
				continue
			}
			var membership MachineGroups
			if err := attributevalue.UnmarshalMap(item, &membership); err != nil {
				return nil, fmt.Errorf("failed to unmarshal the groups item %s: %w", pk.Value, err)
			}
			plan.Macs++
			machineID := strings.TrimPrefix(pk.Value, machinePartitionPrefix)
			for group, f := range flagsOf(membership.Assigned, membership.Reported) {
				want[MemberKey(group, machineID)] = f
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		start = out.LastEvaluatedKey
	}

	now := clock.RFC3339(timeProvider.Now())
	for key, f := range want {
		row, ok := have[key]
		if ok && row.Assigned == f.assigned && row.Reported == f.reported && row.DataType == groupMemberDataType {
			continue
		}
		plan.Writes = append(plan.Writes, GroupMember{PK: key.PartitionKey, SK: key.SortKey, Assigned: f.assigned, Reported: f.reported, UpdatedAt: now, DataType: groupMemberDataType})
	}
	for key := range have {
		if _, ok := want[key]; !ok {
			plan.Deletes = append(plan.Deletes, key)
		}
	}
	sort.Slice(plan.Writes, func(i, j int) bool {
		return plan.Writes[i].PK+"\x00"+plan.Writes[i].SK < plan.Writes[j].PK+"\x00"+plan.Writes[j].SK
	})
	sort.Slice(plan.Deletes, func(i, j int) bool {
		return plan.Deletes[i].PartitionKey+"\x00"+plan.Deletes[i].SortKey < plan.Deletes[j].PartitionKey+"\x00"+plan.Deletes[j].SortKey
	})
	return plan, nil
}

// Apply writes the rebuild. The writes are batched, not a transaction: a groups item changed while the rebuild runs
// can leave its rows wrong, which running the rebuild again fixes.
func (r *MembersRebuild) Apply(client dynamodb.BatchWriteItemAPI) error {
	puts := make([]interface{}, 0, len(r.Writes))
	for _, row := range r.Writes {
		puts = append(puts, row)
	}
	if err := client.BatchWriteItems(puts, r.Deletes); err != nil {
		return fmt.Errorf("failed to write the member rows: %w", err)
	}
	return nil
}
