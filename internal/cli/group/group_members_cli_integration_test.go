//go:build integration

package group

// Group members (owner approved 2026-11-11; docs/groups-design.md, "Group members"). A Mac's groups live on its own
// item (`Machine#<id>` / `Groups`: Assigned and Reported string sets), so listing a group's Macs needs an index of them:
//
//	`GroupMembers#<group>` / `<machine id>`: Assigned (BOOL true, absent otherwise), Reported (BOOL true, absent
//	otherwise), UpdatedAt, DataType "GroupMember"
//
// The rows mirror the groups items exactly, whatever the registry says: a row exists for each group in a Mac's Assigned
// or Reported set, with the matching flags, and no other row exists. Every writer of a groups item changes the rows in
// the same transaction, so a failed write changes neither: `group add|remove`, preflight (reported membership, tested in
// internal/handlers/group_members_integration_test.go) and rudolph-web. `machine forget` deletes the Mac's rows with its
// groups item. Deleting a group keeps the rows, as it keeps Assigned (they count again if the group is created again).
//
//	rudolph group members <group>                        Macs in a registered group, by machine ID, each marked
//	                                                     "assigned", "reported" or "assigned, reported"; Macs that only
//	                                                     report a group that counts only assigned Macs are left out
//	rudolph group members --rebuild [--yes|--dry-run]    rebuilds every row from the groups items (a Scan): writes
//	                                                     missing or wrong rows, deletes rows no groups item backs;
//	                                                     audited as "group.members.rebuild", target "groups"
//
// `group members` of an unregistered group or of the Macs in no group (rudolph:ungrouped) is refused.

import (
	"context"
	"strings"
	"testing"
	"time"

	machinecli "github.com/airbnb/rudolph/internal/cli/machine"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	machineB = "BBBBBBBB-2222-4222-8222-BBBBBBBBBBBB"
	machineC = "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"
)

func put(t *testing.T, row map[string]any) {
	t.Helper()
	av, err := attributevalue.MarshalMap(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: av}); err != nil {
		t.Fatal(err)
	}
}

func putGroupsItem(t *testing.T, machineID string, assigned, reported []string) {
	t.Helper()
	row := map[string]awstypes.AttributeValue{
		"PK":        &awstypes.AttributeValueMemberS{Value: "Machine#" + machineID},
		"SK":        &awstypes.AttributeValueMemberS{Value: "Groups"},
		"DataType":  &awstypes.AttributeValueMemberS{Value: "MachineGroups"},
		"UpdatedAt": &awstypes.AttributeValueMemberS{Value: "2026-11-11T00:00:00Z"},
	}
	if len(assigned) > 0 {
		row["Assigned"] = &awstypes.AttributeValueMemberSS{Value: assigned}
	}
	if len(reported) > 0 {
		row["Reported"] = &awstypes.AttributeValueMemberSS{Value: reported}
	}
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: row}); err != nil {
		t.Fatal(err)
	}
}

func putMemberRow(t *testing.T, group, machineID string, assigned, reported bool) {
	t.Helper()
	row := map[string]any{"PK": "GroupMembers#" + group, "SK": machineID, "DataType": "GroupMember", "UpdatedAt": "2026-11-11T00:00:00Z"}
	if assigned {
		row["Assigned"] = true
	}
	if reported {
		row["Reported"] = true
	}
	put(t, row)
}

type memberFlags struct{ exists, assigned, reported bool }

func memberRow(t *testing.T, group, machineID string) memberFlags {
	t.Helper()
	row := item(t, "GroupMembers#"+group, machineID)
	if len(row) == 0 {
		return memberFlags{}
	}
	f := memberFlags{exists: true}
	if b, ok := row["Assigned"].(*awstypes.AttributeValueMemberBOOL); ok {
		f.assigned = b.Value
	}
	if b, ok := row["Reported"].(*awstypes.AttributeValueMemberBOOL); ok {
		f.reported = b.Value
	}
	if dt, _ := row["DataType"].(*awstypes.AttributeValueMemberS); dt == nil || dt.Value != "GroupMember" {
		t.Errorf("GroupMembers#%s/%s: DataType %v, want GroupMember", group, machineID, row["DataType"])
	}
	return f
}

// lineOf returns the output line that mentions the machine, or "".
func lineOf(out, machineID string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, machineID) {
			return l
		}
	}
	return ""
}

