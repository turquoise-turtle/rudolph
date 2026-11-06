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
// completed syncs sent it, so a single stray postflight cannot make Rudolph believe a Mac got what it never received
// while its postflights arrive. On a Mac whose postflights are blocked, the first fallback count can come at a
// change's first serving, so one stray postflight on the sync after it is enough; Santa's sequential stages and the
// Lambda timeout make strays all but impossible, and a forger could send two anyway.
const completedSyncsKept = 2

// Completed reports whether this sync is known to have completed: a postflight was received for it, i.e. (unless the
// postflight was a stray one) the Mac applied every rule it was sent, or the next preflight settled that its served
// last page counts as its completion (see SettleCompletion).
func (s SyncState) Completed() bool {
	return s.PostflightAt != "" || s.lastPageApplied
}

// fallbackContentServings is the fallback for an incremental sync whose last page was served but that nothing shows
// was applied. A feed rule or removal is lost (until the next clean sync) only when two syncs that carried it count as
// completed without the Mac applying either: the cursor then moves past it, and a removal counts as delivered. So:
//   - after a completed sync that did not count through the fallback (a postflight, a changed hash, or a sync that
//     served nothing new), the next sync whose last page was served counts at once: whatever it carried, it is the
//     first count to carry it;
//   - after a sync that counted through the fallback, the next one counts only once the newest content that sync
//     carried has been served by fallbackContentServings syncs (FallbackContentServings, counted from its first
//     serving with LastPageServedRun). Content first served after that sync has not been counted yet, and older
//     content has been served longer.
//
// So no content is carried by a second count before fallbackContentServings syncs served it: it is lost only after 8
// syncs in a row that served it failed after their last page was served, or 9 when the first of them is also the
// first sync whose postflight did not arrive (its last page is not recorded, so its serving is not counted), however
// the content arrives, with or without a rules hash. 8 is the smallest value with which 8 such syncs after a
// postflight lose nothing (with 7, the eighth is the second count).
//
// It also bounds the cost: whatever arrives, a sync counts at least every 8 syncs, so the feed cursor falls at most
// about 16 syncs behind, and content that leaves the Mac's rules hash unchanged even though the Mac applied it (an
// edit of a custom message or URL, which Santa leaves out of the hash, a rule written again as it was, a REMOVE of a
// rule the Mac does not hold) is sent again by up to about 15 syncs: up to 7 before the first fallback count that
// carries it, then 8 before the second.
const fallbackContentServings = 8

// LastPageServedMayCount reports whether serving this sync's last page could matter to the next sync, i.e. whether
// ruledownload records when it served it (LastPageServedAt). For a clean sync it always may (see
// LastCompletedCleanSync); clean syncs are rare, so this costs about one write per Mac per periodic clean sync. For
// other syncs only after a sync without postflight, so a Mac whose postflights arrive pays nothing on its other syncs.
func (s SyncState) LastPageServedMayCount() bool {
	return s.CleanSync || s.SyncsWithoutPostflight > 0
}

// NextContentRun returns LastPageServedRun for this sync, given whether it served content the previous sync did not
// serve: a feed row stamped after the previous sync started (PreviousPreflightAt), a removal it sent for the first
// time, or, for a clean sync, any rule (it serves rules no feed position shows as new). The run counts the syncs in a row that served the same newest content without completing: it starts again at
// 1 when this sync served newer content, or when the previous sync completed or recorded no served last page
// (ContentRun 0).
func (s SyncState) NextContentRun(servedNewContent bool) int {
	if servedNewContent {
		return 1
	}
	return s.ContentRun + 1
}

// ServesFeedRowsFirst reports whether a feed row stamped feedServedUpTo was written after the previous sync started,
// so that the previous sync cannot have served it. Without a previous sync, every row is new.
func (s SyncState) ServesFeedRowsFirst(feedServedUpTo time.Time) bool {
	previous, err := clock.ParseRFC3339(s.PreviousPreflightAt)
	return err != nil || feedServedUpTo.After(previous)
}

// coveringSync returns the completed sync that bounds what the Mac is known to hold from the rules feed and the
// removals, from the point of view of this sync:
//   - the newest completed sync proven applied (CompletedSync.Applied), if any: it reached the Mac, so the Mac holds
//     what it served, i.e. feed rows stamped before its start and every removal first sent up to it;
//   - otherwise the older of the last two completed syncs: either of the two, if it really reached the Mac (one of
//     them might be a stray postflight), read the feed up to at least its start and sent those removals;
//   - with a single completed sync, that sync. A machine's first completed sync is a clean sync that a postflight or
//     its rules hash confirmed (the fallback never counts a clean sync), so this trusts it no more than its next
//     completed sync would, which makes the cursor move to its start too.
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
// feedServedUpTo and removals first sent in syncs up to removalsFirstSentUpTo (0 when it served none), served only
// what the covering sync (see coveringSync) had already served. Counting such a sync as completed cannot lose
// anything, whether or not the Mac applied it.
//
// What this cannot see: a row written late with a stamp up to the covering sync's start (a writer whose clock was
// behind, by up to the few minutes the CLI allows, a row not yet visible to that sync's eventually consistent read,
// or a row written in the same second as that sync's start, as stamps have one-second resolution), which the
// covering sync may have missed; this is why the feed cursor overlaps (FeedSyncCursorOverlap). A sync that served
// such a row and was not applied still counts, and when the row is older than the newest completed sync's start, that
// count moves the cursor past it: on a Mac whose postflights do not arrive, the row is lost (until the next clean
// sync) if the first sync to serve it fails after its last page was served, as if a stray postflight arrived for every
// failed sync. The window is narrow: minutes for a CLI whose clock is behind, about a second otherwise. A row stamped
// in the covering sync's own second is treated as covered, as it is no more
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

