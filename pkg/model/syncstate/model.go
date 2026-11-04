package syncstate

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
)

const (
	machineInfoPKPrefix             = "Machine#"
	syncStateSK                     = "SyncState"
	syncStateExpiresAfterInDays int = 90
)

// SyncStateRow persists the sync state to the database
type SyncStateRow struct {
	dynamodb.PrimaryKey
	SyncState
}

// SyncState encapulsates data within the sync state row that's not the Primary key
//
// Every preflight replaces the row and postflight updates it; ruledownload only writes to it when the set of
// machine-specific rules it tracks for the Mac changes. The row stays small whatever the number of rules: it is
// written on every sync, and DynamoDB bills a write by the size of the whole item. How one sync hands over to the next:
//
//   - SyncSeq identifies the sync (increasing, at least the preflight time in milliseconds, so it keeps increasing when
//     the row is lost and recreated), and CompletedSyncs holds the last two syncs that completed (reached
//     postflight) before this one started. Santa sends no sync ID, so a postflight cannot prove which sync it closes:
//     a stray one (a delayed duplicate, a retry, a second Mac with a cloned machine ID) can mark a sync as completed
//     whose rules never reached the Mac. Nothing is therefore taken as delivered on the strength of one completion.
//   - FeedSyncCursor is where this sync reads the rules feed from. Postflight does not move it: the next preflight
//     derives the next cursor from the start of the older of the last two completed syncs (see NextFeedSyncCursor).
//   - MachineRulesTracked is a digest of the machine-specific rules the Mac may hold, which are recorded one item per
//     rule in the MachineRuleDelivery#<machine id> partition (see machinerules.GetDeliveries). When the rules that
//     apply now have the same digest, nothing was added or stopped applying, and ruledownload reads nothing more.
//     Empty when unknown (rows written by versions of Rudolph that did not track delivery). MachineRulesTrackedDirty
//     while the items may differ from the last digest written: ruledownload sets it before it changes the items, and
//     preflight sets it when the previous sync's ruledownload did not finish (it may still be running, or died).
//     Every write of the digest by ruledownload is conditional on the row still being its sync's (SyncSeq).
type SyncState struct {
	MachineID      string `dynamodbav:"MachineID"`
	BatchSize      int    `dynamodbav:"BatchSize"`
	CleanSync      bool   `dynamodbav:"CleanSync"`
	LastCleanSync  string `dynamodbav:"LastCleanSync"`
	FeedSyncCursor string `dynamodbav:"FeedSyncCursor"`
	PreflightAt    string `dynamodbav:"PreflightAt"`
	// Only written by versions of Rudolph that updated the row twice during ruledownload; kept so old rows load.
	RuledownloadStartedAt  string          `dynamodbav:"RuledownloadStartedAt,omitempty"`
	RuledownloadFinishedAt string          `dynamodbav:"RuledownloadFinishedAt,omitempty"`
	PostflightAt           string          `dynamodbav:"PostflightAt"`
	LastSyncType           string          `dynamodbav:"LastSyncType,omitempty"`
	RulesReceived          int             `dynamodbav:"RulesReceived"`
	RulesProcessed         int             `dynamodbav:"RulesProcessed"`
	RulesHashAfterSync     string          `dynamodbav:"RulesHashAfterSync,omitempty"`
	SyncSeq                int             `dynamodbav:"SyncSeq,omitempty"`
	CompletedSyncs         []CompletedSync `dynamodbav:"CompletedSyncs,omitempty"`
	MachineRulesTracked    string          `dynamodbav:"MachineRulesTracked,omitempty"`
	// When the machine's delivery tracking items last had their expiry extended (see machinerules.GetDeliveries)
	// Empty when unknown (a sync state created for a Mac without one), which makes the next ruledownload extend it.
	MachineRulesTrackingRefreshedAt string `dynamodbav:"MachineRulesTrackingRefreshedAt,omitempty"`
	// For a clean sync, the start of the last clean sync that completed before it (LastCleanSync holds this sync's
	// start). If this clean sync does not complete, e.g. its preflight response was lost and Santa sent the preflight
	// again, the next preflight goes back to this value instead of counting a clean sync the Mac never performed.
	PreviousCleanSync string `dynamodbav:"PreviousCleanSync,omitempty"`
	// How many syncs in a row before this one received no postflight. Santa ignores a failed postflight, so a Mac
	// whose postflights never arrive keeps syncing; while this is not zero, ruledownload records when it served the
	// sync's last page (LastPageServedAt), which can then count as the sync's completion (see SettleCompletion).
	SyncsWithoutPostflight int `dynamodbav:"SyncsWithoutPostflight,omitempty"`
	// When the previous sync of the machine started (its PreflightAt), for this sync's ruledownload to tell which feed
	// rows it serves for the first time (see ContentRun). Empty for a machine's first sync.
	PreviousPreflightAt string `dynamodbav:"PreviousPreflightAt,omitempty"`
	// When the last completed sync before this one counted through the fallback: how many syncs, this one included,
	// have served the newest content it carried. 0 when the last completed sync did not count through the fallback.
	// See SettleCompletion.
	FallbackContentServings int `dynamodbav:"FallbackContentServings,omitempty"`
	// How many syncs in a row before this one served the newest content the previous sync served without completing
	// (the previous sync's LastPageServedRun, or 0 when it completed or recorded none). See NextContentRun.
	ContentRun int `dynamodbav:"ContentRun,omitempty"`
	// When ruledownload served this sync's last page; only recorded when it may count (see LastPageServedMayCount).
	LastPageServedAt string `dynamodbav:"LastPageServedAt,omitempty"`
	// Recorded with LastPageServedAt: this sync served nothing the covering completed sync had not already
	// served (see ServedNothingNew), so counting it as completed cannot lose anything.
	LastPageServedNothingNew bool `dynamodbav:"LastPageServedNothingNew,omitempty"`
	// Recorded with LastPageServedAt: how many syncs in a row, this one included, served the newest content this sync
	// served without completing (see NextContentRun).
	LastPageServedRun int `dynamodbav:"LastPageServedRun,omitempty"`
	// The rules_hash the Mac reported in this sync's preflight (empty when it reported none, or the row was written
	// by a version of Rudolph that did not record it). The next preflight compares it with the hash it receives to
	// tell whether the Mac applied this sync's rules (see SettleCompletion).
	PreflightRulesHash string `dynamodbav:"PreflightRulesHash,omitempty"`
	// Set on a clean sync performed while the machine config's clean sync flag is set: this sync performs the
	// requested clean sync, and the flag is cleared once it is confirmed (see ConfigCleanSyncConfirmed).
	// ConfigCleanSyncRequest identifies the request (machineconfiguration.MachineConfiguration.CleanSyncRequest, empty
	// for a flag stored without one), so a request made after this sync started is not cleared by it.
	ConfigCleanSync        bool   `dynamodbav:"ConfigCleanSync,omitempty"`
	ConfigCleanSyncRequest string `dynamodbav:"ConfigCleanSyncRequest,omitempty"`
	// How many earlier clean syncs in a row for the same request had their last page served without anything
	// confirming that the Mac applied them (see ConfigCleanSyncConfirmed).
	ConfigCleanSyncServed int            `dynamodbav:"ConfigCleanSyncServed,omitempty"`
	ExpiresAfter          int64          `dynamodbav:"ExpiresAfter,omitempty"`
	DataType              types.DataType `dynamodbav:"DataType"`

	// Whether the served last page counts as this sync's completion, whether the Mac is proven to have applied this
	// sync, and whether it counts through the fallback; decided by the next preflight (SettleCompletion), never stored.
	lastPageApplied        bool
	appliedProven          bool
	countedThroughFallback bool
}

