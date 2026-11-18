package ruledownload

// Third review of the input handling: when ruledownload may answer 409, the page numbers a cursor may carry, and the
// cursors without seq that skip the superseded check.

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// r3Table is r2Table with a conditional writer whose every write fails its condition. Before failing it calls
// beforeFailing, which changes the stored sync state the way the concurrent writer that made the condition fail did.
type r3Table struct {
	*r2Table
	beforeFailing func(row *r2Table)
	updates       int
}

func (r *r3Table) UpdateItemIf(dynamodb.PrimaryKey, interface{}, expression.ConditionBuilder) (*awsdynamodb.UpdateItemOutput, error) {
	r.updates++
	if r.beforeFailing != nil {
		r.beforeFailing(r.r2Table)
	}
	return nil, &types.ConditionalCheckFailedException{Message: new(string)}
}

// r3Handler is r2Handler with table as the conditional writer of the machine rules page.
func r3Handler(table *r3Table) *PostRuledownloadHandler {
	pages := machineRulePage{queryer: table, getter: table, updater: table, timer: mockTimeProvider{}}
	return &PostRuledownloadHandler{
		booted:        true,
		cursorService: concreteRuledownloadCursorService{getter: table},
		ghandler:      concreteGlobalRuleDownloader{queryer: table, machineRules: pages},
		fhandler:      concreteFeedRuleDownloader{queryer: table, machineRules: pages},
		mhandler:      concreteMachineRuleDownloader{machineRules: pages},
	}
}

// r3StaleTrackingState is the current sync of r2SyncState, except that the tracking items' expiry was last extended
// more than machinerules.DeliveryRefreshInterval ago, so its last page must write the tracking summary
// (SetMachineRulesTracked), conditional on the SyncSeq and the summary it read.
func r3StaleTrackingState() *r2Table {
	state := r2SyncState()
	state.MachineRulesTrackingRefreshedAt = "1999-11-01T00:00:00Z"
	return &r2Table{syncState: state}
}

// r3LastPages are the last pages of the current sync, one per strategy, over a fleet without rules.
var r3LastPages = map[string]string{
	"first page, which is also the last": ``,
	"clean sync, last page":              `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa","seq":1790000000000}`,
	"feed sync, last page":               `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa","seq":1790000000000}`,
	"machine page (strategy 3)":          `{"strategy":3,"seq":1790000000000}`,
}

// SetMachineRulesTracked fails its condition (ErrSyncSuperseded) when the summary is not the one this ruledownload
// read, even though the row still belongs to its sync: a concurrent invocation of the same last page (Santa's retry
// of a request that timed out) wrote it first, or an abandoned older sync invalidated it. The sync is then still the
// machine's current one, and Santa retries a 500 (up to five times) but gives up on a 409, which would end a sync
// that can still succeed. So only a sync whose SyncSeq has moved on is a 409.
func TestReviewR3_ConditionalFailureOfTheCurrentSyncIsRetryable(t *testing.T) {
	for name, cursor := range r3LastPages {
		t.Run(name, func(t *testing.T) {
			stale := r3StaleTrackingState()
			stale.t = t
			table := &r3Table{r2Table: stale, beforeFailing: func(row *r2Table) {
				// What a concurrent invocation of the same last page leaves: a new summary, the same sync
				row.syncState.MachineRulesTracked = "another summary"
				row.syncState.MachineRulesTrackingRefreshedAt = "2000-01-01T00:00:00Z"
			}}
			resp := r2Handle(t, r3Handler(table), r2Body(t, cursor))
			if table.updates == 0 {
				t.Fatalf("the last page wrote no tracking summary (answered %d, %s), so the fixture tests nothing", resp.StatusCode, resp.Body)
			}
			r2WantError(t, resp, 500, "a last page whose tracking summary write failed while the sync is still the machine's current one")
		})
	}
}

// The other side of the test above, and the 409 for a sync overtaken while its last page was being served rather
// than before it started: a newer preflight replaced the sync state between this ruledownload's read and its
// conditional write.
func TestReviewR3_SyncOvertakenMidRequestIsAConflict(t *testing.T) {
	for name, cursor := range r3LastPages {
		t.Run(name, func(t *testing.T) {
			stale := r3StaleTrackingState()
			stale.t = t
			table := &r3Table{r2Table: stale, beforeFailing: func(row *r2Table) {
				newer := r2SyncState()
				newer.SyncSeq = r2CurrentSync + 60_000
				row.syncState = newer
			}}
			resp := r2Handle(t, r3Handler(table), r2Body(t, cursor))
			if table.updates == 0 {
				t.Fatalf("the last page wrote no tracking summary (answered %d, %s), so the fixture tests nothing", resp.StatusCode, resp.Body)
			}
			r2WantError(t, resp, 409, "a last page whose sync was overtaken by a newer preflight mid-request")
		})
	}
}

