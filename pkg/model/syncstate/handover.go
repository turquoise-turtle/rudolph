package syncstate

import (
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
)

// FeedSyncCursorOverlap is how far before the start of a sync the next sync starts reading the rules feed.
//
// Feed rows are sorted by the time their writer (a Lambda, or the CLI on an admin's machine) thought it was, and a
// row becomes visible to eventually consistent reads a little after it is written. A sync that started at T can
// therefore miss a row stamped shortly before T, so the next sync reads from T minus this overlap. Rules in the overlap
// are sent twice, which Santa applies idempotently.
const FeedSyncCursorOverlap = 10 * time.Minute

// Completed reports whether the client confirmed this sync with a postflight, i.e. it applied every rule it was sent.
// A sync that never reaches postflight (aborted, timed out, crashed) is not completed, and the next sync repeats what
// it was meant to deliver.
func (s SyncState) Completed() bool {
	return s.PostflightAt != ""
}

// NextFeedSyncCursor returns the feed cursor the next sync of this machine reads from.
//
// A completed sync advances the cursor to FeedSyncCursorOverlap before this sync's preflight: every feed row stamped
// earlier was either read by this sync or is older than the overlap. A sync that did not complete does not advance
// it. An empty cursor (e.g. an unfinished requested clean sync) makes the next sync a clean sync.
func (s SyncState) NextFeedSyncCursor() string {
	if s.Completed() {
		if preflightAt, err := clock.ParseRFC3339(s.PreflightAt); err == nil {
			return clock.RFC3339(preflightAt.Add(-FeedSyncCursorOverlap))
		}
	}
	return s.FeedSyncCursor
}

// NextMachineRulesDelivered returns the machine-specific rules the Mac may hold when the next sync of this machine
// starts: what this sync served if it completed.
//
// If it did not complete, the Mac may or may not have applied what it was served (e.g. only the postflight request
// was lost), so it may hold both what it held before this sync and what this sync served. Keeping every such rule is
// safe: the next sync sends each one that no longer applies as the global rule, or as a REMOVE when there is no
// global rule (which Santa ignores for a rule it does not have).
//
// The result is unknown for rows written by versions of Rudolph that did not track delivery.
func (s SyncState) NextMachineRulesDelivered() MachineRuleKeys {
	if !s.MachineRulesServed.Known() {
		return s.MachineRulesDelivered
	}
	if s.Completed() {
		return s.MachineRulesServed
	}
	return s.MachineRulesDelivered.Union(s.MachineRulesServed)
}
