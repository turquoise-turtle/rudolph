package eventupload

// Collection of allowed executions for group rules (owner approved 2026-11-23). The collection index (PK
// "EventCollection", SK "Rules") gains a string set `Group#<group>` per group with flagged rules (see
// pkg/model/eventcollection). An allowed-by-rule execution whose rule sort key is in some `Group#<g>` set is stored
// when:
//
//   - g is one of the uploading Mac's effective groups (groups.Effective over its `Machine#<id>` / `Groups` item and
//     the registry `GroupRegistry` / `Current`: registered assigned groups, reported groups only while the group
//     accepts reported membership, else "rudolph:ungrouped"), as client modes and group rules count them;
//   - the group rule row `GroupRules#<g>` / `<sort key>` exists, is flagged CollectAllowedEvents and is an allow
//     (ALLOWLIST or ALLOWLIST_COMPILER);
//   - no other of the Mac's groups has a block rule (any BLOCKLIST policy) for the key: between groups a block beats
//     an allow, so the group allow did not decide;
//   - the Mac has no machine rule for the key that applies (machinerules.MachineRuleRow.AppliesAt): a machine rule
//     beats a group rule, and then only its own flag matters, as today.
//
// A flagged global rule keeps collecting as today, whatever groups say; global and machine behaviour is unchanged.
//
// Cost: with no Group# set in the index nothing new is read; with some, the Mac's Groups item and the registry are read
// at most once per upload. A failed read is logged (without the rule), skips the events it concerns, stores the rest
// and never fails the upload. A group rule whose row is gone or not flagged is taken out of `Group#<g>` (best effort),
// in a transaction that checks the row was not flagged again, as for global and machine rules.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	rtypes "github.com/airbnb/rudolph/pkg/types"
)

const (
	gcOtherTeam  = "ABCDE12345"
	gcTeamKey    = "TeamID#" + gcOtherTeam
	gcGlobalTeam = "EQHXZ8M8AV"
	gcUngrouped  = "rudolph:ungrouped"
	gcMacB       = "BBBBBBBB-2222-4222-8222-BBBBBBBBBBBB"
	gcMacC       = "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"
)

// fakeStoreTable is the store table as eventupload's collection lookup sees it: GetItem by key, every read counted,
// some reads failing, and every transaction recorded (and applied to nothing).
type fakeStoreTable struct {
	mu        sync.Mutex
	items     map[[2]string]map[string]dynamotypes.AttributeValue
	reads     [][2]string
	fail      func(pk, sk string) bool
	transacts []*dynamodb.TransactWriteItemsInput
}

func newFakeStoreTable() *fakeStoreTable {
	return &fakeStoreTable{items: map[[2]string]map[string]dynamotypes.AttributeValue{}}
}

func keyOf(key map[string]dynamotypes.AttributeValue) [2]string {
	pk, _ := key["PK"].(*dynamotypes.AttributeValueMemberS)
	sk, _ := key["SK"].(*dynamotypes.AttributeValueMemberS)
	var k [2]string
	if pk != nil {
		k[0] = pk.Value
	}
	if sk != nil {
		k[1] = sk.Value
	}
	return k
}

func (f *fakeStoreTable) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(in.Key)
	f.reads = append(f.reads, k)
	if f.fail != nil && f.fail(k[0], k[1]) {
		return nil, errors.New("injected read failure")
	}
	return &dynamodb.GetItemOutput{Item: f.items[k]}, nil
}

func (f *fakeStoreTable) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transacts = append(f.transacts, in)
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func (f *fakeStoreTable) put(pk, sk string, attrs map[string]dynamotypes.AttributeValue) {
	item := map[string]dynamotypes.AttributeValue{"PK": &dynamotypes.AttributeValueMemberS{Value: pk}, "SK": &dynamotypes.AttributeValueMemberS{Value: sk}}
	for k, v := range attrs {
		item[k] = v
	}
	f.items[[2]string{pk, sk}] = item
}

func (f *fakeStoreTable) readsOf(match func(pk, sk string) bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reads {
		if match(r[0], r[1]) {
			n++
		}
	}
	return n
}

func avS(v string) dynamotypes.AttributeValue { return &dynamotypes.AttributeValueMemberS{Value: v} }
func avN(v int) dynamotypes.AttributeValue {
	return &dynamotypes.AttributeValueMemberN{Value: fmt.Sprint(v)}
}
func avB(v bool) dynamotypes.AttributeValue { return &dynamotypes.AttributeValueMemberBOOL{Value: v} }
func avSS(v ...string) dynamotypes.AttributeValue {
	return &dynamotypes.AttributeValueMemberSS{Value: v}
}

