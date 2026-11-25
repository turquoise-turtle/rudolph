//go:build integration

package fleet_test

// Shared fixtures for the fleet job's tests (internal/fleet, run by cmd/fleet once a day). The job is called as
//
//	fleet.Run(ctx, fleet.Options{Client, Table, StoreTable, Now, MaxMacs})
//
// where Client is the part of *dynamodb.Client it uses (GetItem, Query, BatchWriteItem, PutItem: fleet.Client), Table
// is the events table (EVENTS_DYNAMODB_NAME), StoreTable the store table (DYNAMODB_NAME), Now defaults to time.Now and
// MaxMacs to fleet.DefaultMaxMacs (250,000). The minimum Santa version is not an option: it is FleetHealth / Config's
// MinSantaVersion, else fleethealth.DefaultMinSantaVersion ("2026.9").
//
// Macs are seeded as the sync Lambdas store them: sensor data (`Machine#<id>` / `Current`, in the DataType_MachineID
// index), sync state (`SyncState`), an optional machine config (`Config`) and groups item (`Groups`); the registry
// (`GroupRegistry` / `Current`), the global config, global and group rules, the fleet health setting (`FleetHealth` /
// `Config`: stale threshold, holiday, minimum Santa version) and unknown markers (`UnknownSeen#<id>` / `<sha>` in the
// events table) likewise.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/internal/fleet"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

var (
	ctx = context.Background()
	// The job's clock in every test: midday UTC, so day arithmetic never crosses midnight by accident.
	now   = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	today = "2026-11-22"
)

const (
	monitor    = 1
	lockdown   = 2
	standalone = 3
)

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func daysAgo(n int) string { return now.AddDate(0, 0, -n).Format("2006-01-02") }

func sha(i int) string { return fmt.Sprintf("%064x", 0xf1ee7000+i) }

// machineID is a canonical UUID for fixture n.
func machineID(n int) string { return fmt.Sprintf("%08X-1111-4111-8111-%012X", n, n) }

// mac describes one Mac as the store table holds it. The zero value (with an id) is healthy and measured: synced and
// completed 10 minutes ago, MONITOR as Rudolph intends, SIP on, Santa 2026.9.0, 5 rules, first seen 40 days ago.
type mac struct {
	id, hostname, serial string
	santaVersion         string // default "2026.9.0"; "-" stores an empty version
	reportedMode         int    // default MONITOR
	sip                  *uint32
	noRules              bool

	preflightAt  time.Time // default now - 10 minutes
	postflightAt time.Time // default preflightAt + 1 minute; see noPostflight
	noPostflight bool
	// completed are the PreflightAt of the last completed syncs before this one (CompletedSyncs).
	completed              []time.Time
	syncsWithoutPostflight int
	rulesReceived          int
	rulesProcessed         int
	rulesHash, digest      string
	firstSeenDay           string // default 40 days ago; "-" stores none

	// Machine config; absent when all are zero.
	ownMode          int
	temporaryMode    int
	temporaryExpires time.Time
	disableUpload    *bool

	assigned, reported []string
}

func put(t *testing.T, table string, item map[string]any) {
	t.Helper()
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ddblocal.Raw(t).PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: av}); err != nil {
		t.Fatal(err)
	}
}

func putStore(t *testing.T, item map[string]any)  { t.Helper(); put(t, ddblocal.TableName, item) }
func putEvents(t *testing.T, item map[string]any) { t.Helper(); put(t, ddblocal.EventsTableName, item) }

