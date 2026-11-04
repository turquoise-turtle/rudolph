package syncstate

// ConfigCleanSyncServingsToConfirm is how many clean syncs for the same request (the clean sync flag of a machine
// config) must have had their last page served, without a postflight or a changed rules hash, before the request is
// given up, on a Mac that was already missing postflights when the request's first clean sync started.
//
// A requested clean sync is confirmed by the same evidence as any sync: its postflight, or a rules hash in the next
// preflight that differs from the one in its own preflight (the Mac applied it). On a Mac whose postflights arrive, a
// clean sync it applied gets a postflight, so nothing else counts there: a clean sync that keeps failing after its
// last page was served (e.g. its response is lost, or santad fails to apply it) is retried until one completes.
//
// Neither may ever come for a Mac whose postflights do not arrive, when the clean sync sent exactly the rules the Mac
// already held (its hash does not change although it applied them). A served last page alone is no proof: the
// response may have been lost. So the clean sync is repeated, and after this many served without either
// confirmation the request is given up: either the Mac already held what the clean sync sends, or every one of these
// responses was lost. A clean sync whose last page was never served (it failed earlier) is not counted, so a
// requested clean sync that did not happen is never forgotten; one that did happen repeats at most this many times.
//
// Whether the Mac is one whose postflights do not arrive is judged once, at the request's first clean sync
// (SyncsWithoutPostflight > 0, like the schedule in LastCompletedCleanSync), not at each attempt: every failed
// attempt adds a sync without postflight, so judging each attempt would treat a Mac whose postflights arrive as one
// whose postflights do not after its first failed attempt. The cost of judging once: a Mac whose postflights stop
// arriving after the request was made keeps clean syncing until a postflight arrives, its rules hash changes, or the
// request is cancelled (`rudolph config update --machine <id> --clean-sync=false`).
const ConfigCleanSyncServingsToConfirm = 3

// ConfigCleanSyncProgress describes a clean sync performed for the clean sync flag of a machine config: the request it
// performs (see SyncState.ConfigCleanSyncRequest), how many earlier clean syncs for it were served unconfirmed, and
// whether such servings count (CountsServings; decided by SetConfigCleanSync at the request's first clean sync, when
// Continues is false).
type ConfigCleanSyncProgress struct {
	Request        string
	Served         int
	CountsServings bool
	Continues      bool
}

// ConfigCleanSyncOutcome tells the next preflight, while the machine config's clean sync flag still holds request,
// whether this sync confirmed it (or the request is given up, see ConfigCleanSyncServingsToConfirm), and else how
// the request's next clean sync continues. Whether this sync completed must already be settled (SettleCompletion).
// A nil sync state, or one that did not perform this request (a request made after it started, or a sync that did
// not honour the flag), confirms nothing and starts a new request.
//
// A flag stored without a request identifier (set by hand or by an earlier version of Rudolph) cannot be told apart
// from the same flag set again after its clean sync completed. Postflight clears the flag of a clean sync it closes,
// so when this sync received a postflight, a flag without identifier still set is taken as set again: it gets a clean
// sync of its own (or one more, if the postflight failed to clear it).
func (s *SyncState) ConfigCleanSyncOutcome(request string) (confirmed bool, givenUp bool, next ConfigCleanSyncProgress) {
	next = ConfigCleanSyncProgress{Request: request}
	if s == nil || !s.ConfigCleanSync || s.ConfigCleanSyncRequest != request || (request == "" && s.PostflightAt != "") {
		return false, false, next
	}
	if s.Completed() {
		// A postflight arrived (whose own attempt to clear the flag failed or raced with this preflight), or the
		// rules hash changed: the Mac applied the clean sync
		return true, false, next
	}
	next.Continues = true
	next.CountsServings = s.ConfigCleanSyncCountsServings
	next.Served = s.ConfigCleanSyncServed
	if s.CountsServingsOfLastPage() {
		next.Served++
	}
	if next.Served >= ConfigCleanSyncServingsToConfirm {
		return true, true, next
	}
	return false, false, next
}

// CountsServingsOfLastPage reports whether this requested clean sync's served last page counts towards giving the
// request up (see ConfigCleanSyncServingsToConfirm)
func (s SyncState) CountsServingsOfLastPage() bool {
	return s.ConfigCleanSyncCountsServings && s.LastPageServedAt != ""
}

// SetConfigCleanSync records that this sync is a clean sync performing the given request. At the request's first
// clean sync it decides whether served last pages count: only when the Mac was already missing postflights. Call it
// after ContinueFrom, which sets SyncsWithoutPostflight.
func (r *SyncStateRow) SetConfigCleanSync(progress ConfigCleanSyncProgress) {
	if !progress.Continues {
		progress.CountsServings = r.SyncsWithoutPostflight > 0
		progress.Served = 0
	}
	r.ConfigCleanSync = true
	r.ConfigCleanSyncRequest = progress.Request
	r.ConfigCleanSyncServed = progress.Served
	r.ConfigCleanSyncCountsServings = progress.CountsServings
}