// index writes the collection index: attribute name (Global, Machine#<id>, Group#<g>) to flagged sort keys.
func (f *fakeStoreTable) index(sets map[string][]string) {
	attrs := map[string]dynamotypes.AttributeValue{}
	for name, keys := range sets {
		attrs[name] = avSS(keys...)
	}
	f.put("EventCollection", "Rules", attrs)
}

// registry writes the group registry: group name to AcceptReported.
func (f *fakeStoreTable) registry(groups map[string]bool) {
	m := map[string]dynamotypes.AttributeValue{}
	for name, accept := range groups {
		m[name] = &dynamotypes.AttributeValueMemberM{Value: map[string]dynamotypes.AttributeValue{
			"Description": avS("test"), "AcceptReported": avB(accept), "RulesVersion": avN(1), "CreatedAt": avS("2026-11-23T00:00:00Z"),
		}}
	}
	f.put("GroupRegistry", "Current", map[string]dynamotypes.AttributeValue{
		"Groups": &dynamotypes.AttributeValueMemberM{Value: m}, "MembershipVersion": avN(1), "DataType": avS("GroupRegistry"),
	})
}

func (f *fakeStoreTable) membership(machine string, assigned, reported []string) {
	attrs := map[string]dynamotypes.AttributeValue{"DataType": avS("MachineGroups")}
	if len(assigned) > 0 {
		attrs["Assigned"] = avSS(assigned...)
	}
	if len(reported) > 0 {
		attrs["Reported"] = avSS(reported...)
	}
	f.put("Machine#"+machine, "Groups", attrs)
}

func ruleAttrs(sk string, policy rtypes.Policy, flagged bool) map[string]dynamotypes.AttributeValue {
	ruleType, identifier := rtypes.RuleTypeTeamID, strings.TrimPrefix(sk, "TeamID#")
	if strings.HasPrefix(sk, "SigningID#") {
		ruleType, identifier = rtypes.RuleTypeSigningID, strings.TrimPrefix(sk, "SigningID#")
	}
	attrs := map[string]dynamotypes.AttributeValue{
		"RuleType": avN(int(ruleType)), "Policy": avN(int(policy)), "Identifier": avS(identifier), "Description": avS("test"),
	}
	if flagged {
		attrs["CollectAllowedEvents"] = avB(true)
	}
	return attrs
}

func (f *fakeStoreTable) groupRule(group, sk string, policy rtypes.Policy, flagged bool) {
	attrs := ruleAttrs(sk, policy, flagged)
	attrs["Group"], attrs["DataType"] = avS(group), avS("GroupRule")
	f.put("GroupRules#"+group, sk, attrs)
}

func (f *fakeStoreTable) machineRule(machine, sk string, policy rtypes.Policy, flagged bool, expires int64, removing bool) {
	attrs := ruleAttrs(sk, policy, flagged)
	if expires != 0 {
		attrs["ExpiresAfter"] = avN(int(expires))
	}
	if removing {
		attrs["DeleteOnNextSync"] = avB(true)
	}
	f.put("MachineRules#"+machine, sk, attrs)
}

func (f *fakeStoreTable) globalRule(sk string, policy rtypes.Policy, flagged bool) {
	attrs := ruleAttrs(sk, policy, flagged)
	attrs["DataType"] = avS("GlobalRule")
	f.put("GlobalRules", sk, attrs)
}

var gcNow = time.Unix(1_795_000_000, 0)

func gcSHA(i int) string { return fmt.Sprintf("%064x", 0x6c0000+i) }

func allowedEvent(sha, decision string, pid int, teamID, signingID string) EventUploadEvent {
	return EventUploadEvent{FileSHA256: sha, Decision: decision, ProcessID: pid, ExecutionTime: float64(gcNow.Unix() - 60),
		FileName: "tool", FilePath: "/Applications/Tool.app/Contents/MacOS", TeamID: teamID, SigningIDs: signingID}
}

func teamEvent(sha string, pid int, team string) EventUploadEvent {
	return allowedEvent(sha, "ALLOW_TEAMID", pid, team, "")
}

