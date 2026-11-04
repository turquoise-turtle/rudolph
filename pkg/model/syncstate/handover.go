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
// last page counts as its completion (see SettleCompletion).
func (s SyncState) Completed() bool {
	return s.PostflightAt != "" || s.lastPageApplied
}

// Fallbacks for a served last page that nothing shows was applied, counted in syncs before it since the Mac's last
// completed sync of any kind (SyncsSinceCompleted). Each fallback count resets it, so every count needs fresh
// unconfirmed syncs. A last page served but never applied (the response was lost, or santad failed to apply it) is
// then taken as delivered. A feed rule or removal is lost only when two such syncs count after it was first sent, so
// after 2 × (fallback + 1) syncs in a row whose last page was served but not applied; one counted sync, like a stray
// postflight, cannot lose anything.
const (
	// For a Mac that reports no rules hash, nothing shows that it applied a sync, so without postflights its syncs
	// first count through this. 3 is the largest value that keeps the cost of such a Mac bounded from its sixth sync
	// on (one sync counts through it, the next as serving nothing new, and then the feed cursor moves); loss needs 8
	// failed syncs in a row.
	unconfirmedLastPageCountsAfter = 3
	// For a Mac that reports a rules hash which did not change. A changed hash, or a sync that served nothing new,
	// already counts; this only bounds what is sent again when a sync's content leaves the hash unchanged even when
	// applied (an edit of a custom message or URL, which Santa leaves out of the hash, a rule the Mac already held, or
	// the REMOVE of a rule it may hold but does not). Such content is sent again by up to 2 × (4 + 1) = 10 syncs. 4 is
	// the smallest value with which 8 failed syncs in a row lose nothing (loss needs 10: with 3 it would need 8).
	unchangedHashLastPageCountsAfter = 4
)

// LastPageServedMayCount reports whether serving this sync's last page could matter to the next sync, i.e. whether
// ruledownload records when it served it (LastPageServedAt). For a clean sync it always may (see
// LastCompletedCleanSync and SettleCompletion); clean syncs are rare, so this costs about one write per Mac per
// periodic clean sync. For other syncs only after a sync without postflight, so a Mac whose postflights arrive pays
// nothing on its other syncs.
func (s SyncState) LastPageServedMayCount() bool {
	return s.CleanSync || s.SyncsWithoutPostflight > 0
}

// coveringSync returns the completed sync that bounds what the Mac is known to hold from the rules feed and the
// removals, from the point of view of this sync:
//   - the newest completed sync proven applied (CompletedSync.Applied), if any: it reached the Mac, so the Mac holds
//     what it served, i.e. feed rows stamped before its start and every removal first sent up to it;
//   - otherwise the older of the last two completed syncs: either of the two, if it really reached the Mac (one of
//     them might be a stray postflight), read the feed up to at least its start and sent those removals;
//   - with a single completed sync (a new Mac), that sync; that trusts it no more than its next completed sync would,
//     which makes the cursor move to its start too.
func (s SyncState) coveringSync() (CompletedSync, bool) {
	for _, completed := range s.CompletedSyncs {
		if completed.Applied {
			return completed, true
		}
	}
	switch len(s.CompletedSyncs) {
	case 0:
		return CompletedSync{}, false
	case 1:
		return s.CompletedSyncs[0], true
	default:
		return s.CompletedSyncs[completedSyncsKept-1], true
	}
}

// ServedNothingNew reports whether this incremental sync, which served the rules feed up to the row stamped
// feedServedUpTo and removals that the Mac may act on first sent in syncs up to removalsFirstSentUpTo (0 when it
// served none), served only what the covering sync (see coveringSync) had already served. Counting such a sync as
// completed cannot lose anything, whether or not the Mac applied it.
//
// What this cannot see: a row written late with a stamp up to the covering sync's start (a writer whose clock was
// behind, by up to the few minutes the CLI allows, a row not yet visible to that sync's eventually consistent read,
// or a row written in the same second as that sync's start, as stamps have one-second resolution), which the
// covering sync may have missed; this is why the feed cursor overlaps (FeedSyncCursorOverlap). A sync that served
// such a row and was not applied still counts. The row is lost (until the next clean sync) only when two such syncs
// count in a row, i.e. the Mac failed to apply two consecutive syncs just after a late write into the covering
// sync's overlap window. A row stamped in the covering sync's own second is treated as covered, as it is no more
// likely to be late than one stamped a minute earlier; leaving it out would make a Mac whose postflights never arrive
// wait for a fallback count after every rule written in the second its covering sync started.
func (s SyncState) ServedNothingNew(feedServedUpTo time.Time, removalsFirstSentUpTo int) bool {
	covering, ok := s.coveringSync()
	if !ok {
		return false
	}
	coveredUntil, err := clock.ParseRFC3339(covering.PreflightAt)
	if err != nil {
		return false
	}
	return !feedServedUpTo.After(coveredUntil) && removalsFirstSentUpTo <= covering.Seq
}

