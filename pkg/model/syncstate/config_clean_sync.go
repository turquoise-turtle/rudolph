package syncstate

// ConfigCleanSyncServingsToConfirm is how many clean syncs for the same request (the clean sync flag of a machine
// config) must have had their last page served, without a postflight or a changed rules hash, before the request
// counts as performed.
//
// A requested clean sync is confirmed by the same evidence as any sync: its postflight, or a rules hash in the next
// preflight that differs from the one in its own preflight (the Mac applied it). Neither may ever come for a Mac whose
// postflights do not arrive, when the clean sync sent exactly the rules the Mac already held (its hash does not change
// although it applied them). A served last page alone is no proof: the response may have been lost. So the clean sync
// is repeated, and after this many served without either confirmation the request counts as performed: either the
// Mac already held what the clean sync sends (it applied a clean sync without its rules hash changing), or every one
// of these responses was lost. A clean sync whose last page was never served (it failed earlier) is not counted, so
// a requested clean sync that did not happen is never forgotten; one that did happen repeats at most this many times.
const ConfigCleanSyncServingsToConfirm = 3

// ConfigCleanSyncProgress describes a clean sync performed for the clean sync flag of a machine config: the request it
// performs (see SyncState.ConfigCleanSyncRequest), and how many earlier clean syncs for it were served unconfirmed.
type ConfigCleanSyncProgress struct {
	Request string
	Served  int
}

// ConfigCleanSyncOutcome tells the next preflight, while the machine config's clean sync flag still holds request,
// whether this sync confirmed it, and else how many clean syncs for it have been served unconfirmed so far (this one
// included, when its last page was served). Whether this sync completed must already be settled (SettleCompletion).
// A nil sync state, or one that did not perform this request (a request made after it started, or a sync that did not
// honour the flag), confirms nothing and starts the count again.
func (s *SyncState) ConfigCleanSyncOutcome(request string) (confirmed bool, served int) {
	if s == nil || !s.ConfigCleanSync || s.ConfigCleanSyncRequest != request {
		return false, 0
	}
	if s.Completed() {
		// A postflight arrived (whose own attempt to clear the flag failed or raced with this preflight), or the
		// rules hash changed: the Mac applied the clean sync
		return true, 0
	}
	served = s.ConfigCleanSyncServed
	if s.LastPageServedAt != "" {
		served++
	}
	return served >= ConfigCleanSyncServingsToConfirm, served
}

// SetConfigCleanSync records that this sync is a clean sync performing the given request.
func (r *SyncStateRow) SetConfigCleanSync(progress ConfigCleanSyncProgress) {
	r.ConfigCleanSync = true
	r.ConfigCleanSyncRequest = progress.Request
	r.ConfigCleanSyncServed = progress.Served
}