// Rudolph issues a global or feed cursor only for the page after the first (CloneForNextPage of page 1), so its page
// is at least 2; page 0, page 1 or no page beside a key is forged. A page in the quintillions is forged too, and
// overflows when the next page's cursor is made. 1,000,000 pages of at least one rule is beyond any real fleet, so
// the bound costs nothing. These are refused like the other forged cursors, before anything is read.
func TestReviewR3_GlobalOrFeedCursorWithAPageNeverIssuedIsABadRequest(t *testing.T) {
	keys := map[string]string{
		"clean": `"strategy":1,"batch_size":50,"pk":"GlobalRules","sk":"Binary#aa"`,
		"feed":  `"strategy":2,"batch_size":50,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"`,
	}
	pages := map[string]string{
		"page 0":                   `,"page":0`,
		"page 1":                   `,"page":1`,
		"no page":                  ``,
		"page 1,000,001":           `,"page":1000001`,
		"page 9223372036854775807": `,"page":9223372036854775807`,
	}
	for strategy, key := range keys {
		for pageName, page := range pages {
			t.Run(strategy+", "+pageName, func(t *testing.T) {
				cursor := "{" + key + page + `,"seq":1790000000000}`
				table := &r2Table{t: t, syncState: r2SyncState()}
				resp := r2Handle(t, r2Handler(table), r2Body(t, cursor))
				if resp.StatusCode != 400 || table.queries != 0 || table.gets != 0 {
					t.Errorf("cursor %s was answered %d after %d queries and %d reads, want 400 without reading the table",
						cursor, resp.StatusCode, table.queries, table.gets)
				}
			})
		}
	}
}

// The accept side of the page bounds, and strategy 3 left lenient: earlier versions of Rudolph issued machine cursors
// with whatever page they had reached, or none, and the machine page reads no page.
func TestReviewR3_PageBoundsAcceptWhatTheServerIssuesAndLegacyMachineCursors(t *testing.T) {
	for name, cursor := range map[string]string{
		"clean, page 2":                     `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"feed, page 2":                      `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"}`,
		"clean, page 1,000,000":             `{"strategy":1,"batch_size":50,"page":1000000,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"feed, page 1,000,000":              `{"strategy":2,"batch_size":50,"page":1000000,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"}`,
		"machine, page 0":                   `{"strategy":3,"page":0}`,
		"machine, page 1":                   `{"strategy":3,"page":1}`,
		"machine, page 9223372036854775807": `{"strategy":3,"page":9223372036854775807}`,
	} {
		t.Run(name, func(t *testing.T) {
			var parsed ruledownloadCursor
			if err := json.Unmarshal([]byte(cursor), &parsed); err != nil {
				t.Fatal(err)
			}
			if err := parsed.validate(); err != nil {
				t.Errorf("cursor %s was refused: %v", cursor, err)
			}
		})
	}
}

// Every global and feed cursor Rudolph issues after a numbered preflight carries the sync's seq, so one without seq
// for a machine whose sync state is numbered was issued for an older sync (by a version of Rudolph before seq) or
// forged; served, its last page would be recorded as the current sync's. It is answered 409 like a superseded one.
// The exemption stays for strategy 3 cursors, which only earlier versions of Rudolph issued and often without seq,
// and for unnumbered sync states (SyncSeq 0, an older preflight), whose cursors never carry seq.
func TestReviewR3_GlobalOrFeedLastPageWithoutSeqOfANumberedSyncIsAConflict(t *testing.T) {
	for name, cursor := range map[string]string{
		"clean sync, last page": `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"feed sync, last page":  `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"}`,
	} {
		t.Run(name, func(t *testing.T) {
			table := &r2Table{t: t, syncState: r2SyncState()}
			resp := r2Handle(t, r2Handler(table), r2Body(t, cursor))
			r2WantError(t, resp, 409, fmt.Sprintf("a last page without seq for a sync numbered %d (cursor %s)", r2CurrentSync, cursor))
		})
	}
}

// The controls for the test above: the exemptions it keeps.
func TestReviewR3_CursorWithoutSeqIsServedForStrategy3OrAnUnnumberedSync(t *testing.T) {
	for name, c := range map[string]struct {
		cursor  string
		syncSeq int
	}{
		"machine page (strategy 3), numbered sync": {`{"strategy":3}`, r2CurrentSync},
		"clean sync, unnumbered sync":              {`{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`, 0},
		"feed sync, unnumbered sync":               {`{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"}`, 0},
		"machine page (strategy 3), unnumbered":    {`{"strategy":3}`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			state := r2SyncState()
			state.SyncSeq = c.syncSeq
			table := &r2Table{t: t, syncState: state}
			resp := r2Handle(t, r2Handler(table), r2Body(t, c.cursor))
			if resp.StatusCode != 200 {
				t.Errorf("cursor %s for a sync numbered %d was answered %d (%s), want 200", c.cursor, c.syncSeq, resp.StatusCode, resp.Body)
			}
		})
	}
}

// Beside TestReviewR2_RuledownloadWithoutAPreflightIsABadRequest, which covers the clean and machine pages: the feed
// last page reaches the missing sync state by another path (lastPageOrNext reading it again).
func TestReviewR3_FeedLastPageWithoutAPreflightIsABadRequest(t *testing.T) {
	for name, cursor := range map[string]string{
		"with seq":    `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa","seq":1790000000000}`,
		"without seq": `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"}`,
	} {
		t.Run(name, func(t *testing.T) {
			table := &r2Table{t: t}
			resp := r2Handle(t, r2Handler(table), r2Body(t, cursor))
			r2WantError(t, resp, 400, "a feed last page without a sync state")
		})
	}
}