// upload stores one upload of machine through a fresh warm Lambda's store (no cache from an earlier test), and returns
// the SHA-256s whose execution events were written.
func upload(t *testing.T, table *fakeStoreTable, machine string, events ...EventUploadEvent) map[string]bool {
	t.Helper()
	collected := newCollectedRules(table, "store")
	collected.now = func() time.Time { return gcNow }
	return uploadWith(t, collected, machine, events...)
}

func uploadWith(t *testing.T, collected *collectedRules, machine string, events ...EventUploadEvent) map[string]bool {
	t.Helper()
	rec := &storeRecorder{}
	store := newEventStore(rec, "events", collected)
	store.now = func() time.Time { return gcNow }
	if _, _, err := store.Store(context.Background(), machine, events, nil); err != nil {
		t.Fatalf("the upload failed: %v", err)
	}
	stored := map[string]bool{}
	for _, e := range events {
		for _, key := range rec.keys {
			// The event's sort key is <TS>#<sha>#<pid>; counters and markers hold the hash alone.
			if strings.Contains(key, "#"+e.FileSHA256+"#") {
				stored[e.FileSHA256] = true
			}
		}
	}
	return stored
}

func TestGroupCollection_FlaggedGroupRuleCollectsForItsMembersOnly(t *testing.T) {
	table := newFakeStoreTable()
	table.index(map[string][]string{"Group#eng": {gcTeamKey}})
	table.registry(map[string]bool{"eng": false, "lab": false})
	table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
	table.membership(machineID, []string{"eng"}, nil)
	table.membership(gcMacB, []string{"lab"}, nil)

	if !upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam))[gcSHA(1)] {
		t.Error("an execution allowed by a flagged rule of the Mac's group was not stored")
	}
	if upload(t, table, gcMacB, teamEvent(gcSHA(2), 2, gcOtherTeam))[gcSHA(2)] {
		t.Error("a Mac in another group had its execution stored through eng's flag")
	}
	if upload(t, table, gcMacC, teamEvent(gcSHA(3), 3, gcOtherTeam))[gcSHA(3)] {
		t.Error("a Mac in no group had its execution stored through eng's flag")
	}
	// Other rules of the same Mac are not collected.
	if upload(t, table, machineID, teamEvent(gcSHA(4), 4, "ZZZZZ99999"))[gcSHA(4)] {
		t.Error("an execution allowed by an unflagged rule was stored")
	}
}

func TestGroupCollection_EveryAllowedDecisionMatchesGroupRules(t *testing.T) {
	signing := "SigningID#" + gcOtherTeam + ":com.example.tool"
	table := newFakeStoreTable()
	table.index(map[string][]string{"Group#eng": {signing}})
	table.registry(map[string]bool{"eng": false})
	table.groupRule("eng", signing, rtypes.RulePolicyAllowlistCompiler, true)
	table.membership(machineID, []string{"eng"}, nil)
	got := upload(t, table, machineID, allowedEvent(gcSHA(1), "ALLOW_SIGNINGID", 1, gcOtherTeam, gcOtherTeam+":com.example.tool"))
	if !got[gcSHA(1)] {
		t.Error("an ALLOW_SIGNINGID execution of a flagged ALLOWLIST_COMPILER group rule was not stored")
	}
}

func TestGroupCollection_UngroupedSetCollectsForMacsInNoGroup(t *testing.T) {
	table := newFakeStoreTable()
	table.index(map[string][]string{"Group#" + gcUngrouped: {gcTeamKey}})
	table.registry(map[string]bool{"eng": false, gcUngrouped: false})
	table.groupRule(gcUngrouped, gcTeamKey, rtypes.RulePolicyAllowlist, true)
	table.membership(gcMacB, []string{"eng"}, nil)
	// Assigned a group that is no longer registered: in no group.
	table.membership(gcMacC, []string{"gone"}, nil)

	if !upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam))[gcSHA(1)] {
		t.Error("a Mac without a groups item (in no group) did not have its execution stored through the ungrouped flag")
	}
	if !upload(t, table, gcMacC, teamEvent(gcSHA(3), 3, gcOtherTeam))[gcSHA(3)] {
		t.Error("a Mac assigned only to an unregistered group (in no group) did not have its execution stored")
	}
	if upload(t, table, gcMacB, teamEvent(gcSHA(2), 2, gcOtherTeam))[gcSHA(2)] {
		t.Error("a Mac in a group had its execution stored through the ungrouped flag")
	}
}

