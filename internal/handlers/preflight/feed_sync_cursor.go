package preflight

import (
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
)

func (c concreteStateTrackingService) getFeedSyncStateCursor(syncState *syncstate.SyncStateRow) (feedSyncCursor string, performCleanSync bool) {
	// Check when the last preflight request took place
	if syncState != nil {
		// Pick up where the previous sync left off: from where it started if it completed, and otherwise from where
		// it read the feed itself, so that nothing it did not deliver is skipped
		feedSyncCursor = syncState.NextFeedSyncCursor()
	}
	if feedSyncCursor == "" {
		// If there is no previous sync state, or if no cursor exists.
		// Always force it to clean sync and just set the feed sync cursor to "now"
		feedSyncCursor = clock.RFC3339(c.timeProvider.Now())
		performCleanSync = true
	}

	return
}

// machineRulesDelivered returns the machine-specific rules the Mac may hold as this sync starts, as far as Rudolph
// knows. A machine without a previous sync state has received none.
func machineRulesDelivered(prevSyncState *syncstate.SyncStateRow) syncstate.MachineRuleKeys {
	if prevSyncState == nil {
		return syncstate.NewMachineRuleKeys()
	}
	return prevSyncState.NextMachineRulesDelivered()
}