// CompletedSync identifies a sync that completed (see SyncState.Completed)
type CompletedSync struct {
	Seq         int    `dynamodbav:"Seq"`
	PreflightAt string `dynamodbav:"PreflightAt"`
	// The Mac is proven to have applied this sync: its rules hash changed (see SyncState.SettleCompletion).
	Applied bool `dynamodbav:"Applied,omitempty"`
}

// Update fragments

// UpdatePostflightItem closes out a sync the client reported nothing about: it only marks the sync as completed. Like
// every postflight it leaves FeedSyncCursor alone; the next preflight derives the next cursor (NextFeedSyncCursor).
type UpdatePostflightItem struct {
	PostflightAt string `dynamodbav:"PostflightAt"`
	ExpiresAfter int64  `dynamodbav:"ExpiresAfter,omitempty"`
}

// UpdatePostflightOutcomeItem closes out a sync: it marks the sync as completed (PostflightAt) and records what the
// client reported about the sync it just finished: the sync type it actually performed, how many rules it received
// and applied, and the hash of its rule database afterwards (which is what reveals drift; the preflight hash is
// stored with the sensor data).
//
// It does not touch FeedSyncCursor or the machine rule sets: the next preflight derives both from this row once the
// sync is marked as completed.
//
// The two reported strings are omitted when the client reported none, so a postflight that says nothing
// about them leaves the last known values in place instead of erasing them.
type UpdatePostflightOutcomeItem struct {
	PostflightAt       string `dynamodbav:"PostflightAt"`
	ExpiresAfter       int64  `dynamodbav:"ExpiresAfter,omitempty"`
	LastSyncType       string `dynamodbav:"LastSyncType,omitempty"`
	RulesReceived      int    `dynamodbav:"RulesReceived"`
	RulesProcessed     int    `dynamodbav:"RulesProcessed"`
	RulesHashAfterSync string `dynamodbav:"RulesHashAfterSync,omitempty"`
}

