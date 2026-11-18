package ruledownload

// Fourth review of the input handling: the conditional tracking summary writes of trackDeliveries when tracking items
// change (the dirty write before the item writes, and the final write after them), a sync state that disappears
// mid-request, and a failed read of the sync state after a failed condition.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	"github.com/airbnb/rudolph/pkg/response"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// r4Table is r2Table with the machine's rules and tracking items, a batch reader that finds no global rule, a batch
// writer that records the tracking item writes, and a conditional writer whose failAt-th write (counting from 1)
// fails its condition after beforeFailing changed the table the way the concurrent writer did. Every other
// conditional write succeeds without changing the stored sync state, which these tests do not read back.
type r4Table struct {
	*r2Table
	machineRules  []machinerules.MachineRuleRow
	deliveries    map[string]machinerules.Delivery
	failAt        int
	beforeFailing func(table *r4Table)
	// failGets makes every GetItem fail from then on
	failGets    bool
	updates     []interface{}
	batchWrites int
}

func (r *r4Table) GetItem(key dynamodb.PrimaryKey, consistent bool) (*awsdynamodb.GetItemOutput, error) {
	if r.failGets {
		r.gets++
		return nil, errors.New("GetItem failed")
	}
	return r.r2Table.GetItem(key, consistent)
}

func (r *r4Table) Query(input *awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error) {
	r.queries++
	pk, _ := input.ExpressionAttributeValues[":pk"].(*types.AttributeValueMemberS)
	if pk == nil {
		return &awsdynamodb.QueryOutput{}, nil
	}
	var items []map[string]types.AttributeValue
	switch pk.Value {
	case machinerules.MachineRulesPK(inputHandlingMachineID):
		for _, row := range r.machineRules {
			item, err := attributevalue.MarshalMap(row)
			if err != nil {
				r.t.Fatal(err)
			}
			items = append(items, item)
		}
	case machinerules.DeliveriesPK(inputHandlingMachineID):
		for sortKey, delivery := range r.deliveries {
			item, err := attributevalue.MarshalMap(struct {
				dynamodb.PrimaryKey
				RemovalFirstSentInSync int `dynamodbav:"RemovalFirstSentInSync,omitempty"`
			}{dynamodb.PrimaryKey{PartitionKey: pk.Value, SortKey: sortKey}, delivery.RemovalFirstSentInSync})
			if err != nil {
				r.t.Fatal(err)
			}
			items = append(items, item)
		}
	}
	return &awsdynamodb.QueryOutput{Items: items}, nil
}

func (r *r4Table) BatchGetItems([]dynamodb.PrimaryKey, bool) ([]map[string]types.AttributeValue, error) {
	return nil, nil
}

func (r *r4Table) BatchWriteItems([]interface{}, []dynamodb.PrimaryKey) error {
	r.batchWrites++
	return nil
}

func (r *r4Table) UpdateItemIf(_ dynamodb.PrimaryKey, item interface{}, _ expression.ConditionBuilder) (*awsdynamodb.UpdateItemOutput, error) {
	r.updates = append(r.updates, item)
	if len(r.updates) != r.failAt {
		return &awsdynamodb.UpdateItemOutput{}, nil
	}
	if r.beforeFailing != nil {
		r.beforeFailing(r)
	}
	return nil, &types.ConditionalCheckFailedException{Message: new(string)}
}

// summaries returns the MachineRulesTracked value of each conditional write so far ("" for a write without one).
func (r *r4Table) summaries() []string {
	out := make([]string, 0, len(r.updates))
	for _, item := range r.updates {
		values, err := attributevalue.MarshalMap(item)
		if err != nil {
			r.t.Fatal(err)
		}
		summary, _ := values["MachineRulesTracked"].(*types.AttributeValueMemberS)
		if summary == nil {
			out = append(out, "")
			continue
		}
		out = append(out, summary.Value)
	}
	return out
}

