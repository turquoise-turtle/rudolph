package fleet

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
)

// Automatic clean syncs for rules hash outliers.
//
// Santa computes its rules hash over the rules in the order its database returns them, which is the order the Mac
// wrote them (2026.8 to 2026.10; northpolesec/santa#1272 proposes sorting). A rule sent again moves to the end, a
// clean sync writes the rules in Rudolph's order, and a Mac that syncs incrementally appends. So Macs holding the same
// rules can report different hashes, and most outliers settleOutliers finds are that and nothing else. A clean sync
// rewrites the rules in Rudolph's order and brings such a Mac back to the majority hash, so the job requests one
// before it lists the Mac, and lists only the Macs whose hash still differs afterwards.

const (
	// MaxCleanSyncRequestsPerRun bounds the clean syncs one run requests. A clean sync sends a Mac every rule, and
	// Santa uploads no events during one: a bad day (a majority that is itself wrong) must not put the whole fleet
	// through that at once. The outliers beyond it are listed, and the next run requests theirs.
	MaxCleanSyncRequestsPerRun = 100

	// noRuleChange is the rule change of a set that has none on record.
	noRuleChange = "none"

	cleanSyncAuditActor  = "job:fleet"
	cleanSyncAuditAction = "fleet.clean_sync"
	cleanSyncAuditTarget = "fleet"
)

// ruleChangeKey names a set's rule change in a Mac's AutoCleanSyncFor. A Mac gets one automatic clean sync per key:
// a later rule change is a new key, and only then may the hash differ for the same reason again.
func ruleChangeKey(at time.Time) string {
	if at.IsZero() {
		return noRuleChange
	}
	return at.UTC().Format(time.RFC3339)
}

// requestCleanSyncs decides, for each outlier, between requesting a clean sync, waiting for one, and listing the Mac.
// It returns the outliers to list, the IDs of the Macs it requested a clean sync for, and how many are waiting.
//
//   - A clean sync is requested for it, by anyone, or the job requested one for this rule change and none has
//     completed since: waiting. That includes a Mac whose requested clean sync was given up (its postflights never
//     arrive): it stays waiting until the next rule change rather than being requested on every run.
//   - The job requested one for this rule change and a clean sync completed after the request: listed, marked
//     afterCleanSync. Order was not the reason.
//   - Otherwise a clean sync is requested, up to MaxCleanSyncRequestsPerRun a run; beyond that the Mac is listed
//     unmarked.
//
// On a failed request it returns the error with the requests made so far, which the caller still has to record.
func (r *run) requestCleanSyncs(ctx context.Context, outliers []outlier) (listed []outlier, requested []string, pending int, err error) {
	for _, o := range outliers {
		key := ruleChangeKey(o.ruleChange)
		switch {
		case o.cleanSyncRequested:
			pending++
		case o.autoCleanSyncFor == key && o.cleanSyncSince:
			o.afterCleanSync = true
			listed = append(listed, o)
		case o.autoCleanSyncFor == key:
			pending++
		case len(requested) < MaxCleanSyncRequestsPerRun:
			if _, err := machineconfiguration.RequestFleetCleanSync(ctx, r.opts.Client, r.opts.StoreTable, o.id, key, r.settings.now); err != nil {
				return nil, requested, pending, fmt.Errorf("fleet: %w", err)
			}
			requested = append(requested, o.id)
		default:
			listed = append(listed, o)
		}
	}
	return listed, requested, pending, nil
}

// auditCleanSyncs records one audit entry naming every Mac the run requested a clean sync for; none when it requested
// nothing. The requests change Macs' configuration, and nobody typed a command for them.
func (r *run) auditCleanSyncs(ctx context.Context, requested []string) error {
	if len(requested) == 0 {
		return nil
	}
	noun := "Macs"
	if len(requested) == 1 {
		noun = "Mac"
	}
	entry := audit.Entry{
		Actor:  cleanSyncAuditActor,
		Action: cleanSyncAuditAction,
		Target: cleanSyncAuditTarget,
		Summary: fmt.Sprintf("requested a clean sync for %d %s whose rules hash differed from that of the Macs sent the same rules: %s",
			len(requested), noun, strings.Join(requested, ", ")),
	}
	// The run's time, like the AutoCleanSyncAt of the requests it records.
	if err := audit.RecordContext(ctx, clock.FrozenTimeProvider{Current: r.settings.now}, r.opts.Client, r.opts.StoreTable, entry); err != nil {
		// %q: the IDs are what Macs reported. The log is then the only record of who was requested.
		log.Printf("fleet: clean syncs were requested but could not be recorded in the audit trail, for %q", requested)
		return fmt.Errorf("fleet: %w", err)
	}
	return nil
}