func TestGroupCollection_ReportedMembershipCountsOnlyWhenTheGroupAcceptsIt(t *testing.T) {
	staffKey := "TeamID#STAFF00001"
	table := newFakeStoreTable()
	table.index(map[string][]string{"Group#lab": {gcTeamKey}, "Group#staff": {staffKey}})
	table.registry(map[string]bool{"lab": true, "staff": false})
	table.groupRule("lab", gcTeamKey, rtypes.RulePolicyAllowlist, true)
	table.groupRule("staff", staffKey, rtypes.RulePolicyAllowlist, true)
	table.membership(machineID, nil, []string{"lab", "staff"})

	got := upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam), teamEvent(gcSHA(2), 2, "STAFF00001"))
	if !got[gcSHA(1)] {
		t.Error("a reported group that accepts reported membership did not count")
	}
	if got[gcSHA(2)] {
		t.Error("a reported group that does not accept reported membership counted")
	}
}

func TestGroupCollection_TheGroupRuleRowMustConfirm(t *testing.T) {
	for name, setup := range map[string]func(*fakeStoreTable){
		"row gone":        func(*fakeStoreTable) {},
		"row not flagged": func(f *fakeStoreTable) { f.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, false) },
		"row is a block":  func(f *fakeStoreTable) { f.groupRule("eng", gcTeamKey, rtypes.RulePolicyBlocklist, true) },
		"group not registered": func(f *fakeStoreTable) {
			f.registry(map[string]bool{"lab": false})
			f.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			table := newFakeStoreTable()
			table.index(map[string][]string{"Group#eng": {gcTeamKey}})
			table.registry(map[string]bool{"eng": false})
			table.membership(machineID, []string{"eng"}, nil)
			setup(table)
			if upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam))[gcSHA(1)] {
				t.Error("the execution was stored")
			}
		})
	}
}

// forgotten reports whether a transaction took sk out of the index attribute, conditional on the rule row at
// (rulePK, sk) not being flagged.
func (f *fakeStoreTable) forgotten(attribute, rulePK, sk string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.transacts {
		var update, check bool
		for _, item := range in.TransactItems {
			if u := item.Update; u != nil && keyOf(u.Key) == [2]string{"EventCollection", "Rules"} {
				for _, name := range u.ExpressionAttributeNames {
					if name != attribute {
						continue
					}
					for _, v := range u.ExpressionAttributeValues {
						if set, ok := v.(*dynamotypes.AttributeValueMemberSS); ok && len(set.Value) == 1 && set.Value[0] == sk {
							update = update || (u.UpdateExpression != nil && strings.Contains(*u.UpdateExpression, "DELETE"))
						}
					}
				}
			}
			if c := item.ConditionCheck; c != nil && keyOf(c.Key) == [2]string{rulePK, sk} {
				check = true
			}
		}
		if update && check {
			return true
		}
	}
	return false
}

func TestGroupCollection_AStaleGroupEntryIsTakenOutOfTheIndex(t *testing.T) {
	for name, rowExists := range map[string]bool{"row gone": false, "row not flagged": true} {
		t.Run(name, func(t *testing.T) {
			table := newFakeStoreTable()
			table.index(map[string][]string{"Group#eng": {gcTeamKey}})
			table.registry(map[string]bool{"eng": false})
			table.membership(machineID, []string{"eng"}, nil)
			if rowExists {
				table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, false)
			}
			upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam))
			if !table.forgotten("Group#eng", "GroupRules#eng", gcTeamKey) {
				t.Errorf("no transaction took %s out of Group#eng, checking GroupRules#eng first (transactions: %d)", gcTeamKey, len(table.transacts))
			}
		})
	}
}

