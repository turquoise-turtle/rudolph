package eventcollection_test

// Collection of allowed executions for group rules (owner approved 2026-11-23). Besides `Global` and
// `Machine#<machine ID>`, the index item (PK "EventCollection", SK "Rules") holds a string set per device group with
// flagged group rules:
//
//   - Group#<group name>: the flagged rules of that group, by rule sort key as stored on the group rule row
//     (`GroupRules#<group>` / `<rule sort key>`); the Macs in no group have `Group#rudolph:ungrouped`.
//
// GroupIndexAttribute names the attribute. ParseIndex fills Index.Groups (group name to the set of flagged sort keys)
// and GroupCandidates returns, for an event's rule sort key, the flagged stored sort keys of each group that match it
// (hash identifiers case-insensitively, as for global and machine rules).

import (
	"reflect"
	"sort"
	"testing"

	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	awsdynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func ss(values ...string) *awsdynamodbtypes.AttributeValueMemberSS {
	return &awsdynamodbtypes.AttributeValueMemberSS{Value: values}
}

func TestGroupIndexAttribute(t *testing.T) {
	for group, want := range map[string]string{
		"eng":               "Group#eng",
		"lab macs":          "Group#lab macs",
		"rudolph:ungrouped": "Group#rudolph:ungrouped",
	} {
		if got := eventcollection.GroupIndexAttribute(group); got != want {
			t.Errorf("GroupIndexAttribute(%q) = %q, want %q", group, got, want)
		}
	}
	// The existing attributes are unchanged.
	if got := eventcollection.IndexAttribute(""); got != "Global" {
		t.Errorf("IndexAttribute(\"\") = %q, want Global", got)
	}
	if got := eventcollection.IndexAttribute("AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"); got != "Machine#AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA" {
		t.Errorf("IndexAttribute(machine) = %q", got)
	}
}

func TestParseIndex_GroupSets(t *testing.T) {
	ix := eventcollection.ParseIndex(map[string]awsdynamodbtypes.AttributeValue{
		"PK":     eventcollectionS("EventCollection"),
		"SK":     eventcollectionS("Rules"),
		"Global": ss("TeamID#EQHXZ8M8AV"),
		"Machine#AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA": ss("SigningID#EQHXZ8M8AV:com.google.Chrome"),
		"Group#eng":               ss("TeamID#ABCDE12345", "SigningID#EQHXZ8M8AV:com.google.GeminiMacOS"),
		"Group#rudolph:ungrouped": ss("TeamID#ABCDE12345"),
		"Group#not a set":         eventcollectionS("TeamID#ABCDE12345"),
	})
	want := map[string]map[string]bool{
		"eng":               {"TeamID#ABCDE12345": true, "SigningID#EQHXZ8M8AV:com.google.GeminiMacOS": true},
		"rudolph:ungrouped": {"TeamID#ABCDE12345": true},
	}
	if !reflect.DeepEqual(ix.Groups, want) {
		t.Errorf("Groups = %v, want %v (a Group# attribute that is not a string set is ignored)", ix.Groups, want)
	}
	if !ix.Global["TeamID#EQHXZ8M8AV"] || len(ix.Machines) != 1 {
		t.Errorf("the global and machine sets were not parsed as before: %v %v", ix.Global, ix.Machines)
	}
	for _, name := range []string{"Group#eng", "Group#rudolph:ungrouped"} {
		if _, ok := ix.Machines[name]; ok {
			t.Errorf("%s was read as a machine", name)
		}
	}
}

func eventcollectionS(v string) *awsdynamodbtypes.AttributeValueMemberS {
	return &awsdynamodbtypes.AttributeValueMemberS{Value: v}
}

func TestIndexWithOnlyGroupSetsIsNotEmpty(t *testing.T) {
	ix := eventcollection.ParseIndex(map[string]awsdynamodbtypes.AttributeValue{"Group#eng": ss("TeamID#ABCDE12345")})
	if ix.Empty() {
		t.Error("an index whose only flagged rules are group rules reports Empty")
	}
	if !eventcollection.ParseIndex(nil).Empty() {
		t.Error("a missing index item is not empty")
	}
	if got := eventcollection.ParseIndex(nil).Groups; got == nil {
		t.Error("Groups of an empty index is nil; want an empty map, as Global and Machines")
	}
}

func TestGroupCandidates(t *testing.T) {
	upper := "Binary#" + "ABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABAB"
	lower := "Binary#" + "abababababababababababababababababababababababababababababababab"
	ix := eventcollection.ParseIndex(map[string]awsdynamodbtypes.AttributeValue{
		"Global":                  ss("TeamID#EQHXZ8M8AV"),
		"Group#eng":               ss("TeamID#ABCDE12345", upper),
		"Group#lab":               ss("TeamID#ABCDE12345"),
		"Group#rudolph:ungrouped": ss(lower),
	})

	sorted := func(m map[string][]string) map[string][]string {
		for _, v := range m {
			sort.Strings(v)
		}
		return m
	}
	if got, want := sorted(ix.GroupCandidates("TeamID#ABCDE12345")), map[string][]string{"eng": {"TeamID#ABCDE12345"}, "lab": {"TeamID#ABCDE12345"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("GroupCandidates(team ID) = %v, want %v", got, want)
	}
	// Santa reports hashes in lower case; older rows may hold upper case. The stored sort key is returned, to confirm
	// the row it names.
	if got, want := sorted(ix.GroupCandidates(lower)), map[string][]string{"eng": {upper}, "rudolph:ungrouped": {lower}}; !reflect.DeepEqual(got, want) {
		t.Errorf("GroupCandidates(binary) = %v, want %v", got, want)
	}
	// Team IDs and signing IDs are case-sensitive.
	if got := ix.GroupCandidates("TeamID#abcde12345"); len(got) != 0 {
		t.Errorf("GroupCandidates matched a team ID in another case: %v", got)
	}
	if got := ix.GroupCandidates("TeamID#EQHXZ8M8AV"); len(got) != 0 {
		t.Errorf("a global rule was returned as a group candidate: %v", got)
	}
	if got := ix.GroupCandidates(""); len(got) != 0 {
		t.Errorf("GroupCandidates(\"\") = %v", got)
	}
	// Global and machine candidates are unchanged by group sets.
	if global, machine := ix.Candidates("AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA", "TeamID#ABCDE12345"); len(global) != 0 || len(machine) != 0 {
		t.Errorf("Candidates returned group rules: %v %v", global, machine)
	}
}
