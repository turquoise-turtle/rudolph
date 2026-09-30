package syncstate

import (
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
)

// UpdatePostflightDate closes out a sync the client reported nothing about. See UpdatePostflightItem.
func UpdatePostflightDate(timeProvider clock.TimeProvider, client dynamodb.UpdateItemAPI, machineID string) (err error) {
	_, err = client.UpdateItem(
		dynamodb.PrimaryKey{
			PartitionKey: syncStatePK(machineID),
			SortKey:      syncStateSK,
		},
		newUpdatePostflightItem(timeProvider),
	)

	if err != nil {
		return fmt.Errorf("failed to update item: %w", err)
	}
	return
}

// UpdatePostflightOutcome closes out a sync: it marks the sync as completed, and records what the client reported
// about the sync it just finished: the sync type it performed, and how many rules it received and successfully
// applied. See UpdatePostflightOutcomeItem.
func UpdatePostflightOutcome(
	timeProvider clock.TimeProvider,
	client dynamodb.UpdateItemAPI,
	machineID string,
	lastSyncType string,
	rulesReceived int,
	rulesProcessed int,
	rulesHashAfterSync string,
) (err error) {
	_, err = client.UpdateItem(
		dynamodb.PrimaryKey{
			PartitionKey: syncStatePK(machineID),
			SortKey:      syncStateSK,
		},
		UpdatePostflightOutcomeItem{
			PostflightAt:       clock.RFC3339(timeProvider.Now()),
			ExpiresAfter:       GetSyncStateExpiresAfter(timeProvider),
			LastSyncType:       lastSyncType,
			RulesReceived:      rulesReceived,
			RulesProcessed:     rulesProcessed,
			RulesHashAfterSync: rulesHashAfterSync,
		},
	)

	if err != nil {
		return fmt.Errorf("failed to update item: %w", err)
	}
	return
}

func newUpdatePostflightItem(timeProvider clock.TimeProvider) UpdatePostflightItem {
	return UpdatePostflightItem{
		PostflightAt: clock.RFC3339(timeProvider.Now()),
		// Not read for a completed sync: the next preflight derives the cursor from PreflightAt (NextFeedSyncCursor).
		FeedSyncCursor: clock.RFC3339(timeProvider.Now().Add(time.Minute * -10)),
		ExpiresAfter:   GetSyncStateExpiresAfter(timeProvider),
	}
}

// UpdateMachineRulesTracked records the digest of the machine-specific rules tracked as possibly held by the Mac,
// after ruledownload changed them (see SyncState.MachineRulesTracked).
//
// The update is conditional on the sync state row existing, so a machine without one (e.g. whose row was deleted
// mid-sync) fails the ruledownload rather than getting a partial row.
func UpdateMachineRulesTracked(client dynamodb.UpdateItemAPI, machineID string, digest string) error {
	_, err := client.UpdateItem(
		dynamodb.PrimaryKey{
			PartitionKey: syncStatePK(machineID),
			SortKey:      syncStateSK,
		},
		updateMachineRulesTrackedItem{MachineRulesTracked: digest},
	)
	if err != nil {
		return fmt.Errorf("failed to record the machine rules tracked: %w", err)
	}
	return nil
}