// MachineRulesTrackedDirty is the MachineRulesTracked value while the tracking items may not match the digest. It is
// never the digest of a set of rules, so a sync that sees it reads the items.
const MachineRulesTrackedDirty = "dirty"

// MachineRulesTrackedOvertaken is the MachineRulesTracked value written by a ruledownload that changed tracking items
// and then found that the sync state no longer holds what it expected (typically a newer sync of the machine took
// over while its writes were in flight): its item writes may have landed after the newer sync's, so the summary
// the newer sync wrote cannot be trusted. Like MachineRulesTrackedDirty it never matches a set of rules. It differs
// from it so that a newer ruledownload still writing items, which expects MachineRulesTrackedDirty, cannot replace
// it with a summary that hides the older writes.
const MachineRulesTrackedOvertaken = "dirty:overtaken"

type updateMachineRulesTrackedOnlyItem struct {
	MachineRulesTracked string `dynamodbav:"MachineRulesTracked"`
}

type updateLastPageServedItem struct {
	LastPageServedAt         string `dynamodbav:"LastPageServedAt"`
	LastPageServedNothingNew bool   `dynamodbav:"LastPageServedNothingNew"`
	LastPageServedRun        int    `dynamodbav:"LastPageServedRun"`
}

type updateMachineRulesTrackedItem struct {
	MachineRulesTracked             string `dynamodbav:"MachineRulesTracked"`
	MachineRulesTrackingRefreshedAt string `dynamodbav:"MachineRulesTrackingRefreshedAt,omitempty"`
}

func syncStatePK(machineID string) string {
	return fmt.Sprintf("%s%s", machineInfoPKPrefix, machineID)
}

func GetSyncStateExpiresAfter(timeProvider clock.TimeProvider) int64 {
	return clock.Unixtimestamp(timeProvider.Now().UTC().AddDate(0, 0, syncStateExpiresAfterInDays))
}

func GetDataType() types.DataType {
	return types.DataTypeSyncState
}
