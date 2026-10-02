package syncstate

import (
	"time"

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

// ContinueFrom carries what a sync of the machine hands over to the next one: its identifier, which syncs completed,
// and the summary of the machine-specific rules the Mac may hold. prev is nil for a machine without a sync state (new,
// or its row expired); the Mac then holds no machine-specific rules from Rudolph, as its sync is a clean sync.
func (r *SyncStateRow) ContinueFrom(prev *SyncStateRow, emptyMachineRulesDigest string, now time.Time) {
	r.SyncSeq = int(now.UnixMilli())
	if prev == nil {
		r.MachineRulesTracked = emptyMachineRulesDigest
		r.MachineRulesTrackingRefreshedAt = clock.RFC3339(now)
		return
	}
	if next := prev.NextSyncSeq(); next > r.SyncSeq {
		r.SyncSeq = next
	}
	r.CompletedSyncs = prev.NextCompletedSyncs()
	r.MachineRulesTracked = prev.MachineRulesTracked
	r.MachineRulesTrackingRefreshedAt = prev.MachineRulesTrackingRefreshedAt
	if prev.MachineRulesTracked != "" && !prev.Completed() {
		// The previous sync's ruledownload may have died after changing the tracking items, or may still be running
		// (e.g. Santa gave up on a slow response and started this sync): its summary cannot be trusted.
		r.MachineRulesTracked = MachineRulesTrackedDirty
	}
}