func (m mac) seed(t *testing.T) {
	t.Helper()
	pk := "Machine#" + m.id
	preflight := m.preflightAt
	if preflight.IsZero() {
		preflight = now.Add(-10 * time.Minute)
	}
	version := m.santaVersion
	switch version {
	case "":
		version = "2026.9.0"
	case "-":
		version = ""
	}
	mode := m.reportedMode
	if mode == 0 {
		mode = monitor
	}
	rules := 5
	if m.noRules {
		rules = 0
	}
	sensor := map[string]any{
		"PK": pk, "SK": "Current", "DataType": "SensorData", "MachineID": m.id,
		"ModelIdentifier": "Mac15,3", "OSVersion": "26.1", "OSBuild": "25B78", "SantaVersion": version,
		"ClientMode": mode, "RequestCleanSync": false, "PrimaryUser": "alice", "RuleCount": rules, "BinaryRuleCount": rules,
		"CertificateRuleCount": 0, "CDHashRuleCount": 0, "TeamIDRuleCount": 0, "SigningIDRuleCount": 0,
		"CompilerRuleCount": 0, "TransitiveRuleCount": 0, "Time": rfc(preflight), "LastSeenDay": preflight.UTC().Format("2006-01-02"),
		"ExpiresAfter": now.AddDate(0, 0, 90).Unix(),
	}
	if m.hostname != "" {
		sensor["Hostname"], sensor["HostnameKey"] = m.hostname, strings.ToLower(m.hostname)
	}
	if m.serial != "" {
		sensor["SerialNum"], sensor["SerialKey"] = m.serial, strings.ToUpper(m.serial)
	}
	if m.sip != nil {
		sensor["SIPStatus"] = *m.sip
	}
	putStore(t, sensor)

	postflight := ""
	if !m.noPostflight {
		p := m.postflightAt
		if p.IsZero() {
			p = preflight.Add(time.Minute)
		}
		postflight = rfc(p)
	}
	state := map[string]any{
		"PK": pk, "SK": "SyncState", "DataType": "SyncState", "MachineID": m.id, "BatchSize": 50,
		"CleanSync": false, "LastCleanSync": "", "FeedSyncCursor": "", "PreflightAt": rfc(preflight),
		"PostflightAt": postflight, "RulesReceived": m.rulesReceived, "RulesProcessed": m.rulesProcessed,
		"SyncSeq": preflight.UnixMilli(), "ExpiresAfter": now.AddDate(0, 0, 90).Unix(),
	}
	if m.syncsWithoutPostflight > 0 {
		state["SyncsWithoutPostflight"] = m.syncsWithoutPostflight
	}
	if m.rulesHash != "" {
		state["RulesHashAfterSync"] = m.rulesHash
	}
	if m.digest != "" {
		state["MachineRulesTracked"] = m.digest
	}
	var completed []map[string]any
	for i, c := range m.completed {
		completed = append(completed, map[string]any{"Seq": c.UnixMilli() + int64(i), "PreflightAt": rfc(c)})
	}
	if len(completed) > 0 {
		state["CompletedSyncs"] = completed
	}
	switch m.firstSeenDay {
	case "":
		state["FirstSeenDay"] = daysAgo(40)
	case "-":
	default:
		state["FirstSeenDay"] = m.firstSeenDay
	}
	putStore(t, state)

	if m.ownMode != 0 || m.temporaryMode != 0 || m.disableUpload != nil {
		cfg := map[string]any{"PK": pk, "SK": "Config", "DataType": "MachineConfig"}
		if m.ownMode != 0 {
			cfg["ClientMode"] = m.ownMode
		}
		if m.temporaryMode != 0 {
			cfg["TemporaryClientMode"] = m.temporaryMode
			cfg["TemporaryClientModeExpiresAt"] = m.temporaryExpires.Unix()
		}
		if m.disableUpload != nil {
			cfg["DisableUnknownEventUpload"] = *m.disableUpload
		}
		putStore(t, cfg)
	}

	if len(m.assigned) > 0 || len(m.reported) > 0 {
		g := map[string]any{"PK": pk, "SK": "Groups", "DataType": "MachineGroups", "UpdatedAt": rfc(now.Add(-24 * time.Hour))}
		if len(m.assigned) > 0 {
			g["Assigned"] = &types.AttributeValueMemberSS{Value: m.assigned}
		}
		if len(m.reported) > 0 {
			g["Reported"] = &types.AttributeValueMemberSS{Value: m.reported}
		}
		av, err := attributevalue.MarshalMap(g)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"Assigned", "Reported"} {
			if ss, ok := g[k].(*types.AttributeValueMemberSS); ok {
				av[k] = ss
			}
		}
		if _, err := ddblocal.Raw(t).PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: av}); err != nil {
			t.Fatal(err)
		}
	}
}

func seed(t *testing.T, macs ...mac) {
	t.Helper()
	for _, m := range macs {
		m.seed(t)
	}
}