func TestGroupCollection_ABlockInAnotherOfTheMacsGroupsWins(t *testing.T) {
	blocks := []rtypes.Policy{rtypes.RulePolicyBlocklist, rtypes.RulePolicySilentBlocklist, rtypes.RulePolicySilentGUIBlocklist, rtypes.RulePolicySilentTTYBlocklist}
	for _, block := range blocks {
		t.Run(fmt.Sprint("policy ", int(block)), func(t *testing.T) {
			table := newFakeStoreTable()
			table.index(map[string][]string{"Group#eng": {gcTeamKey}})
			// "aaa" sorts before "eng" and "zzz" after: the name order must not matter.
			table.registry(map[string]bool{"eng": false, "aaa": false, "zzz": false})
			table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
			table.groupRule("aaa", gcTeamKey, block, false)
			table.groupRule("zzz", gcTeamKey, block, false)
			table.membership(machineID, []string{"eng", "aaa"}, nil)
			table.membership(gcMacB, []string{"eng", "zzz"}, nil)
			if upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam))[gcSHA(1)] {
				t.Error("stored through eng's allow although another of the Mac's groups (sorting first) blocks the key")
			}
			if upload(t, table, gcMacB, teamEvent(gcSHA(2), 2, gcOtherTeam))[gcSHA(2)] {
				t.Error("stored through eng's allow although another of the Mac's groups (sorting last) blocks the key")
			}
		})
	}

	t.Run("blocks elsewhere do not count", func(t *testing.T) {
		table := newFakeStoreTable()
		table.index(map[string][]string{"Group#eng": {gcTeamKey}})
		table.registry(map[string]bool{"eng": false, "lab": false, "other": false})
		table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
		// Another of the Mac's groups allows the key too, without the flag; it blocks another key.
		table.groupRule("lab", gcTeamKey, rtypes.RulePolicyAllowlist, false)
		table.groupRule("lab", "TeamID#ZZZZZ99999", rtypes.RulePolicyBlocklist, false)
		// A group the Mac is not in blocks the key.
		table.groupRule("other", gcTeamKey, rtypes.RulePolicyBlocklist, false)
		table.membership(machineID, []string{"eng", "lab"}, nil)
		if !upload(t, table, machineID, teamEvent(gcSHA(3), 3, gcOtherTeam))[gcSHA(3)] {
			t.Error("not stored, although no group of the Mac blocks the key")
		}
	})
}

func TestGroupCollection_AMachineRuleDecidesForItsMac(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*fakeStoreTable)
		indexed bool // the machine rule is in the Mac's Machine# set
		want    bool
	}{
		{"machine allow not flagged", func(f *fakeStoreTable) {
			f.machineRule(machineID, gcTeamKey, rtypes.RulePolicyAllowlist, false, gcNow.Add(time.Hour).Unix(), false)
		}, false, false},
		{"machine allow without expiry, not flagged", func(f *fakeStoreTable) {
			f.machineRule(machineID, gcTeamKey, rtypes.RulePolicyAllowlist, false, 0, false)
		}, false, false},
		{"machine allow flagged", func(f *fakeStoreTable) {
			f.machineRule(machineID, gcTeamKey, rtypes.RulePolicyAllowlist, true, gcNow.Add(time.Hour).Unix(), false)
		}, true, true},
		{"machine rule expired", func(f *fakeStoreTable) {
			f.machineRule(machineID, gcTeamKey, rtypes.RulePolicyAllowlist, false, gcNow.Add(-time.Hour).Unix(), false)
		}, false, true},
		{"machine rule being removed", func(f *fakeStoreTable) {
			f.machineRule(machineID, gcTeamKey, rtypes.RulePolicyAllowlist, false, 0, true)
		}, false, true},
		{"machine rule of another Mac", func(f *fakeStoreTable) {
			f.machineRule(gcMacB, gcTeamKey, rtypes.RulePolicyAllowlist, false, 0, false)
		}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			table := newFakeStoreTable()
			sets := map[string][]string{"Group#eng": {gcTeamKey}}
			if c.indexed {
				sets["Machine#"+machineID] = []string{gcTeamKey}
			}
			table.index(sets)
			table.registry(map[string]bool{"eng": false})
			table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
			table.membership(machineID, []string{"eng"}, nil)
			c.setup(table)
			if got := upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam))[gcSHA(1)]; got != c.want {
				t.Errorf("stored = %v, want %v", got, c.want)
			}
		})
	}
}

// The global flag works as today, whatever the Mac's groups say.
func TestGroupCollection_AFlaggedGlobalRuleStillCollects(t *testing.T) {
	globalKey := "TeamID#" + gcGlobalTeam
	table := newFakeStoreTable()
	table.index(map[string][]string{"Global": {globalKey}, "Group#eng": {gcTeamKey}})
	table.registry(map[string]bool{"eng": false})
	table.globalRule(globalKey, rtypes.RulePolicyAllowlist, true)
	table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
	table.groupRule("eng", globalKey, rtypes.RulePolicyAllowlist, false)
	table.membership(machineID, []string{"eng"}, nil)
	got := upload(t, table, machineID, teamEvent(gcSHA(1), 1, gcGlobalTeam), teamEvent(gcSHA(2), 2, gcOtherTeam))
	if !got[gcSHA(1)] {
		t.Error("an execution of a flagged global rule's key was not stored")
	}
	if !got[gcSHA(2)] {
		t.Error("an execution of a flagged group rule was not stored beside a flagged global rule")
	}
}