// r4Handler is r3Handler with table also as the batch reader and writer of the machine rules page.
func r4Handler(table *r4Table) *PostRuledownloadHandler {
	pages := machineRulePage{
		queryer: table, getter: table, updater: table, batchGetter: table, batchWriter: table, timer: mockTimeProvider{},
	}
	return &PostRuledownloadHandler{
		booted:        true,
		cursorService: concreteRuledownloadCursorService{getter: table},
		ghandler:      concreteGlobalRuleDownloader{queryer: table, machineRules: pages},
		fhandler:      concreteFeedRuleDownloader{queryer: table, machineRules: pages},
		mhandler:      concreteMachineRuleDownloader{machineRules: pages},
	}
}

var (
	r4StartingRule = rules.RuleSortKeyFromTypeIdentifier(strings.Repeat("a", 64), rudolphtypes.RuleTypeBinary)
	r4StoppedRule  = rules.RuleSortKeyFromTypeIdentifier(strings.Repeat("b", 64), rudolphtypes.RuleTypeBinary)
	r4RemovedRule  = rules.RuleSortKeyFromTypeIdentifier(strings.Repeat("c", 64), rudolphtypes.RuleTypeBinary)
)

// r4ItemChanges are current syncs whose last page changes tracking items, each with a summary that is the digest of
// the items (neither "" nor dirty), so that the page first writes MachineRulesTrackedDirty, conditional on that
// summary, before any item write.
var r4ItemChanges = map[string]func(t *testing.T) *r4Table{
	// The Mac is sent a machine rule for the first time: its item is written
	"a rule starts applying": func(t *testing.T) *r4Table {
		state := r2SyncState()
		table := &r4Table{r2Table: &r2Table{t: t, syncState: state}, deliveries: map[string]machinerules.Delivery{}}
		table.machineRules = []machinerules.MachineRuleRow{{
			PrimaryKey: dynamodb.PrimaryKey{PartitionKey: machinerules.MachineRulesPK(inputHandlingMachineID), SortKey: r4StartingRule},
			SantaRule:  rules.SantaRule{RuleType: rudolphtypes.RuleTypeBinary, Policy: rudolphtypes.RulePolicyAllowlist, Identifier: strings.Repeat("a", 64)},
		}}
		return table
	},
	// A rule the Mac holds no longer applies (its row is gone): its item is marked with the sync that first sends
	// its removal
	"a rule stops applying": func(t *testing.T) *r4Table {
		deliveries := map[string]machinerules.Delivery{r4StoppedRule: {}}
		state := r2SyncState()
		state.MachineRulesTracked = machinerules.DeliveryDigest(deliveries)
		return &r4Table{r2Table: &r2Table{t: t, syncState: state}, deliveries: deliveries}
	},
	// The removal of a rule was delivered (two completed syncs sent it): its item is deleted
	"a removal was delivered": func(t *testing.T) *r4Table {
		deliveries := map[string]machinerules.Delivery{r4RemovedRule: {RemovalFirstSentInSync: r2CurrentSync - 120_000}}
		state := r2SyncState()
		state.MachineRulesTracked = machinerules.DeliveryDigest(deliveries)
		state.CompletedSyncs = []syncstate.CompletedSync{
			{Seq: r2CurrentSync - 60_000, PreflightAt: "1999-12-31T23:00:00Z"},
			{Seq: r2CurrentSync - 120_000, PreflightAt: "1999-12-31T22:00:00Z"},
		}
		return &r4Table{r2Table: &r2Table{t: t, syncState: state}, deliveries: deliveries}
	},
}

// What the concurrent writer that made a conditional summary write fail leaves.
var (
	// A concurrent invocation of the same last page (Santa's retry of a request that timed out) wrote the summary
	// first; the sync is still the machine's current one
	r4SameSync = func(table *r4Table) {
		table.syncState.MachineRulesTracked = "another summary"
	}
	// A newer preflight replaced the sync state
	r4NewerSync = func(table *r4Table) {
		newer := r2SyncState()
		newer.SyncSeq = r2CurrentSync + 60_000
		table.syncState = newer
	}
	// The sync state expired (TTL) or was deleted by `machine forget`
	r4NoSyncState = func(table *r4Table) {
		table.syncState = nil
	}
)

