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

// Completed reports whether this sync is known to have completed: a postflight was received for it, i.e. (unless the
// postflight was a stray one) the Mac applied every rule it was sent, or the next preflight settled that its served
// last page counts as its completion (see SettleLastPageServed).
func (s SyncState) Completed() bool {
	return s.PostflightAt != "" || s.lastPageApplied
}

// unconfirmedLastPageCountsAfter is how many syncs in a row without postflight a sync must follow for its served last
// page to count as its completion when nothing shows whether the Mac applied it (no rules hash, see
// SettleLastPageServed).
//
// A last page served but never applied (the response was lost, or santad failed to apply it) then counts as
// delivered. A feed rule or removal is lost only when two such syncs count, so after at least
// unconfirmedLastPageCountsAfter + 2 failed syncs in a row; one counted sync, like a stray postflight, cannot lose
// anything. Raising the value delays by as many syncs the point from which a Mac whose postflights never arrive stops
// re-reading the rules feed since its last completed sync on every sync.
const unconfirmedLastPageCountsAfter = 3

// LastPageServedMayCount reports whether this sync's served last page could count as its completion, i.e. whether
// ruledownload records when it served it (LastPageServedAt). It never does after a sync that received a postflight.
func (s SyncState) LastPageServedMayCount() bool {
	if s.SyncsWithoutPostflight == 0 {
		return false
	}
	return s.PreflightRulesHash != "" || s.SyncsWithoutPostflight >= unconfirmedLastPageCountsAfter
}

// SettleLastPageServed decides whether this sync counts as completed although no postflight arrived for it, because
// ruledownload served its last page (LastPageServedAt). The next preflight of the machine calls it, with the rules
// hash the Mac reports in that preflight, before anything reads Completed.
//
// Santa ignores a failed postflight, so without this a Mac whose postflights never arrive (a broken postflight route,
// a proxy dropping it) would never have a completed sync: its feed cursor would never advance, re-sending every feed
// change since its first sync on every sync, and no removal would ever count as delivered. But a served last page is
// no proof that the Mac applied it: the response may have been lost, or santad may have failed to apply it, and
// counting such syncs as completed loses the feed rules and removals they carried.
//
// So a served last page counts when the Mac's rules hash changed between this sync's preflight and the next: Santa
// applies a sync's rules at once, after downloading its last page, so a changed hash shows it applied them. An
// unchanged hash proves nothing either way (the pages may have changed nothing on the Mac), so the sync does not count
// and what it sent is sent again, until a sync that changes the Mac's rules, or a postflight, completes. When either
// preflight carried no hash (a Santa that does not report one, or a row written before the hash was recorded), the
// page counts once the sync followed unconfirmedLastPageCountsAfter syncs without postflight.
func (s *SyncState) SettleLastPageServed(nextRulesHash string) {
	switch {
	case s.LastPageServedAt == "":
		s.lastPageApplied = false
	case s.PreflightRulesHash != "" && nextRulesHash != "":
		s.lastPageApplied = nextRulesHash != s.PreflightRulesHash
	default:
		s.lastPageApplied = s.SyncsWithoutPostflight >= unconfirmedLastPageCountsAfter
	}
}

// ruledownloadFinished reports whether this sync's ruledownload has no tracking writes still to come: it received a
// postflight, or ruledownload recorded serving the last page, which it does after its tracking writes.
func (s SyncState) ruledownloadFinished() bool {
	return s.PostflightAt != "" || s.LastPageServedAt != ""
}

// LastCompletedCleanSync returns the start of the last clean sync known to have completed, from the point of view of
// the next sync: this sync's LastCleanSync, unless this sync is a clean sync that did not complete. A nil sync state
// has none.
func (s *SyncState) LastCompletedCleanSync() string {
	if s == nil {
		return ""
	}
	if s.CleanSync && !s.Completed() {
		return s.PreviousCleanSync
	}
	return s.LastCleanSync
}

// NextSyncsWithoutPostflight returns how many syncs in a row without postflight the next sync of this machine follows.
func (s SyncState) NextSyncsWithoutPostflight() int {
	if s.PostflightAt != "" {
		return 0
	}
	return s.SyncsWithoutPostflight + 1
}

// NextSyncSeq returns the smallest identifier the next sync of this machine may have (see ContinueFrom).
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