func TestGroupMembersCLI_AssignAndRemoveKeepTheRowsInStep(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustGroup(t, "add", "--machine", machine, "eng")
	if got := memberRow(t, "eng", machine); got != (memberFlags{exists: true, assigned: true}) {
		t.Fatalf("after group add: GroupMembers#eng/%s = %+v, want Assigned only", machine, got)
	}
	out := mustGroup(t, "members", "eng")
	if line := lineOf(out, machine); line == "" || !strings.Contains(line, "assigned") || strings.Contains(line, "reported") {
		t.Errorf("group members eng: the Mac's line %q, want it marked assigned\n%s", line, out)
	}

	// A Mac that also reports the group keeps its row when the assignment is removed
	putGroupsItem(t, machine, []string{"eng"}, []string{"eng"})
	putMemberRow(t, "eng", machine, true, true)
	mustGroup(t, "remove", "--machine", machine, "eng")
	if got := memberRow(t, "eng", machine); got != (memberFlags{exists: true, reported: true}) {
		t.Errorf("after removing the assignment of a Mac that reports eng: %+v, want Reported only", got)
	}

	putGroupsItem(t, machine, []string{"eng"}, nil)
	putMemberRow(t, "eng", machine, true, false)
	mustGroup(t, "remove", "--machine", machine, "eng")
	if got := memberRow(t, "eng", machine); got.exists {
		t.Errorf("after removing the only membership: the row is still there: %+v", got)
	}
}

// The groups item and the member row change together or not at all.
func TestGroupMembersCLI_AFailedMemberWriteChangesNothing(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op != "GetItem" && c.Op != "Query" && c.Op != "Scan" && c.Op != "BatchGetItem" && strings.Contains(c.InputJSON(), "GroupMembers#") {
			f := ddblocal.FaultAccessDenied
			return &f
		}
		return nil
	}})
	if out, err := group(t, "add", "--machine", machine, "eng"); err == nil {
		t.Fatalf("group add succeeded although the member row could not be written:\n%s", out)
	}
	if assigned := item(t, "Machine#"+machine, "Groups")["Assigned"]; assigned != nil {
		t.Errorf("the groups item was changed although the member row was not: Assigned %v", assigned)
	}
}

func TestGroupMembersCLI_ListMarksHowAndHonoursNoReported(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	putGroupsItem(t, machine, []string{"eng"}, nil)
	putMemberRow(t, "eng", machine, true, false)
	putGroupsItem(t, machineB, nil, []string{"eng"})
	putMemberRow(t, "eng", machineB, false, true)
	putGroupsItem(t, machineC, []string{"eng"}, []string{"eng"})
	putMemberRow(t, "eng", machineC, true, true)

	out := mustGroup(t, "members", "eng")
	if line := lineOf(out, machine); !strings.Contains(line, "assigned") || strings.Contains(line, "reported") {
		t.Errorf("assigned Mac: %q\n%s", line, out)
	}
	if line := lineOf(out, machineB); !strings.Contains(line, "reported") || strings.Contains(line, "assigned") {
		t.Errorf("reporting Mac: %q\n%s", line, out)
	}
	if line := lineOf(out, machineC); !strings.Contains(line, "assigned") || !strings.Contains(line, "reported") {
		t.Errorf("assigned and reporting Mac: %q\n%s", line, out)
	}
	if strings.Index(out, machine) > strings.Index(out, machineB) || strings.Index(out, machineB) > strings.Index(out, machineC) {
		t.Errorf("members are not listed by machine ID:\n%s", out)
	}

	mustGroup(t, "update", "eng", "--no-reported")
	out = mustGroup(t, "members", "eng")
	if lineOf(out, machineB) != "" {
		t.Errorf("a Mac that only reports a group counting only assigned Macs is listed:\n%s", out)
	}
	if lineOf(out, machine) == "" || lineOf(out, machineC) == "" {
		t.Errorf("assigned Macs are missing once the group counts only assigned Macs:\n%s", out)
	}
}

func TestGroupMembersCLI_Refused(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	for _, args := range [][]string{
		{"members", "nope"},
		{"members", "rudolph:ungrouped"},
		{"members"},
		{"members", "eng", "--rebuild"},
	} {
		if out, err := group(t, args...); err == nil {
			t.Errorf("group %v was accepted:\n%s", args, out)
		}
	}
}

