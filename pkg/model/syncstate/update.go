package syncstate

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
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
		ExpiresAfter: GetSyncStateExpiresAfter(timeProvider),
	}
}

// ErrSyncSuperseded is returned when the sync state row no longer belongs to the sync updating it: a newer sync of
// the machine started (or the summary was changed by someone else) in the meantime.
var ErrSyncSuperseded = errors.New("the sync state was replaced by a newer sync of the machine")

// SetMachineRulesTracked replaces the tracking summary (see SyncState.MachineRulesTracked), and the time the tracking
// items' expiry was extended when refreshedAt is not empty. It only does so while the row still belongs to the sync
// syncSeq and the summary is still expected ("" for none); otherwise it returns ErrSyncSuperseded and changes nothing.
func SetMachineRulesTracked(client dynamodb.UpdateItemIfAPI, machineID string, syncSeq int, expected, summary, refreshedAt string) error {
	condition := expression.Equal(expression.Name("SyncSeq"), expression.Value(syncSeq))
	if expected == "" {
		condition = condition.And(expression.AttributeNotExists(expression.Name("MachineRulesTracked")))
	} else {
		condition = condition.And(expression.Equal(expression.Name("MachineRulesTracked"), expression.Value(expected)))
	}
	_, err := client.UpdateItemIf(
		dynamodb.PrimaryKey{
			PartitionKey: syncStatePK(machineID),
			SortKey:      syncStateSK,
		},
		updateMachineRulesTrackedItem{MachineRulesTracked: summary, MachineRulesTrackingRefreshedAt: refreshedAt},
		condition,
	)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrSyncSuperseded
	}
	if err != nil {
		return fmt.Errorf("failed to record the machine rules tracked: %w", err)
	}
	return nil
}