func isNewPartition(pk, _ string) bool {
	return pk == "GroupRegistry" || strings.HasPrefix(pk, "GroupRules#") || strings.HasPrefix(pk, "Machine#")
}

// Without any Group# set, collection reads exactly what it read before group rules could be flagged.
func TestGroupCollection_NoGroupSetsReadNothingNew(t *testing.T) {
	globalKey := "TeamID#" + gcGlobalTeam
	machineKey := "SigningID#" + gcOtherTeam + ":com.example.tool"
	table := newFakeStoreTable()
	table.index(map[string][]string{"Global": {globalKey}, "Machine#" + machineID: {machineKey}})
	table.registry(map[string]bool{"eng": false})
	table.membership(machineID, []string{"eng"}, nil)
	table.globalRule(globalKey, rtypes.RulePolicyAllowlist, true)
	table.machineRule(machineID, machineKey, rtypes.RulePolicyAllowlist, true, 0, false)
	table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)

	got := upload(t, table, machineID,
		teamEvent(gcSHA(1), 1, gcGlobalTeam),
		allowedEvent(gcSHA(2), "ALLOW_SIGNINGID", 2, gcOtherTeam, gcOtherTeam+":com.example.tool"),
		teamEvent(gcSHA(3), 3, gcOtherTeam),
		teamEvent(gcSHA(4), 4, "ZZZZZ99999"),
	)
	if !got[gcSHA(1)] || !got[gcSHA(2)] {
		t.Errorf("the flagged global and machine rules no longer collect: %v", got)
	}
	if got[gcSHA(3)] {
		t.Error("a flagged group rule that is not in the index collected")
	}
	if n := table.readsOf(isNewPartition); n != 0 {
		t.Errorf("%d reads of the registry, a Groups item or group rules although no group rule is flagged: %v", n, table.reads)
	}
	// The index, the global rule and the machine rule: as before.
	if n := table.readsOf(func(string, string) bool { return true }); n != 3 {
		t.Errorf("%d reads, want 3 (the index and one confirmation each for the global and the machine rule): %v", n, table.reads)
	}
}

// An upload without an allowed-by-rule execution reads nothing at all, group sets or not.
func TestGroupCollection_UploadWithoutAllowedByRuleEventsReadsNothing(t *testing.T) {
	table := newFakeStoreTable()
	table.index(map[string][]string{"Group#eng": {gcTeamKey}})
	table.registry(map[string]bool{"eng": false})
	table.membership(machineID, []string{"eng"}, nil)
	upload(t, table, machineID,
		allowedEvent(gcSHA(1), "BLOCK_BINARY", 1, gcOtherTeam, ""),
		allowedEvent(gcSHA(2), "ALLOW_UNKNOWN", 2, gcOtherTeam, ""),
		allowedEvent(gcSHA(3), "ALLOW_SCOPE", 3, gcOtherTeam, ""),
	)
	if len(table.reads) != 0 {
		t.Errorf("%d reads of the store table for an upload without allowed-by-rule executions: %v", len(table.reads), table.reads)
	}
}

