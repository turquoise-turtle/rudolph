package machine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spf13/cobra"
)

func init() {
	flags.AddYesFlag(MachineCmd)
	flags.AddDryRunFlag(MachineCmd)

	var machineID string
	forgetCmd := &cobra.Command{
		Use:   "forget -m <machine-id> [--yes|--dry-run]",
		Short: "Delete everything Rudolph keeps in the store table for a retired Mac",
		Long: `Deletes everything Rudolph keeps in the store table for one Mac: what it reported (sensor data), its sync state,
its configuration, its group membership (with its rows in the group member index), its machine rules and their
delivery tracking, and its entries in the index of rules whose allowed events are collected. Group registrations,
global rules and group rules are not changed. Events in the events table are not deleted; they expire on their own. If
the Mac syncs again, it starts afresh, as a Mac Rudolph has never seen. Forget a Mac after it stops syncing: what a sync writes after the list is made is not
deleted. Running forget again lists and deletes whatever is left.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := Client(cmd)
			if err != nil {
				return err
			}
			w, ok := client.(writer)
			if !ok {
				return errors.New("this DynamoDB client cannot delete a machine's items")
			}
			id, err := flags.ResolveMachineID(client, machineID)
			if err != nil {
				return err
			}
			plan, err := planForget(client, id)
			if err != nil {
				return err
			}
			if plan.empty() {
				return fmt.Errorf("nothing is stored for machine ID %s (in any case); nothing to forget", machineID)
			}
			plan.print()
			apply, err := flags.ApprovalOf(cmd).Confirm()
			if err != nil || !apply {
				return err
			}
			serial := ""
			if plan.sensor != nil && plan.sensor.SerialNum != "" {
				serial = ", serial " + plan.sensor.SerialNum
			}
			if changed, err := plan.apply(w); err != nil {
				if !changed {
					return err
				}
				return flags.RecordPartial(client, "machine.forget", flags.MachineTarget(id), "forgetting the machine"+serial, err)
			}
			fmt.Printf("Forgot %s: deleted %d items. If the Mac syncs again, it starts afresh.\n", id, len(plan.keys()))
			return flags.Record(client, "machine.forget", flags.MachineTarget(id), fmt.Sprintf("forgot the machine: deleted %d items%s", len(plan.keys()), serial))
		},
	}
	forgetCmd.Flags().StringVarP(&machineID, "machine", "m", "", "The Mac's machine UUID")
	_ = forgetCmd.MarkFlagRequired("machine")
	MachineCmd.AddCommand(forgetCmd)
}

// writer is what forgetting a Mac writes with
type writer interface {
	groups.RegistryWriter
	dynamodb.BatchWriteItemAPI
}

// forgetPlan is what Rudolph keeps for one Mac in the store table
type forgetPlan struct {
	machineID string
	sensor    *sensordata.SensorData
	// machineItems are the keys of the Mac's own partition (Machine#<id>), by sort key
	machineItems []dynamodb.PrimaryKey
	rules        []dynamodb.PrimaryKey
	deliveries   []dynamodb.PrimaryKey
	assigned     []string
	reported     []string
	// members are the Mac's rows in the member index of each group it is in (GroupMembers#<group>)
	members []dynamodb.PrimaryKey
	// collected is the number of the Mac's machine rules in the event collection index
	collected int
}

func planForget(client dynamodb.DynamoDBClient, machineID string) (*forgetPlan, error) {
	p := &forgetPlan{machineID: machineID}
	var err error
	if p.sensor, err = sensordata.GetSensorData(client, machineID); err != nil {
		return nil, fmt.Errorf("failed to read the machine's sensor data: %w", err)
	}
	// Every partition key below is built by the package that owns it. The sensor data, sync state, configuration and
	// groups items share the Mac's partition, so one Query finds them, and any item added there later.
	partitions := []struct {
		pk   string
		keys *[]dynamodb.PrimaryKey
	}{
		{sensordata.SensorDataPK(machineID), &p.machineItems},
		{machinerules.MachineRulesPK(machineID), &p.rules},
		{machinerules.DeliveriesPK(machineID), &p.deliveries},
	}
	for _, part := range partitions {
		if *part.keys, err = partitionKeys(client, part.pk); err != nil {
			return nil, err
		}
	}
	membership, err := groups.GetMachineGroups(client, machineID, true)
	if err != nil {
		return nil, err
	}
	p.assigned, p.reported = membership.Assigned, membership.Reported
	p.members = groups.MemberKeys(machineID, membership)
	index, err := eventcollection.ReadIndex(client)
	if err != nil {
		return nil, err
	}
	p.collected = len(index.Machines[machineID])
	return p, nil
}

// partitionKeys returns the keys of every item in a partition: a paged Query, strongly consistent, never a Scan
func partitionKeys(client dynamodb.QueryAPI, pk string) ([]dynamodb.PrimaryKey, error) {
	var keys []dynamodb.PrimaryKey
	var start map[string]awstypes.AttributeValue
	for {
		out, err := client.Query(&awsdynamodb.QueryInput{
			KeyConditionExpression:    aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]awstypes.AttributeValue{":pk": &awstypes.AttributeValueMemberS{Value: pk}},
			ProjectionExpression:      aws.String("PK, SK"),
			ConsistentRead:            aws.Bool(true),
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list the items under %s: %w", pk, err)
		}
		for _, item := range out.Items {
			sk, _ := item["SK"].(*awstypes.AttributeValueMemberS)
			if sk != nil {
				keys = append(keys, dynamodb.PrimaryKey{PartitionKey: pk, SortKey: sk.Value})
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return keys, nil
		}
		start = out.LastEvaluatedKey
	}
}

func (p *forgetPlan) keys() []dynamodb.PrimaryKey {
	keys := append([]dynamodb.PrimaryKey{}, p.machineItems...)
	keys = append(keys, p.rules...)
	keys = append(keys, p.deliveries...)
	return append(keys, p.members...)
}

func (p *forgetPlan) hasGroupsItem() bool {
	groupsSK := groups.MachineGroupsKey("").SortKey
	for _, key := range p.machineItems {
		if key.SortKey == groupsSK {
			return true
		}
	}
	return false
}

func (p *forgetPlan) empty() bool {
	return len(p.keys()) == 0 && p.collected == 0
}

// machineItemKinds names the items of the Mac's own partition, by sort key (the same for every Mac)
func machineItemKinds() []struct{ sk, name string } {
	_, sensorSK := sensordata.MachineIDSensorDataPKSK("")
	return []struct{ sk, name string }{
		{sensorSK, "sensor data (its last report)"},
		{syncstate.Key("").SortKey, "sync state"},
		{machineconfiguration.MachineConfigurationKey("").SortKey, "configuration"},
		{groups.MachineGroupsKey("").SortKey, "group membership"},
	}
}

func (p *forgetPlan) print() {
	fmt.Printf("Forgetting machine %s", p.machineID)
	if p.sensor != nil {
		fmt.Printf(" (hostname %s, serial number %s)", orNone(p.sensor.Hostname), orNone(p.sensor.SerialNum))
	}
	fmt.Println(". This deletes:")
	known := map[string]bool{}
	for _, kind := range machineItemKinds() {
		known[kind.sk] = true
		for _, key := range p.machineItems {
			if key.SortKey == kind.sk {
				fmt.Printf("  %-34s 1 item\n", kind.name+":")
			}
		}
	}
	var others []string
	for _, key := range p.machineItems {
		if !known[key.SortKey] {
			others = append(others, key.SortKey)
		}
	}
	if len(others) > 0 {
		fmt.Printf("  %-34s %s (%s)\n", "other items of the Mac:", items(len(others)), strings.Join(others, ", "))
	}
	if len(p.assigned) > 0 || len(p.reported) > 0 {
		fmt.Printf("  %-34s assigned %s; reported %s\n", "groups it leaves:", list(p.assigned), list(p.reported))
		fmt.Printf("  %-34s %s\n", "group member rows:", items(len(p.members)))
	}
	fmt.Printf("  %-34s %s\n", "machine rules:", items(len(p.rules)))
	fmt.Printf("  %-34s %s\n", "machine rule delivery tracking:", items(len(p.deliveries)))
	if p.collected > 0 {
		fmt.Printf("  %-34s %d flagged machine rule(s) (an attribute of the index item)\n", "event collection index entries:", p.collected)
	}
	fmt.Println("Group registrations, global rules and group rules are not changed. Its events in the events table are not")
	fmt.Println("deleted; they expire on their own. If the Mac syncs again, it starts afresh, as a Mac Rudolph has never seen.")
}

func items(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}

// apply deletes what the plan lists. Assigned groups are removed with the writer `rudolph group remove` uses, which
// records the membership change for readers that cache membership and keeps the member rows in step; the member rows
// left (those of reported groups) are deleted with the Mac's items; deleting the Groups row records it too, since
// reported groups alone also make the Mac a member. A run that fails part-way can be run again: it lists what is
// left. On an error, changed says whether anything may have been changed before it.
func (p *forgetPlan) apply(client writer) (changed bool, err error) {
	timeProvider := clock.ConcreteTimeProvider{}
	for _, group := range p.assigned {
		if err := groups.RemoveAssigned(timeProvider, client, p.machineID, group); err != nil {
			return changed, fmt.Errorf("group %q: %w", group, err)
		}
		changed = true
	}
	if p.collected > 0 {
		if err := eventcollection.ForgetMachine(client, p.machineID); err != nil {
			return changed, err
		}
		changed = true
	}
	keys := p.keys()
	if err := client.BatchWriteItems(nil, keys); err != nil {
		// The deletes are sent 25 at a time, so a failure after the first batch leaves some items deleted
		return changed || len(keys) > 25, fmt.Errorf("failed to delete the machine's items: %w", err)
	}
	if p.hasGroupsItem() {
		if err := groups.MembershipChanged(client); err != nil {
			return true, err
		}
	}
	return true, nil
}
