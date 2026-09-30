package syncstate

import (
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
)

func CreateNewSyncState(
	timeProvider clock.TimeProvider,
	machineID string,
	requestCleanSync bool,
	lastCleanSync string,
	batchSize int,
	feedSyncCursor string,
) SyncStateRow {
	return SyncStateRow{
		PrimaryKey: dynamodb.PrimaryKey{
			PartitionKey: syncStatePK(machineID),
			SortKey:      syncStateSK,
		},
		SyncState: SyncState{
			MachineID:      machineID,
			CleanSync:      requestCleanSync,
			BatchSize:      batchSize,
			LastCleanSync:  lastCleanSync,
			PreflightAt:    clock.RFC3339(timeProvider.Now()),
			FeedSyncCursor: feedSyncCursor,
			ExpiresAfter:   GetSyncStateExpiresAfter(timeProvider),
			DataType:       GetDataType(),
		},
	}
}

// ContinueFrom carries what a sync of the machine hands over to the next one: its number, which syncs completed, and
// the digest of the machine-specific rules the Mac may hold. prev is nil for a machine without a sync state; the Mac
// then holds no machine-specific rules from Rudolph (its first sync is a clean sync).
func (r *SyncStateRow) ContinueFrom(prev *SyncStateRow, emptyMachineRulesDigest string) {
	if prev == nil {
		r.SyncSeq = 1
		r.MachineRulesTracked = emptyMachineRulesDigest
		return
	}
	r.SyncSeq = prev.NextSyncSeq()
	r.CompletedSyncs = prev.NextCompletedSyncs()
	r.MachineRulesTracked = prev.MachineRulesTracked
}