// RemovalMayChangeTheMac reports whether sending the removal of a machine rule first sent in sync firstSentInSync (0
// when unknown) may change the Mac's rules, so whether a sync that sends it may count as having served nothing new
// only once the removal is covered.
//
// For a Mac that reports a rules hash, a rule it got from a sync it applied changed its hash, so that sync, or a later
// one, is proven applied (AppliedThroughSyncSeq). When no sync proven applied since the rule was first sent exists, the
// Mac never received it (or already held an identical rule): its REMOVE changes nothing on the Mac, and waiting for a
// changed hash or a covering sync would freeze its feed cursor and send the REMOVE for ever. A rule first sent before
// this was tracked, or sent again while its removal was pending, may be held and its removal counts.
func (s SyncState) RemovalMayChangeTheMac(firstSentInSync int) bool {
	return s.PreflightRulesHash == "" || firstSentInSync == 0 || s.AppliedThroughSyncSeq >= firstSentInSync
}

// SettleCompletion decides, at the next preflight of the machine, what that preflight's rules hash shows about this
// sync: whether the Mac applied it (CompletedSync.Applied, AppliedThroughSyncSeq), and whether it counts as completed
// although no postflight arrived for it. Nothing may read Completed before it is called.
//
// Santa applies all the rules of a sync in one transaction after downloading its last page, so when the hash the Mac
// reports changed since this sync's preflight and the sync's last page was served (or its postflight arrived), the Mac
// applied this sync.
//
// Santa ignores a failed postflight, so without more a Mac whose postflights never arrive (a broken postflight route,
// a proxy dropping it) would never have a completed sync: its feed cursor would never advance, and no removal would
// ever count as delivered. But a served last page is no proof that the Mac applied it, and counting such syncs as
// completed loses the feed rules and removals they carried. So a served last page counts as the sync's completion
// when:
//   - the Mac is proven to have applied it (the hash changed);
//   - or the sync served nothing new (LastPageServedNothingNew, see ServedNothingNew): counting it cannot lose
//     anything, and it lets the feed cursor of a Mac whose rules stopped changing move on;
//   - or the sync followed enough unconfirmed syncs (SyncsSinceCompleted): unconfirmedLastPageCountsAfter when either
//     preflight carried no hash (a Santa that does not report one, or a row written before the hash was recorded),
//     unchangedHashLastPageCountsAfter when the hash did not change.
func (s *SyncState) SettleCompletion(nextRulesHash string) {
	served := s.PostflightAt != "" || s.LastPageServedAt != ""
	hashed := s.PreflightRulesHash != "" && nextRulesHash != ""
	s.appliedProven = served && hashed && nextRulesHash != s.PreflightRulesHash
	switch {
	case s.LastPageServedAt == "":
		s.lastPageApplied = false
	case s.appliedProven, s.LastPageServedNothingNew:
		s.lastPageApplied = true
	case !hashed:
		s.lastPageApplied = s.SyncsSinceCompleted >= unconfirmedLastPageCountsAfter
	default:
		s.lastPageApplied = s.SyncsSinceCompleted >= unchangedHashLastPageCountsAfter
	}
}

// ruledownloadFinished reports whether this sync's ruledownload has no tracking writes still to come: it received a
// postflight, or ruledownload recorded serving the last page, which it does after its tracking writes.
func (s SyncState) ruledownloadFinished() bool {
	return s.PostflightAt != "" || s.LastPageServedAt != ""
}

// LastCompletedCleanSync returns the start of the last clean sync that counts as performed, from the point of view of
// the next sync, which uses it to schedule the periodic clean sync: this sync's LastCleanSync, unless this sync is a
// clean sync that does not count. A nil sync state has none.
//
// A clean sync counts when it completed, or when its last page was served, the Mac was already missing postflights
// (SyncsWithoutPostflight) and an earlier clean sync counted (PreviousCleanSync). The second case is for a Mac whose
// postflights never arrive: a clean sync that sends the rules the Mac already holds leaves its rules hash unchanged,
// so it rarely counts as completed soon, and without this the following syncs would be clean syncs again (every
// rule, on every sync). Counting it for the schedule only cannot lose a rule even if the Mac never applied it: what
// the feed cursor and removals rely on is still the completed syncs, and the Mac got every rule through an earlier
// clean sync and the syncs since; at worst the periodic refresh comes one period late. Without an earlier clean sync,
// the Mac may never have received the rules written before its first one, so an unconfirmed clean sync does not count.
// A Mac whose postflights arrive gets a postflight for a clean sync it performed, so one that did not complete (e.g.
// its last response was lost) is retried at the next sync.
func (s *SyncState) LastCompletedCleanSync() string {
	if s == nil {
		return ""
	}
	if s.CleanSync && !s.Completed() && (s.LastPageServedAt == "" || s.PreviousCleanSync == "" || s.SyncsWithoutPostflight == 0) {
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

// NextSyncsSinceCompleted returns how many syncs in a row that did not complete the next sync of this machine follows.
func (s SyncState) NextSyncsSinceCompleted() int {
	if s.Completed() {
		return 0
	}
	return s.SyncsSinceCompleted + 1
}

// NextAppliedThroughSyncSeq returns AppliedThroughSyncSeq for the next sync of this machine, numbered nextSyncSeq. When
// this sync is proven applied, no sync of the machine lies between it and the next one, so every sync numbered below
// nextSyncSeq that sent a rule did so no later than this one.
func (s SyncState) NextAppliedThroughSyncSeq(nextSyncSeq int) int {
	if s.appliedProven {
		return nextSyncSeq - 1
	}
	return s.AppliedThroughSyncSeq
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
		completed = append([]CompletedSync{{Seq: s.SyncSeq, PreflightAt: s.PreflightAt, Applied: s.appliedProven}}, completed...)
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
