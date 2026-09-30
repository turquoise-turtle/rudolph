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
// are sent twice, which Santa applies idempotently. The CLI refuses to write rules when its clock is off by more than
// a few minutes (see internal/cli/clockcheck).
const FeedSyncCursorOverlap = 10 * time.Minute

// completedSyncsKept is how many completed syncs a sync state remembers. A change counts as delivered once two
// completed syncs sent it, so a single stray postflight cannot make Rudolph believe a Mac got what it never received.
const completedSyncsKept = 2

// Completed reports whether a postflight was received for this sync, i.e. (unless the postflight was a stray one) the
// Mac applied every rule it was sent.
func (s SyncState) Completed() bool {
	return s.PostflightAt != ""
}

// NextSyncSeq returns the number of the next sync of this machine. Syncs are numbered from 1.
func (s SyncState) NextSyncSeq() int {
	return s.SyncSeq + 1
}

// NextCompletedSyncs returns the completed syncs the next sync of this machine starts with: this sync if it
// completed, then the most recent earlier ones, newest first.
func (s SyncState) NextCompletedSyncs() []CompletedSync {
	completed := append([]CompletedSync(nil), s.CompletedSyncs...)
	if s.Completed() {
		completed = append([]CompletedSync{{Seq: s.SyncSeq, PreflightAt: s.PreflightAt}}, completed...)
	}
	if len(completed) > completedSyncsKept {
		completed = completed[:completedSyncsKept]
	}
	return completed
}

// NextFeedSyncCursor returns the feed cursor the next sync of this machine reads from.
//
// Two completed syncs are needed to advance the cursor to FeedSyncCursorOverlap before the start of the older one: at
// least one of them really reached the Mac, and each read the feed up to its own start. With one completed sync, the
// cursor moves to the overlap before its start only if that is earlier than the current cursor (the cursor of a
// machine's first, clean, sync is the time it started). Without any, the cursor does not move. An empty cursor
// (e.g. an unfinished requested clean sync) makes the next sync a clean sync.
func (s SyncState) NextFeedSyncCursor() string {
	completed := s.NextCompletedSyncs()
	switch len(completed) {
	case 0:
		return s.FeedSyncCursor
	case 1:
		cursor, ok := cursorBefore(completed[0])
		if !ok || (s.FeedSyncCursor != "" && s.FeedSyncCursor < cursor) {
			return s.FeedSyncCursor
		}
		return cursor
	default:
		if cursor, ok := cursorBefore(completed[1]); ok {
			return cursor
		}
		return s.FeedSyncCursor
	}
}

func cursorBefore(sync CompletedSync) (string, bool) {
	preflightAt, err := clock.ParseRFC3339(sync.PreflightAt)
	if err != nil {
		return "", false
	}
	return clock.RFC3339(preflightAt.Add(-FeedSyncCursorOverlap)), true
}

// DeliveryConfirmed reports whether a change first sent by sync firstSentInSync, and by every sync after it, has been
// delivered: two syncs that sent it have completed since.
func DeliveryConfirmed(firstSentInSync int, completed []CompletedSync) bool {
	return len(completed) >= completedSyncsKept && completed[completedSyncsKept-1].Seq >= firstSentInSync
}