// The dirty write is the first conditional write of a last page that changes tracking items. Like the final one
// (TestReviewR3_ConditionalFailureOfTheCurrentSyncIsRetryable), its condition fails both when the sync is still the
// machine's current one, which Santa must retry (500), and when a newer sync took over (409). Pins the dirty call
// site, which the fixtures of the third review never reach (they change no item); and no item is written once the
// dirty write failed, as nothing then marks the summary stale.
func TestReviewR4_DirtyWriteFailureDependsOnTheCurrentSync(t *testing.T) {
	for change, fixture := range r4ItemChanges {
		for name, cursor := range r3LastPages {
			for _, c := range []struct {
				concurrent func(*r4Table)
				status     int
				what       string
			}{
				{r4SameSync, 500, "the sync is still the machine's current one"},
				{r4NewerSync, 409, "a newer preflight took over"},
			} {
				t.Run(fmt.Sprintf("%s, %s, %d", change, name, c.status), func(t *testing.T) {
					table := fixture(t)
					table.failAt = 1
					table.beforeFailing = c.concurrent
					resp := r2Handle(t, r4Handler(table), r2Body(t, cursor))
					summaries := table.summaries()
					if len(summaries) == 0 || summaries[0] != syncstate.MachineRulesTrackedDirty {
						t.Fatalf("the conditional writes were %q (answered %d, %s), want the dirty write first, so the fixture tests nothing",
							summaries, resp.StatusCode, resp.Body)
					}
					r2WantError(t, resp, c.status, "a last page whose dirty tracking write failed because "+c.what)
					if table.batchWrites != 0 || len(summaries) != 1 {
						t.Errorf("after the dirty write failed, %d tracking item writes and the conditional writes %q followed, want none",
							table.batchWrites, summaries)
					}
				})
			}
		}
	}
}

// The final summary write after item writes: its condition fails, the summary is invalidated (the item writes may
// have landed after a newer sync recorded its summary), and the answer again depends on whether the sync is still
// the machine's current one. Pins the path where the failure follows item writes.
func TestReviewR4_FinalWriteFailureAfterItemWritesInvalidatesThenDependsOnTheCurrentSync(t *testing.T) {
	for change, fixture := range r4ItemChanges {
		for name, cursor := range r3LastPages {
			for _, c := range []struct {
				concurrent func(*r4Table)
				status     int
				what       string
			}{
				{r4SameSync, 500, "the sync is still the machine's current one"},
				{r4NewerSync, 409, "a newer preflight took over"},
			} {
				t.Run(fmt.Sprintf("%s, %s, %d", change, name, c.status), func(t *testing.T) {
					table := fixture(t)
					table.failAt = 2
					table.beforeFailing = c.concurrent
					resp := r2Handle(t, r4Handler(table), r2Body(t, cursor))
					summaries := table.summaries()
					if len(summaries) < 2 || summaries[0] != syncstate.MachineRulesTrackedDirty || table.batchWrites == 0 {
						t.Fatalf("the conditional writes were %q around %d item writes (answered %d, %s), want the dirty write, item writes and the final write, so the fixture tests nothing",
							summaries, table.batchWrites, resp.StatusCode, resp.Body)
					}
					if len(summaries) != 3 || summaries[2] != syncstate.MachineRulesTrackedOvertaken {
						t.Errorf("after the final write failed, the conditional writes were %q, want the invalidation (%q) third",
							summaries, syncstate.MachineRulesTrackedOvertaken)
					}
					r2WantError(t, resp, c.status, "a last page whose final tracking write failed after item writes because "+c.what)
				})
			}
		}
	}
}

// A sync state deleted between this ruledownload's first read and its conditional write (its TTL expired, or
// `machine forget` removed it) is no superseded sync: no newer sync exists. The owner's answer for a machine without a
// sync state is 400 "No sync in progress: preflight first" (TestReviewR2_RuledownloadWithoutAPreflightIsABadRequest),
// which tells Santa to preflight again; 409 says a newer sync took over, which is not true.
func TestReviewR4_SyncStateDeletedMidRequestIsNoSyncInProgress(t *testing.T) {
	r4ForEachDeletedSyncState(t, func(t *testing.T, resp *events.APIGatewayProxyResponse) {
		r2WantError(t, resp, 400, "a last page whose sync state was deleted before its conditional tracking write")
		var body response.ErrorResponse
		if resp.StatusCode == 400 && (json.Unmarshal([]byte(resp.Body), &body) != nil || body.Error != "No sync in progress: preflight first") {
			t.Errorf("answered 400 with %s, want the body of a ruledownload without a sync state", resp.Body)
		}
	})
}

