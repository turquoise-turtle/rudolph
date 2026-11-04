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

// InvalidateMachineRulesTracked replaces the tracking summary with MachineRulesTrackedOvertaken, whichever sync the
// row belongs to, so that the next sync reads the tracking items instead of trusting the summary. A ruledownload calls
// it when it may have changed tracking items after a newer sync of the machine recorded its own summary. It changes
// nothing when the row has no summary (delivery not tracked yet, which already makes every sync read the items) or no
// longer exists.
func InvalidateMachineRulesTracked(client dynamodb.UpdateItemIfAPI, machineID string) error {
	_, err := client.UpdateItemIf(
		dynamodb.PrimaryKey{
			PartitionKey: syncStatePK(machineID),
			SortKey:      syncStateSK,
		},
		updateMachineRulesTrackedOnlyItem{MachineRulesTracked: MachineRulesTrackedOvertaken},
		expression.AttributeExists(expression.Name("MachineRulesTracked")),
	)
	if err != nil && !dynamodb.IsConditionalCheckFailed(err) {
		return fmt.Errorf("failed to invalidate the machine rules tracked: %w", err)
	}
	return nil
}

// MarkLastPageServed records that ruledownload served the last page of the sync syncSeq (see
// SyncState.LastPageServedAt), whether the sync served nothing new (see SyncState.ServedNothingNew) and its content run
// (see SyncState.NextContentRun). It returns ErrSyncSuperseded and changes nothing when the row belongs to another sync.
func MarkLastPageServed(timeProvider clock.TimeProvider, client dynamodb.UpdateItemIfAPI, machineID string, syncSeq int, nothingNew bool, run int) error {
	_, err := client.UpdateItemIf(
		dynamodb.PrimaryKey{
			PartitionKey: syncStatePK(machineID),
			SortKey:      syncStateSK,
		},
		updateLastPageServedItem{LastPageServedAt: clock.RFC3339(timeProvider.Now()), LastPageServedNothingNew: nothingNew, LastPageServedRun: run},
		expression.Equal(expression.Name("SyncSeq"), expression.Value(syncSeq)),
	)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrSyncSuperseded
	}
	if err != nil {
		return fmt.Errorf("failed to record that the last page was served: %w", err)
	}
	return nil
}
