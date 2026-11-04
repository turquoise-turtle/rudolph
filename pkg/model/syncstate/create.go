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
// or its row expired); the Mac then holds no machine-specific rules from Rudolph once its sync, a clean sync, completes.
// Whether prev completed must already be settled (SettleCompletion).
//
// The tracking items of a machine whose sync state expired may still exist (they live longer, see
// machinerules.DeliveryExpiry), and stand for rules the Mac still holds until that clean sync completes. When their
// expiry was last extended is lost with the sync state, so the refresh time is left empty: the next ruledownload
// extends the expiry of every item kept, rather than letting items written before the gap expire while their rules
// may still be on the Mac.
func (r *SyncStateRow) ContinueFrom(prev *SyncStateRow, emptyMachineRulesDigest string, now time.Time) {
	r.SyncSeq = int(now.UnixMilli())
	if prev == nil {
		r.MachineRulesTracked = emptyMachineRulesDigest
		return
	}
	if next := prev.NextSyncSeq(); next > r.SyncSeq {
		r.SyncSeq = next
	}
	if r.CleanSync {
		r.PreviousCleanSync = prev.LastCompletedCleanSync()
	}
	r.SyncsWithoutPostflight = prev.NextSyncsWithoutPostflight()
	r.PreviousPreflightAt = prev.PreflightAt
	r.ContentRun = prev.NextContentRunCarried()
	r.CompletedSyncs = prev.NextCompletedSyncs()
	r.MachineRulesTracked = prev.MachineRulesTracked
	r.MachineRulesTrackingRefreshedAt = prev.MachineRulesTrackingRefreshedAt
	if prev.MachineRulesTracked != "" && !prev.ruledownloadFinished() {
		// The previous sync's ruledownload may have died after changing the tracking items, or may still be running
		// (e.g. Santa gave up on a slow response and started this sync): its summary cannot be trusted. A served last
		// page is enough here even when it does not count as the sync's completion: it was recorded after the tracking
		// writes, so the summary matches the items.
		r.MachineRulesTracked = MachineRulesTrackedDirty
	}
}
