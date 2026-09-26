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
// Every preflight replaces the row and postflight updates it; ruledownload only writes to it when the
// machine-specific rules it serves differ from those the Mac already holds. How one sync hands over to the next:
//
//   - FeedSyncCursor is where this sync reads the rules feed from. Postflight does not move it: the next preflight
//     derives the next cursor from this sync's PreflightAt, and only if this sync completed (see NextFeedSyncCursor).
//   - MachineRulesDelivered is the set of machine-specific rules the Mac may hold as this sync starts, and
//     MachineRulesServed the set this sync's ruledownload sent. The next preflight replaces the delivered set with
//     the served set only if this sync completed (see NextMachineRulesDelivered). So a rule that stopped applying
//     keeps being sent as removed until a sync that served its removal has completed.
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
	MachineRulesDelivered  MachineRuleKeys `dynamodbav:"MachineRulesDelivered"`
	MachineRulesServed     MachineRuleKeys `dynamodbav:"MachineRulesServed"`
	ExpiresAfter           int64           `dynamodbav:"ExpiresAfter,omitempty"`
	DataType               types.DataType  `dynamodbav:"DataType"`
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

type updateMachineRulesServedItem struct {
	MachineRulesServed MachineRuleKeys `dynamodbav:"MachineRulesServed"`
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
