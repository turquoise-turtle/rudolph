//go:build integration

package handlers

// Group members, the preflight side (the rows and the CLI are described in
// internal/cli/group/group_members_cli_integration_test.go). Preflight keeps `GroupMembers#<group>` / `<machine id>`
// in step with the Reported set it writes on `Machine#<id>` / `Groups`, in the same transaction: a group the Mac starts
// reporting gets Reported=true on its row (created when missing), a group it stops reporting loses Reported, and a row
// left with neither Assigned nor Reported is deleted. A failed write changes neither item and leaves the membership
// basis as it was, so the next preflight tries again. Steady-state syncs write nothing more.

import (
	"context"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type memberRowFlags struct{ exists, assigned, reported bool }

func readMemberRow(t *testing.T, group, machineID string) memberRowFlags {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.TableName),
		Key:            map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupMembers#" + group}, "SK": &awstypes.AttributeValueMemberS{Value: machineID}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Item) == 0 {
		return memberRowFlags{}
	}
	f := memberRowFlags{exists: true}
	if b, ok := out.Item["Assigned"].(*awstypes.AttributeValueMemberBOOL); ok {
		f.assigned = b.Value
	}
	if b, ok := out.Item["Reported"].(*awstypes.AttributeValueMemberBOOL); ok {
		f.reported = b.Value
	}
	return f
}

func reportedAttribute(t *testing.T, machineID string) []string {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.TableName),
		Key:            map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "Machine#" + machineID}, "SK": &awstypes.AttributeValueMemberS{Value: "Groups"}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := out.Item["Reported"].(*awstypes.AttributeValueMemberSS)
	if ss == nil {
		return nil
	}
	return ss.Value
}

func TestGroupMembers_PreflightKeepsReportedRowsInStep(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true, "ops": true, "admins": false})
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"Eng", "ops", "admins", "unregistered"}
	syncNormal(t, h)
	for group, want := range map[string]memberRowFlags{
		"eng":          {exists: true, reported: true},
		"ops":          {exists: true, reported: true},
		"admins":       {}, // does not accept reported membership
		"unregistered": {},
	} {
		if got := readMemberRow(t, group, machineA); got != want {
			t.Errorf("after reporting eng and ops: GroupMembers#%s = %+v, want %+v", group, got, want)
		}
	}

	h.PrimaryUserGroups = []string{"eng"}
	syncNormal(t, h)
	if got := readMemberRow(t, "ops", machineA); got.exists {
		t.Errorf("the Mac stopped reporting ops but its row is still there: %+v", got)
	}
	if got := readMemberRow(t, "eng", machineA); got != (memberRowFlags{exists: true, reported: true}) {
		t.Errorf("eng row after reporting only eng: %+v", got)
	}

	// An assigned group keeps its row, without Reported, when the Mac stops reporting it
	assignGroups(t, machineA, "eng")
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName:                 aws.String(ddblocal.TableName),
		Key:                       map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "Machine#" + machineA}, "SK": &awstypes.AttributeValueMemberS{Value: "Groups"}},
		UpdateExpression:          aws.String("SET Reported = :r"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":r": &awstypes.AttributeValueMemberSS{Value: []string{"eng"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	putItem(t, map[string]any{"PK": "GroupMembers#eng", "SK": machineA, "DataType": "GroupMember", "Assigned": true, "Reported": true, "UpdatedAt": "2026-11-11T00:00:00Z"})
	h.PrimaryUserGroups = nil
	syncNormal(t, h)
	if got := readMemberRow(t, "eng", machineA); got != (memberRowFlags{exists: true, assigned: true}) {
		t.Errorf("eng row after an assigned Mac stopped reporting eng: %+v, want Assigned only", got)
	}
}

// From the groups review: a Mac whose sync state expired must still lose a group it no longer reports, row included.
func TestGroupMembers_RowClearsAfterTheSyncStateExpired(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true})
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng"}
	syncNormal(t, h)
	if got := readMemberRow(t, "eng", machineA); !got.reported {
		t.Fatalf("precondition: the eng row, got %+v", got)
	}
	if _, err := ddblocal.Raw(t).DeleteItem(context.Background(), &awsdynamodb.DeleteItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key:       map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "Machine#" + machineA}, "SK": &awstypes.AttributeValueMemberS{Value: "SyncState"}},
	}); err != nil {
		t.Fatal(err)
	}
	h.PrimaryUserGroups = nil
	for i := 0; i < 3; i++ {
		h.MustSync(t)
	}
	if got := readMemberRow(t, "eng", machineA); got.exists {
		t.Errorf("after leaving eng while the sync state had expired: the row is still there: %+v", got)
	}
}

// The groups item and the member rows change together, and a failure is retried at the next preflight.
func TestGroupMembers_AFailedWriteChangesNeitherAndIsRetried(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true})
	h := newHost(t, machineA, transport)
	syncNormal(t, h)

	failing := true
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if failing && c.Op != "GetItem" && c.Op != "Query" && c.Op != "Scan" && c.Op != "BatchGetItem" && strings.Contains(c.InputJSON(), "GroupMembers#") {
			f := ddblocal.FaultAccessDenied
			return &f
		}
		return nil
	}})
	h.PrimaryUserGroups = []string{"eng"}
	h.MustSync(t) // a membership write that fails is not fatal to the sync
	if got := reportedAttribute(t, machineA); len(got) != 0 {
		t.Errorf("the groups item records Reported %v although its member row could not be written", got)
	}
	if got := readMemberRow(t, "eng", machineA); got.exists {
		t.Errorf("a member row was written although the write failed: %+v", got)
	}

	failing = false
	h.MustSync(t)
	if got := reportedAttribute(t, machineA); len(got) != 1 || got[0] != "eng" {
		t.Errorf("the next preflight did not retry: Reported %v", got)
	}
	if got := readMemberRow(t, "eng", machineA); got != (memberRowFlags{exists: true, reported: true}) {
		t.Errorf("the next preflight did not write the row: %+v", got)
	}
}

func TestGroupMembers_SteadyStateWritesNoMemberRow(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true, "ops": true})
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng", "ops"}
	syncNormal(t, h)
	h.MustSync(t)

	var writes int
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op != "GetItem" && c.Op != "Query" && c.Op != "Scan" && c.Op != "BatchGetItem" && strings.Contains(c.InputJSON(), "GroupMembers#") {
			writes++
		}
		return nil
	}})
	h.MustSync(t)
	h.MustSync(t)
	if writes != 0 {
		t.Errorf("steady-state syncs made %d member row writes, want none", writes)
	}
}