// SettleCompletion decides, at the next preflight of the machine, what that preflight's rules hash shows about this
// sync: whether the Mac applied it (CompletedSync.Applied), and whether it counts as completed although no postflight
// arrived for it. Nothing may read Completed before it is called.
//
// Santa applies all the rules of a sync in one transaction after downloading its last page, and only a sync's apply
// changes a Mac's rules between two of its preflights. So when the hash the Mac reports changed since this sync's
// preflight, the Mac applied this sync, whether or not ruledownload recorded serving its last page (it does not after
// a sync whose postflight arrived). Known exotic case: Santa stops waiting for santad's apply after 300 seconds and
// goes on; an apply that only finishes after the next sync's preflight changes the hash between that sync's preflight
// and the one after, so that later sync is marked applied instead of the one whose rules changed the Mac.
//
// Santa ignores a failed postflight, so without more a Mac whose postflights never arrive (a broken postflight route,
// a proxy dropping it) would never have a completed sync: its feed cursor would never advance, and no removal would
// ever count as delivered. But a served last page is no proof that the Mac applied it, and counting such syncs as
// completed loses the feed rules and removals they carried. So the sync counts as completed when:
//   - the Mac is proven to have applied it (the hash changed);
//   - or its last page was served and it served nothing new (LastPageServedNothingNew, see ServedNothingNew):
//     counting it cannot lose anything, and it lets the feed cursor of a Mac whose rules stopped changing move on;
//   - or its last page was served, it is not a clean sync, and the fallback allows it (see fallbackContentServings).
//     A clean sync never counts this way: until one completes, the Mac may lack every rule written before it (e.g.
//     after its sync state expired), which the incremental syncs after it would never send.
func (s *SyncState) SettleCompletion(nextRulesHash string) {
	s.appliedProven = s.PreflightRulesHash != "" && nextRulesHash != "" && nextRulesHash != s.PreflightRulesHash
	s.countedThroughFallback = false
	switch {
	case s.appliedProven:
		s.lastPageApplied = true
	case s.LastPageServedAt == "":
		s.lastPageApplied = false
	case s.LastPageServedNothingNew:
		s.lastPageApplied = true
	default:
		s.countedThroughFallback = s.PostflightAt == "" && !s.CleanSync &&
			(s.FallbackContentServings == 0 || s.FallbackContentServings >= fallbackContentServings)
		s.lastPageApplied = s.countedThroughFallback
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

// NextFallbackContentServings returns FallbackContentServings for the next sync of this machine, counting the next
// sync's serving: after a sync that counted through the fallback, one more than its run; after any other completed
// sync, 0; otherwise one more than this sync's, when this sync recorded serving its last page (a sync that did not is
// not counted as a serving, which only delays the next count).
func (s SyncState) NextFallbackContentServings() int {
	switch {
	case s.countedThroughFallback:
		return s.LastPageServedRun + 1
	case s.Completed():
		return 0
	case s.FallbackContentServings > 0 && s.LastPageServedAt != "":
		return s.FallbackContentServings + 1
	default:
		return s.FallbackContentServings
	}
}

// NextContentRunCarried returns ContentRun for the next sync of this machine: this sync's run, unless it completed or
// recorded no served last page, after which the run starts again.
func (s SyncState) NextContentRunCarried() int {
	if s.Completed() || s.LastPageServedAt == "" {
		return 0
	}
	return s.LastPageServedRun
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

// MayHoldDowngradedPolicies reports whether, from the point of view of the next sync, the Mac may still hold rules that
// were sent as SILENT_BLOCKLIST in place of SILENT_GUI_BLOCKLIST or SILENT_TTY_BLOCKLIST because its Santa predated
// them. An incremental sync never sends those rules again, so once the Mac runs a Santa that knows them, it needs a
// clean sync; they are gone only when a clean sync that sent the real policies completed. Whether this sync completed
// must already be settled (SettleCompletion). A nil sync state holds none: the next sync is a clean sync anyway.
func (s *SyncState) MayHoldDowngradedPolicies() bool {
	if s == nil || !s.HoldsDowngradedPolicies {
		return false
	}
	return !(s.CleanSync && !s.SilentBlocklistOnly && s.Completed())
}