func boolPtr(b bool) *bool      { return &b }
func sipFlags(v uint32) *uint32 { return &v }

// group is a registry entry; mode 0 is none.
type group struct {
	name           string
	acceptReported bool
	mode           int
}

// registry writes the group registry as Rudolph stores it.
func registry(t *testing.T, groups ...group) {
	t.Helper()
	entries := map[string]any{}
	for i, g := range groups {
		e := map[string]any{"Description": g.name, "AcceptReported": g.acceptReported, "RulesVersion": i + 1, "CreatedAt": rfc(now.AddDate(0, -1, 0))}
		if g.mode != 0 {
			e["ClientMode"] = g.mode
		}
		entries[g.name] = e
	}
	putStore(t, map[string]any{"PK": "GroupRegistry", "SK": "Current", "DataType": "GroupRegistry", "Groups": entries,
		"MembershipVersion": 1, "RulesVersionCounter": len(groups)})
}

// globalConfig stores the global config through Rudolph's own code, with the given client mode and, when set,
// DisableUnknownEventUpload.
func globalConfig(t *testing.T, mode rudolphtypes.ClientMode, disableUpload *bool) {
	t.Helper()
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.ClientMode = mode
	cfg.DisableUnknownEventUpload = disableUpload
	svc := machineconfiguration.GetUncachedMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	if err := svc.SetGlobalConfig(cfg, machineconfiguration.ConfirmGlobalLockdown()); err != nil {
		t.Fatal(err)
	}
}

// globalRule stores a global rule's row; the job only needs its sort key (Binary#, SigningID#, TeamID#...).
func globalRule(t *testing.T, sk string) {
	t.Helper()
	typ, identifier, _ := strings.Cut(sk, "#")
	ruleType := map[string]int{"Binary": 1, "Cert": 2, "SigningID": 3, "TeamID": 4, "CDHash": 5}[typ]
	putStore(t, map[string]any{"PK": "GlobalRules", "SK": sk, "RuleType": ruleType, "Policy": 1, "Identifier": identifier,
		"GlobalRulePolicy": 1, "Description": "fleet test"})
}

func groupRule(t *testing.T, group, sk string) {
	t.Helper()
	typ, identifier, _ := strings.Cut(sk, "#")
	ruleType := map[string]int{"Binary": 1, "Cert": 2, "SigningID": 3, "TeamID": 4, "CDHash": 5}[typ]
	putStore(t, map[string]any{"PK": "GroupRules#" + group, "SK": sk, "RuleType": ruleType, "Policy": 1, "Identifier": identifier,
		"Group": group, "DataType": "GroupRule", "Description": "fleet test"})
}

// holiday stores the FleetHealth / Config item as `rudolph fleet holiday` and the web view write it. normal 0 leaves
// StaleDays out; holidayDays 0 leaves the holiday out.
func holiday(t *testing.T, normal, holidayDays int, until string) {
	t.Helper()
	item := map[string]any{"PK": "FleetHealth", "SK": "Config"}
	if normal != 0 {
		item["StaleDays"] = normal
	}
	if holidayDays != 0 {
		item["HolidayStaleDays"] = holidayDays
		item["HolidayUntil"] = until
	}
	putStore(t, item)
}

// marker is an unknown marker as eventupload writes it.
type marker struct {
	sha, signingID, teamID string
	lastDay                int // days before today
}

func markers(t *testing.T, machine string, ms ...marker) {
	t.Helper()
	var requests []types.WriteRequest
	for _, m := range ms {
		item := map[string]any{"PK": "UnknownSeen#" + machine, "SK": m.sha, "LastDay": daysAgo(m.lastDay), "FileName": "tool",
			"ExpiresAfter": now.AddDate(0, 0, 35-m.lastDay).Unix()}
		if m.signingID != "" {
			item["SigningID"] = m.signingID
		}
		if m.teamID != "" {
			item["TeamID"] = m.teamID
		}
		av, err := attributevalue.MarshalMap(item)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: av}})
	}
	for len(requests) > 0 {
		n := min(25, len(requests))
		pending := map[string][]types.WriteRequest{ddblocal.EventsTableName: requests[:n]}
		for len(pending) > 0 {
			out, err := ddblocal.Raw(t).BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
			if err != nil {
				t.Fatal(err)
			}
			pending = out.UnprocessedItems
		}
		requests = requests[n:]
	}
}

