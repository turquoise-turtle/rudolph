package preflight

import (
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
)

func (c concreteStateTrackingService) getFeedSyncStateCursor(syncState *syncstate.SyncStateRow) (feedSyncCursor string, performCleanSync bool) {
	// Check when the last preflight request took place
	if syncState != nil {
		// Pick up where earlier syncs left off: from where the syncs known to be completed started, and otherwise from
		// where the previous sync read the feed itself, so that nothing that was not delivered is skipped
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
