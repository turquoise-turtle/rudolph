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
//   - SyncSeq numbers the syncs of the machine, and CompletedSyncs holds the last two syncs that completed (reached
//     postflight) before this one started. Santa sends no sync ID, so a postflight cannot prove which sync it closes:
//     a stray one (a delayed duplicate, a retry, a second Mac with a cloned machine ID) can mark a sync as completed
//     whose rules never reached the Mac. Nothing is therefore taken as delivered on the strength of one completion.
//   - FeedSyncCursor is where this sync reads the rules feed from. Postflight does not move it: the next preflight
//     derives the next cursor from the start of the older of the last two completed syncs (see NextFeedSyncCursor).
//   - MachineRulesTracked is a digest of the machine-specific rules the Mac may hold, which are recorded one item per
//     rule in the MachineRuleDelivery#<machine id> partition (see machinerules.GetDeliveries). When the rules that
//     apply now have the same digest, nothing was added or stopped applying, and ruledownload reads nothing more.
//     Empty when unknown (rows written by versions of Rudolph that did not track delivery).
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
	ExpiresAfter           int64           `dynamodbav:"ExpiresAfter,omitempty"`
	DataType               types.DataType  `dynamodbav:"DataType"`
}

// CompletedSync identifies a sync that reached postflight
type CompletedSync struct {
	Seq         int    `dynamodbav:"Seq"`
	PreflightAt string `dynamodbav:"PreflightAt"`
}

// Update fragments

// UpdatePostflightItem closes out a sync the client reported nothing about.
//
// Its FeedSyncCursor (10 minutes before the postflight) is not what the next sync reads from: that is derived from
// PreflightAt by NextFeedSyncCursor, because a cursor taken at the end of a slow sync skips rules added while it ran.
type UpdatePostflightItem struct {
	PostflightAt   string `dynamodbav:"PostflightAt"`
	FeedSyncCursor string `dynamodbav:"FeedSyncCursor"`
	ExpiresAfter   int64  `dynamodbav:"ExpiresAfter,omitempty"`
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

type updateMachineRulesTrackedItem struct {
	MachineRulesTracked string `dynamodbav:"MachineRulesTracked"`
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