// nMarkers returns n markers of distinct binaries, all seen today.
func nMarkers(first, n int) []marker {
	out := make([]marker, n)
	for i := range out {
		out[i] = marker{sha: sha(first + i)}
	}
	return out
}

// ---------------------------------------------------------------------------------------------------------------
// Running the job, with every DynamoDB call recorded
// ---------------------------------------------------------------------------------------------------------------

// call is one DynamoDB call the job made.
type call struct {
	seq        int
	op         string
	table      string
	index      string
	keys       []string // partition key values: Query's, GetItem's, or every written item's
	limit      int32
	started    int // number of calls finished when this one started
	finishedAt int // number of calls finished when this one finished, itself included
}

// recorder is a fleet.Client over DynamoDB Local that records every call and can fail one.
type recorder struct {
	raw      *dynamodb.Client
	mu       sync.Mutex
	calls    []*call
	finished int
	// failBatch, when > 0, fails the failBatch-th BatchWriteItem call.
	failBatch int
	batches   int
}

var keyCondition = regexp.MustCompile(`(#?\w+)\s*=\s*(:\w+)`)

func (r *recorder) begin(op, table, index string, keys []string, limit int32) *call {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := &call{seq: len(r.calls), op: op, table: table, index: index, keys: keys, limit: limit, started: r.finished}
	r.calls = append(r.calls, c)
	return c
}

func (r *recorder) end(c *call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished++
	c.finishedAt = r.finished
}

func stringOf(v types.AttributeValue) string {
	if s, ok := v.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return fmt.Sprintf("%v", v)
}

func (r *recorder) GetItem(ctx context.Context, in *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	c := r.begin("GetItem", aws.ToString(in.TableName), "", []string{stringOf(in.Key["PK"])}, 0)
	defer r.end(c)
	return r.raw.GetItem(ctx, in, optFns...)
}

func (r *recorder) Query(ctx context.Context, in *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	var keys []string
	for _, m := range keyCondition.FindAllStringSubmatch(aws.ToString(in.KeyConditionExpression), -1) {
		name := m[1]
		if strings.HasPrefix(name, "#") {
			name = in.ExpressionAttributeNames[name]
		}
		if name == "PK" || name == "DataType" {
			keys = append(keys, stringOf(in.ExpressionAttributeValues[m[2]]))
		}
	}
	c := r.begin("Query", aws.ToString(in.TableName), aws.ToString(in.IndexName), keys, aws.ToInt32(in.Limit))
	defer r.end(c)
	return r.raw.Query(ctx, in, optFns...)
}

func (r *recorder) BatchWriteItem(ctx context.Context, in *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	var keys []string
	table := ""
	for name, reqs := range in.RequestItems {
		table = name
		for _, w := range reqs {
			if w.PutRequest != nil {
				keys = append(keys, stringOf(w.PutRequest.Item["PK"]))
			}
			if w.DeleteRequest != nil {
				keys = append(keys, "DELETE "+stringOf(w.DeleteRequest.Key["PK"]))
			}
		}
	}
	c := r.begin("BatchWriteItem", table, "", keys, 0)
	defer r.end(c)
	r.mu.Lock()
	r.batches++
	fail := r.failBatch > 0 && r.batches == r.failBatch
	r.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("injected BatchWriteItem failure")
	}
	return r.raw.BatchWriteItem(ctx, in, optFns...)
}

func (r *recorder) PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	c := r.begin("PutItem", aws.ToString(in.TableName), "", []string{stringOf(in.Item["PK"])}, 0)
	defer r.end(c)
	return r.raw.PutItem(ctx, in, optFns...)
}

var _ fleet.Client = (*recorder)(nil)

func newRecorder(t *testing.T) *recorder { return &recorder{raw: ddblocal.Raw(t)} }

func options(r *recorder, at time.Time) fleet.Options {
	return fleet.Options{Client: r, Table: ddblocal.EventsTableName, StoreTable: ddblocal.TableName, Now: func() time.Time { return at }}
}

// run runs the job at now and returns its calls.
func run(t *testing.T) *recorder {
	t.Helper()
	return runAt(t, now)
}