// Whatever the answer to the test above, the deleted sync state is the client's to fix by preflighting again, not a
// server failure: answered 500, the api_5xx alarm would count it. Pin (409 today, 400 once the test above passes).
func TestReviewR4_SyncStateDeletedMidRequestIsNoServerError(t *testing.T) {
	r4ForEachDeletedSyncState(t, func(t *testing.T, resp *events.APIGatewayProxyResponse) {
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("a last page whose sync state was deleted before its conditional tracking write was answered %d (%s), want a 4xx",
				resp.StatusCode, resp.Body)
		}
	})
}

// r4ForEachDeletedSyncState serves every last page over each conditional tracking write, with the sync state deleted
// just before that write fails its condition, and has check judge the response.
func r4ForEachDeletedSyncState(t *testing.T, check func(t *testing.T, resp *events.APIGatewayProxyResponse)) {
	type fixture struct {
		table  func(t *testing.T) *r4Table
		failAt int
		dirty  bool
	}
	fixtures := map[string]fixture{
		"final write without item changes": {func(t *testing.T) *r4Table {
			stale := r3StaleTrackingState()
			stale.t = t
			return &r4Table{r2Table: stale, deliveries: map[string]machinerules.Delivery{}}
		}, 1, false},
	}
	for change, table := range r4ItemChanges {
		fixtures["dirty write, "+change] = fixture{table, 1, true}
		fixtures["final write after item writes, "+change] = fixture{table, 2, false}
	}
	for fixtureName, f := range fixtures {
		for name, cursor := range r3LastPages {
			t.Run(fixtureName+", "+name, func(t *testing.T) {
				table := f.table(t)
				table.failAt = f.failAt
				table.beforeFailing = r4NoSyncState
				resp := r2Handle(t, r4Handler(table), r2Body(t, cursor))
				if len(table.updates) < f.failAt {
					t.Fatalf("the last page made %d conditional writes (answered %d, %s), fewer than the %d the fixture fails, so it tests nothing",
						len(table.updates), resp.StatusCode, resp.Body, f.failAt)
				}
				check(t, resp)
				// Whatever the answer, a failed dirty write ends the request: item writes after it would land
				// under a sync state that is gone.
				if f.dirty && (table.batchWrites != 0 || len(table.updates) != 1) {
					t.Errorf("after the dirty write failed with the sync state deleted, %d tracking item writes and %d conditional writes followed, want none",
						table.batchWrites, len(table.updates)-1)
				}
			})
		}
	}
}

// When the sync state cannot be read again after a failed condition, whether the sync was overtaken is unknown: a
// server failure, answered 500 so that Santa retries (not 409, which would end a sync that may still succeed). Pin.
func TestReviewR4_SyncStateReadFailureAfterAFailedConditionIsAServerError(t *testing.T) {
	fixtures := map[string]func(t *testing.T) *r4Table{
		"final write without item changes": func(t *testing.T) *r4Table {
			stale := r3StaleTrackingState()
			stale.t = t
			return &r4Table{r2Table: stale, deliveries: map[string]machinerules.Delivery{}}
		},
		"dirty write": r4ItemChanges["a rule starts applying"],
	}
	for fixtureName, fixture := range fixtures {
		for name, cursor := range r3LastPages {
			t.Run(fixtureName+", "+name, func(t *testing.T) {
				table := fixture(t)
				table.failAt = 1
				table.beforeFailing = func(table *r4Table) {
					r4SameSync(table)
					table.failGets = true
				}
				resp := r2Handle(t, r4Handler(table), r2Body(t, cursor))
				if len(table.updates) == 0 {
					t.Fatalf("the last page made no conditional write (answered %d, %s), so the fixture tests nothing", resp.StatusCode, resp.Body)
				}
				r2WantError(t, resp, 500, "a last page whose sync state could not be read again after a failed conditional tracking write")
			})
		}
	}
}