func TestGroupMembersCLI_DeletingAGroupKeepsTheRows(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustGroup(t, "add", "--machine", machine, "eng")
	mustGroup(t, "delete", "eng")
	if got := memberRow(t, "eng", machine); got != (memberFlags{exists: true, assigned: true}) {
		t.Errorf("after deleting the group: %+v, want the Assigned row kept", got)
	}
	if out, err := group(t, "members", "eng"); err == nil {
		t.Errorf("group members of a deleted group was accepted:\n%s", out)
	}
	mustGroup(t, "create", "eng")
	if out := mustGroup(t, "members", "eng"); lineOf(out, machine) == "" {
		t.Errorf("the assigned Mac is not listed once the group is created again:\n%s", out)
	}
}

func TestGroupMembersCLI_ForgetDeletesTheMacsRows(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustGroup(t, "create", "ops")
	mustGroup(t, "add", "--machine", machine, "eng")
	putGroupsItem(t, machine, []string{"eng"}, []string{"ops"})
	putMemberRow(t, "ops", machine, false, true)
	mustGroup(t, "add", "--machine", machineB, "eng")

	if out, err := clitest.Run(t, env, machinecli.MachineCmd, "yes\n", "forget", "-m", machine); err != nil {
		t.Fatalf("machine forget: %v\n%s", err, out)
	}
	if got := memberRow(t, "eng", machine); got.exists {
		t.Errorf("forget left GroupMembers#eng/%s", machine)
	}
	if got := memberRow(t, "ops", machine); got.exists {
		t.Errorf("forget left GroupMembers#ops/%s", machine)
	}
	if got := memberRow(t, "eng", machineB); !got.exists {
		t.Error("forget deleted another Mac's row")
	}
}

func TestGroupMembersCLI_Rebuild(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustGroup(t, "create", "ops")
	putGroupsItem(t, machine, []string{"eng"}, []string{"eng", "ops"})
	putGroupsItem(t, machineB, nil, []string{"ops"})
	putGroupsItem(t, machineC, []string{"retired"}, nil)                        // a deleted group: mirrored all the same
	putMemberRow(t, "eng", machine, true, false)                                // wrong flags
	putMemberRow(t, "eng", machineB, false, true)                               // no longer backed
	putMemberRow(t, "ops", "DDDDDDDD-4444-4444-8444-DDDDDDDDDDDD", true, false) // a Mac with no groups item
	put(t, map[string]any{"PK": "Machine#" + machineB, "SK": "SyncState", "DataType": "SyncState"})

	if _, err := group(t, "members", "--rebuild", "--dry-run"); err != nil {
		t.Fatalf("--rebuild --dry-run: %v", err)
	}
	if got := memberRow(t, "eng", machineB); !got.exists {
		t.Fatal("--rebuild --dry-run changed a row")
	}
	if got := memberRow(t, "ops", machine); got.exists {
		t.Fatal("--rebuild --dry-run wrote a row")
	}

	if out, err := group(t, "members", "--rebuild"); err != nil {
		t.Fatalf("--rebuild: %v\n%s", err, out)
	}
	want := map[[2]string]memberFlags{
		{"eng", machine}:      {exists: true, assigned: true, reported: true},
		{"ops", machine}:      {exists: true, reported: true},
		{"ops", machineB}:     {exists: true, reported: true},
		{"retired", machineC}: {exists: true, assigned: true},
		{"eng", machineB}:     {},
		{"ops", "DDDDDDDD-4444-4444-8444-DDDDDDDDDDDD"}: {},
	}
	for k, w := range want {
		if got := memberRow(t, k[0], k[1]); got != w {
			t.Errorf("after --rebuild: GroupMembers#%s/%s = %+v, want %+v", k[0], k[1], got, w)
		}
	}

	all, err := modelaudit.List(ddblocal.Client(t), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range all {
		if e.Action == "group.members.rebuild" && e.Target == "groups" {
			found = true
		}
	}
	if !found {
		t.Errorf("no group.members.rebuild audit entry with target groups: %+v", all)
	}

	// Running it again writes no member row
	var memberWrites int
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op != "GetItem" && c.Op != "Query" && c.Op != "Scan" && c.Op != "BatchGetItem" && strings.Contains(c.InputJSON(), "GroupMembers#") {
			memberWrites++
		}
		return nil
	}})
	if _, err := group(t, "members", "--rebuild"); err != nil {
		t.Fatal(err)
	}
	if memberWrites != 0 {
		t.Errorf("a second --rebuild with nothing to fix made %d member row writes", memberWrites)
	}
}