func runAt(t *testing.T, at time.Time) *recorder {
	t.Helper()
	r := newRecorder(t)
	if err := fleet.Run(ctx, options(r, at)); err != nil {
		t.Fatalf("fleet: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------------------------------------------
// Reading what the job wrote
// ---------------------------------------------------------------------------------------------------------------

type counts struct{ Macs, Ready, Few, Many, Unmeasured, Locked int }

// pointer is FleetPointer / Current, decoded.
type pointer struct {
	Snapshot, GeneratedAt, MinSantaVersion, HolidayUntil string
	Macs, Attention, VersionUnknown, Capped, StaleDays   int
	Holiday                                              bool
	Health                                               map[string]int
	Readiness                                            map[string]counts

	raw map[string]types.AttributeValue
}

func readPointer(t *testing.T) (pointer, bool) {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.EventsTableName),
		Key:            map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: "FleetPointer"}, "SK": &types.AttributeValueMemberS{Value: "Current"}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Item) == 0 {
		return pointer{}, false
	}
	var p pointer
	if err := attributevalue.UnmarshalMap(out.Item, &p); err != nil {
		t.Fatalf("the pointer does not decode: %v\n%v", err, out.Item)
	}
	p.raw = out.Item
	return p, true
}

func mustPointer(t *testing.T) pointer {
	t.Helper()
	p, ok := readPointer(t)
	if !ok {
		t.Fatal("no FleetPointer / Current after the run")
	}
	return p
}

// rows reads a whole list of the pointer's snapshot, in sort key order. list is e.g. "Health#Stale", "Ready#All" or
// "Ready#Group#eng".
func rows(t *testing.T, p pointer, list string) []map[string]types.AttributeValue {
	t.Helper()
	return partition(t, "FleetSnapshot#"+p.Snapshot+"#"+list)
}

func partition(t *testing.T, pk string) []map[string]types.AttributeValue {
	t.Helper()
	var items []map[string]types.AttributeValue
	var start map[string]types.AttributeValue
	for {
		out, err := ddblocal.Raw(t).Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(ddblocal.EventsTableName),
			KeyConditionExpression:    aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: pk}},
			ExclusiveStartKey:         start,
			ConsistentRead:            aws.Bool(true),
		})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, out.Items...)
		if len(out.LastEvaluatedKey) == 0 {
			return items
		}
		start = out.LastEvaluatedKey
	}
}

// eventsItems scans the whole events table (tests only; the job never scans).
func eventsItems(t *testing.T) []map[string]types.AttributeValue {
	t.Helper()
	var items []map[string]types.AttributeValue
	var start map[string]types.AttributeValue
	for {
		out, err := ddblocal.Raw(t).Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(ddblocal.EventsTableName), ExclusiveStartKey: start})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, out.Items...)
		if len(out.LastEvaluatedKey) == 0 {
			return items
		}
		start = out.LastEvaluatedKey
	}
}

func s(item map[string]types.AttributeValue, name string) string {
	v, _ := item[name].(*types.AttributeValueMemberS)
	if v == nil {
		return ""
	}
	return v.Value
}

func n(item map[string]types.AttributeValue, name string) (int, bool) {
	v, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	var out int
	if _, err := fmt.Sscan(v.Value, &out); err != nil {
		return 0, false
	}
	return out, true
}

func b(item map[string]types.AttributeValue, name string) (bool, bool) {
	v, ok := item[name].(*types.AttributeValueMemberBOOL)
	if !ok {
		return false, false
	}
	return v.Value, true
}

// sks returns the sort keys of rows.
func sks(items []map[string]types.AttributeValue) []string {
	var out []string
	for _, it := range items {
		out = append(out, s(it, "SK"))
	}
	return out
}

// byMachine indexes rows by MachineID.
func byMachine(items []map[string]types.AttributeValue) map[string]map[string]types.AttributeValue {
	out := map[string]map[string]types.AttributeValue{}
	for _, it := range items {
		out[s(it, "MachineID")] = it
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reset empties both tables.
func reset(t *testing.T) {
	t.Helper()
	ddblocal.Reset(t)
}

func fleetRun(opts fleet.Options) error { return fleet.Run(ctx, opts) }