func TestGroupCollection_GroupsItemAndRegistryAreReadAtMostOncePerUpload(t *testing.T) {
	table := newFakeStoreTable()
	var keys []string
	var events []EventUploadEvent
	for i := 0; i < 6; i++ {
		team := fmt.Sprintf("TEAM%06d", i)
		keys = append(keys, "TeamID#"+team)
		table.groupRule("eng", "TeamID#"+team, rtypes.RulePolicyAllowlist, true)
		events = append(events, teamEvent(gcSHA(10+i), 10+i, team), teamEvent(gcSHA(20+i), 20+i, team))
	}
	// Keys of the index that are no group's do not need the Mac's groups either way.
	events = append(events, teamEvent(gcSHA(30), 30, "ZZZZZ99999"))
	table.index(map[string][]string{"Group#eng": keys, "Group#lab": {"TeamID#LAB0000001"}})
	table.registry(map[string]bool{"eng": false, "lab": false})
	table.membership(machineID, []string{"eng"}, nil)

	got := upload(t, table, machineID, events...)
	for i := 0; i < 6; i++ {
		if !got[gcSHA(10+i)] || !got[gcSHA(20+i)] {
			t.Errorf("the executions of flagged group rule %d were not stored", i)
		}
	}
	groupsReads := table.readsOf(func(pk, sk string) bool { return pk == "Machine#"+machineID && sk == "Groups" })
	registryReads := table.readsOf(func(pk, _ string) bool { return pk == "GroupRegistry" })
	if groupsReads > 1 || registryReads > 1 {
		t.Errorf("one upload read the Mac's Groups item %d times and the registry %d times, want at most once each", groupsReads, registryReads)
	}
	if n := table.readsOf(func(pk, _ string) bool { return strings.HasPrefix(pk, "Machine#") && pk != "Machine#"+machineID }); n != 0 {
		t.Errorf("%d reads of other Macs' partitions", n)
	}
	// Each group rule is confirmed once per upload, however many of its executions the upload holds.
	for _, key := range keys {
		if n := table.readsOf(func(pk, sk string) bool { return pk == "GroupRules#eng" && sk == key }); n > 1 {
			t.Errorf("the row of %s was read %d times in one upload", key, n)
		}
	}
}

func TestGroupCollection_AFailedReadSkipsOnlyTheEventsItConcerns(t *testing.T) {
	globalKey := "TeamID#" + gcGlobalTeam
	for name, fail := range map[string]func(pk, sk string) bool{
		"groups item":    func(pk, sk string) bool { return pk == "Machine#"+machineID && sk == "Groups" },
		"registry":       func(pk, _ string) bool { return pk == "GroupRegistry" },
		"group rule row": func(pk, _ string) bool { return strings.HasPrefix(pk, "GroupRules#") },
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			table := newFakeStoreTable()
			table.index(map[string][]string{"Global": {globalKey}, "Group#eng": {gcTeamKey}})
			table.registry(map[string]bool{"eng": false})
			table.membership(machineID, []string{"eng"}, nil)
			table.globalRule(globalKey, rtypes.RulePolicyAllowlist, true)
			table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
			table.fail = fail

			got := upload(t, table, machineID,
				allowedEvent(gcSHA(1), "BLOCK_UNKNOWN", 1, "", ""),
				teamEvent(gcSHA(2), 2, gcOtherTeam),
				teamEvent(gcSHA(3), 3, gcGlobalTeam),
			)
			if !got[gcSHA(1)] {
				t.Error("the blocked execution was not stored")
			}
			if !got[gcSHA(3)] {
				t.Error("the execution of the flagged global rule was not stored")
			}
			if got[gcSHA(2)] {
				t.Error("the execution of the group rule was stored although its lookup failed")
			}
			if out := logs.String(); !strings.Contains(out, machineID) {
				t.Errorf("the failed read was not logged with the machine ID:\n%s", out)
			} else if strings.Contains(out, gcOtherTeam) {
				t.Errorf("the log names the rule, which identifies what the Mac runs:\n%s", out)
			}
			for _, in := range table.transacts {
				for _, item := range in.TransactItems {
					if item.Update != nil {
						t.Errorf("a failed read took a rule out of the index: %+v", item.Update.ExpressionAttributeNames)
					}
				}
			}
		})
	}
}

func TestGroupCollection_ConfirmationsAreCachedForAMinuteAtMost(t *testing.T) {
	table := newFakeStoreTable()
	table.index(map[string][]string{"Group#eng": {gcTeamKey}})
	table.registry(map[string]bool{"eng": false})
	table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, true)
	table.membership(machineID, []string{"eng"}, nil)
	now := gcNow
	collected := newCollectedRules(table, "store")
	collected.now = func() time.Time { return now }

	if !uploadWith(t, collected, machineID, teamEvent(gcSHA(1), 1, gcOtherTeam))[gcSHA(1)] {
		t.Fatal("the first upload did not store the execution")
	}
	// Collection switched off: a warm Lambda may keep storing for up to 60 s, not longer.
	table.groupRule("eng", gcTeamKey, rtypes.RulePolicyAllowlist, false)
	now = gcNow.Add(61 * time.Second)
	if uploadWith(t, collected, machineID, teamEvent(gcSHA(2), 2, gcOtherTeam))[gcSHA(2)] {
		t.Error("a warm Lambda still stored the group rule's executions more than 60 s after collection was switched off")
	}
}
