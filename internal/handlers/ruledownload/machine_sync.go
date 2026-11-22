package ruledownload

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	"github.com/airbnb/rudolph/pkg/response"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

// The machine-specific rules of a sync are sent at its end, after the global rules (clean sync) or the rules feed
// (incremental sync), so that they take precedence over them: on the last global or feed page, and on further pages
// when they do not fit it (see machineTail). They are re-sent on every sync.
//
// A machine rule that stops applying (it expired, was marked for removal, or its row is gone, e.g. deleted by the
// table's TTL) must also leave the Mac. Rudolph tracks which machine rules the Mac may hold (see
// machinerules.GetDeliveries); each of those that no longer applies is sent as the global rule for the same
// identifier and type if there is one, so the Mac falls back to the fleet rule, and as a REMOVE otherwise, until two
// completed syncs have sent it (see syncstate.DeliveryConfirmed).
//
// FIXME(derek.wang)
//
//	we don't necessarily have to do it this way; because this makes it really annoying to remove machine rules, as we need complex logic to
//	DELETE them. We could, as an alternative design, simply add machine-specific rules to the feed, and add an extra column to make the rule
//	pertinent only to a single machine. We can use filter expressions to omit them from other machines. However, this design makes it very
//	hard to figure out which rules belong on which machines.
type machineRuleDownloder interface {
	handle(machineID string, ruledownloadRequest *RuledownloadRequest) (*events.APIGatewayProxyResponse, error)
}

// concreteMachineRuleDownloader serves a page holding only the machine-specific rules, for a cursor with the machine
// strategy. Current syncs get them from their last global or feed page on, under that page's strategy; a sync is only
// on this strategy when an earlier version of Rudolph issued its cursor.
type concreteMachineRuleDownloader struct {
	machineRules machineRulePage
}

func (d concreteMachineRuleDownloader) handle(machineID string, ruledownloadRequest *RuledownloadRequest) (*events.APIGatewayProxyResponse, error) {
	log.Printf("  Ruledownload machine page")

	cursor := ruledownloadCursor{}
	if ruledownloadRequest != nil && ruledownloadRequest.Cursor != nil {
		cursor = *ruledownloadRequest.Cursor
	}
	cursor.Strategy = ruledownloadStrategyMachine
	// Earlier versions issued machine cursors with a key, which no machine page reads; it is not handed back
	cursor.ruledownloadCursorDDBLastEvaluatedKey = ruledownloadCursorDDBLastEvaluatedKey{}
	syncState, err := syncstate.GetByMachineID(d.machineRules.getter, machineID)
	if err != nil {
		return response.InternalServerError(fmt.Errorf("reading the sync state: %w", err), response.ErrInternalServerErrorResponse)
	}
	if syncState == nil {
		return syncErrorResponse("serving the machine rules page", errNoSyncState)
	}
	if cursor.SyncSeq == 0 {
		// Earlier versions of Rudolph issued machine cursors without seq, so one without seq is taken as the current
		// sync's
		cursor.SyncSeq = syncState.SyncSeq
	}
	cursor.syncState = syncState
	cursor.SilentBlocklistOnly = syncState.SilentBlocklistOnly
	return lastPageOrNext(machineID, cursor, nil, nil, d.machineRules, nil)
}

// machineRulePage builds the machine-specific part of a sync.
type machineRulePage struct {
	queryer     dynamodb.QueryAPI
	getter      dynamodb.GetItemAPI
	batchGetter dynamodb.BatchGetItemAPI
	batchWriter dynamodb.BatchWriteItemAPI
	updater     dynamodb.UpdateItemIfAPI
	deleter     dynamodb.DeleteItemIfAPI
	timer       clock.TimeProvider
	// groups caches the group registry and group rules for this server instance; nil serves no group rules
	groups *groups.Cache
	// responseBudget is how many bytes of rules a response carries (see encodedRuleBytes); 0 for
	// defaultResponseBudgetBytes
	responseBudget int
}

func (p machineRulePage) budget() int {
	if p.responseBudget > 0 {
		return p.responseBudget
	}
	return defaultResponseBudgetBytes
}

// tailRule is a rule of the machine-specific tail, with the rule sort key it is tracked and paged by
type tailRule struct {
	sortKey string
	rule    rules.SantaRule
}

// The sections of the machine-specific tail, in the order they are sent. No rule sort key is in two of them, so the
// order between them does not change what the Mac ends up with; what matters is that all of them follow the global
// or feed rules. The replacements come last so that a rule that stops applying after an earlier page sent it is
// replaced by a later page of the same sync.
const (
	tailMachineRules = iota + 1
	tailGroupRules
	tailReplacements
)

// tailPosition is how far a sync has got through the tail: the section the next page goes on with, and the sort key
// of the last rule of that section already sent ("" for none). The zero value is the start of the tail.
type tailPosition struct {
	section int
	sortKey string
}

// machineTail is the machine-specific end of a sync: the machine's own rules that apply, the rules of its groups,
// and what replaces the rules the Mac may hold that no longer apply. Each section is in sort key order, which is what
// lets a sync whose tail does not fit one response go on from a key (see fill).
type machineTail struct {
	syncState *syncstate.SyncStateRow
	sections  [tailReplacements][]tailRule
	// removalsFirstSentUpTo is the latest sync in which one of the replacements was first sent, 0 without any
	removalsFirstSentUpTo int
}

// fill adds the rules of the tail after from to the page, and reports where the next page goes on when they do not
// all fit. The tail is built again for every page (see machineRulePage.tail) and may have changed in between; going
// on from a sort key rather than a count means a change before that key is left to the next sync, which sends every
// rule that applies again anyway, and never shifts the rest so that a rule is skipped.
func (t *machineTail) fill(page *rulePage, from tailPosition) (next tailPosition, more bool) {
	for section := max(from.section, tailMachineRules); section <= tailReplacements; section++ {
		sent := ""
		if section == from.section {
			sent = from.sortKey
		}
		for _, entry := range t.sections[section-1] {
			if sent != "" && entry.sortKey <= sent {
				continue
			}
			if !page.add(entry.rule) {
				return tailPosition{section: section, sortKey: sent}, true
			}
			sent = entry.sortKey
		}
	}
	return tailPosition{}, false
}

// tail returns the machine-specific rules to send at the end of the sync, and records which rules the Mac may hold
// from now on. When they do not fit the sync's last global or feed page they go on over further pages, each of which
// calls tail again: the rules are read again, and the tracking, already brought up to date by the first of those
// pages, is only written again if something changed in between.
//
// syncState is the machine's sync state as read at the start of this ruledownload; when nil, it is read again.
// cursorSyncSeq is the sync the request's cursor was issued for (its seq, 0 when it has none). Every global or feed
// cursor this version issues for a numbered sync carries the sync's seq, so one without was issued by an older version
// (a sync in flight across an upgrade is refused once and the next sync works) or forged, and is refused like a
// superseded one.
func (p machineRulePage) tail(machineID string, syncState *syncstate.SyncStateRow, cursorSyncSeq int) (*machineTail, error) {
	if syncState == nil {
		var err error
		syncState, err = syncstate.GetByMachineID(p.getter, machineID)
		if err != nil {
			return nil, fmt.Errorf("failed to read the sync state: %w", err)
		}
		if syncState == nil {
			return nil, errNoSyncState
		}
	}
	if cursorSyncSeq != syncState.SyncSeq {
		// The last page of an older sync, requested after a newer sync of the machine started (Santa gave up on it):
		// its tracking writes and served page would be taken as the newer sync's
		return nil, fmt.Errorf("ruledownload of a superseded sync: %w", syncstate.ErrSyncSuperseded)
	}

	// DynamoDB returns the rows in sort key order, which fill relies on
	rows, err := machinerules.GetMachineRules(p.queryer, machineID)
	if err != nil {
		return nil, err
	}

	now := p.timer.Now()
	tail := &machineTail{syncState: syncState}
	applying := map[string]machinerules.Delivery{}
	for _, row := range *rows {
		if !row.AppliesAt(now) {
			continue
		}
		if row.Policy == rudolphtypes.RulePolicyAllowlistTransitive {
			// Never sent to Santa (see DDBRulesToResponseRules), so the Mac cannot hold it
			continue
		}
		tail.sections[tailMachineRules-1] = append(tail.sections[tailMachineRules-1], tailRule{sortKey: row.SortKey, rule: row.SantaRule})
		applying[row.SortKey] = machinerules.Delivery{}
	}

	// The rules of the Mac's groups join the machine rules: delivery tracking treats them alike, so a group rule that
	// stops applying (the Mac left the group, or the rule was removed) is replaced by the global rule or removed
	groupRules, err := p.groupRules(machineID, applying)
	if err != nil {
		return nil, err
	}
	for _, rule := range groupRules {
		tail.sections[tailGroupRules-1] = append(tail.sections[tailGroupRules-1], tailRule{sortKey: rule.SortKey, rule: rule.SantaRule})
		applying[rule.SortKey] = machinerules.Delivery{}
	}

	refresh := trackingNeedsRefresh(syncState, now)
	if refresh || machinerules.DeliveryDigest(applying) != syncState.MachineRulesTracked {
		// Something was added or stopped applying, a removal is pending, delivery is not tracked yet, the summary may
		// be stale (MachineRulesTrackedDirty), or the tracking items' expiry must be extended
		replacements, firstSentUpTo, err := p.trackDeliveries(machineID, syncState, *rows, applying, refresh, now)
		if err != nil {
			return nil, err
		}
		tail.sections[tailReplacements-1] = replacements
		tail.removalsFirstSentUpTo = firstSentUpTo
	}

	// Never log the rules themselves: they identify the binaries a machine is allowed or blocked from running.
	log.Printf("  Ruledownload machine rules: %d in force (%d from groups), %d no longer applying", len(applying), len(groupRules), len(tail.sections[tailReplacements-1]))

	// Rows marked for deletion are no longer needed once delivery is tracked: whether the Mac still has to be told is
	// decided by the tracked deliveries, not by the row. Without tracking (rows written by older versions of Rudolph,
	// or a sync numbered by an older preflight) the rows are what says the Mac may hold these rules, so they are kept.
	if syncState.SyncSeq > 0 {
		for _, row := range *rows {
			if !row.DeleteOnNextSync {
				continue
			}
			if err := machinerules.DeleteRuleMarkedForDeletion(p.deleter, row.PrimaryKey); err != nil {
				return nil, err
			}
		}
	}
	return tail, nil
}

// recordLastPageServed is called for the sync's last page, the one that ends the tail. For a clean sync, or after
// syncs that received no postflight, it records that the page was served, which the next preflight may count as the
// sync's completion or as a clean sync performed (see syncstate.SyncState.LastPageServedMayCount). It is recorded
// after the tracking of tail, so a sync whose last page was served never has tracking writes still to come.
//
// feedServedUpTo is, for an incremental sync, the stamp of the newest feed row the sync served (the zero time when it
// served none), and nil for any other sync or when it cannot be told.
func (p machineRulePage) recordLastPageServed(machineID string, tail *machineTail, feedServedUpTo *time.Time) {
	syncState := tail.syncState
	if syncState.SyncSeq == 0 || !syncState.LastPageServedMayCount() {
		return
	}
	// Machine rules that apply are sent on every sync whether or not earlier ones completed, so only the feed rows
	// and the removals decide whether this sync served anything new
	nothingNew := feedServedUpTo != nil && syncState.ServedNothingNew(*feedServedUpTo, tail.removalsFirstSentUpTo)
	// A clean sync serves every global rule, including rules written just before it that no earlier sync served,
	// which neither its feed position nor the next sync's would show as new
	newContent := syncState.CleanSync || tail.removalsFirstSentUpTo == syncState.SyncSeq ||
		(feedServedUpTo != nil && syncState.ServesFeedRowsFirst(*feedServedUpTo))
	run := syncState.NextContentRun(newContent)
	err := syncstate.MarkLastPageServed(p.timer, p.updater, machineID, syncState.SyncSeq, nothingNew, run)
	if err != nil && !errors.Is(err, syncstate.ErrSyncSuperseded) {
		// Not fatal: the sync is then only counted as completed if its postflight arrives
		log.Printf("  %v", err)
	}
}

// groupRules returns the rules of the Mac's groups to send, one per rule type and identifier, sorted by rule sort key
// so that pages are stable. Keys covered by a machine rule that applies (in machineKeys) are left out: the machine rule
// wins. Without registered groups nothing is read.
func (p machineRulePage) groupRules(machineID string, machineKeys map[string]machinerules.Delivery) ([]groups.RuleRow, error) {
	if p.groups == nil {
		return nil, nil
	}
	registry, err := p.groups.Registry(p.getter)
	if err != nil {
		return nil, err
	}
	if len(registry.Groups) == 0 {
		return nil, nil
	}
	// Strongly consistent: preflight may have changed the Mac's reported groups moments ago
	membership, err := groups.GetMachineGroups(p.getter, machineID, true)
	if err != nil {
		return nil, err
	}
	var all []groups.GroupRules
	for _, group := range groups.Effective(registry, membership) {
		rows, err := p.groups.Rules(p.queryer, registry, group)
		if err != nil {
			return nil, err
		}
		all = append(all, groups.GroupRules{Group: group, Rules: rows})
	}
	winners := groups.Resolve(all, func(sortKey string) bool {
		_, ok := machineKeys[sortKey]
		return ok
	})
	out := make([]groups.RuleRow, 0, len(winners))
	for _, rule := range winners {
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortKey < out[j].SortKey })
	return out, nil
}

// trackingNeedsRefresh reports whether the machine's tracking items must be rewritten to extend their expiry (see
// machinerules.DeliveryRefreshInterval).
func trackingNeedsRefresh(syncState *syncstate.SyncStateRow, now time.Time) bool {
	if syncState.MachineRulesTracked == "" || syncState.SyncSeq == 0 {
		// Not tracked yet (every item is written when tracking starts), or a sync that cannot record anything
		return false
	}
	refreshedAt, err := clock.ParseRFC3339(syncState.MachineRulesTrackingRefreshedAt)
	return err != nil || now.Sub(refreshedAt) >= machinerules.DeliveryRefreshInterval
}

// trackDeliveries brings the tracked deliveries up to date with the rules that apply now, and returns what to send for
// the rules the Mac may hold that no longer apply, and the latest sync in which one of those was first sent (0 when
// there are none).
//
// The tracking items and the summary on the sync state cannot be written atomically. So before changing any item,
// the summary is set to MachineRulesTrackedDirty, which never matches the rules that apply; if this ruledownload dies
// before it writes the new summary, the next sync reads the items again instead of trusting a stale summary. Both
// summary writes are conditional on the sync state still being this sync's and holding the value this sync last saw
// or wrote, so a ruledownload overtaken by a newer sync of the machine fails rather than overwriting its summary.
func (p machineRulePage) trackDeliveries(
	machineID string,
	syncState *syncstate.SyncStateRow,
	rows []machinerules.MachineRuleRow,
	applying map[string]machinerules.Delivery,
	refresh bool,
	now time.Time,
) ([]tailRule, int, error) {
	tracked, err := machinerules.GetDeliveries(p.queryer, machineID)
	if err != nil {
		return nil, 0, err
	}
	untracked := map[string]bool{}
	if syncState.MachineRulesTracked == "" {
		// Not tracked yet. Earlier versions of Rudolph sent every row of the machine on every sync, whether it still
		// applied or not, so the Mac may hold any of them.
		for _, row := range rows {
			if _, ok := tracked[row.SortKey]; !ok {
				tracked[row.SortKey] = machinerules.Delivery{}
				untracked[row.SortKey] = true
			}
		}
	}
	// A sync numbered by an older preflight cannot record removals; it sends them and leaves tracking to later syncs
	record := syncState.SyncSeq > 0

	set := map[string]machinerules.Delivery{}
	var forget, stopped []string
	removalsFirstSentUpTo := 0
	for sortKey := range applying {
		if delivery, ok := tracked[sortKey]; !ok || untracked[sortKey] || delivery.RemovalFirstSentInSync != 0 {
			// Sent for the first time, or applying again after its removal was sent
			set[sortKey] = machinerules.Delivery{}
		}
	}
	for sortKey, delivery := range tracked {
		if _, ok := applying[sortKey]; ok {
			continue
		}
		firstSent := delivery.RemovalFirstSentInSync
		switch {
		case firstSent == 0:
			firstSent = syncState.SyncSeq
			set[sortKey] = machinerules.Delivery{RemovalFirstSentInSync: firstSent}
			stopped = append(stopped, sortKey)
		case syncstate.DeliveryConfirmed(firstSent, syncState.CompletedSyncs):
			forget = append(forget, sortKey)
			continue
		default:
			stopped = append(stopped, sortKey)
		}
		removalsFirstSentUpTo = max(removalsFirstSentUpTo, firstSent)
	}
	sort.Strings(stopped)

	replacements, err := p.replacementsFor(stopped)
	if err != nil {
		return nil, 0, err
	}
	if !record {
		return replacements, removalsFirstSentUpTo, nil
	}

	refreshedAt := ""
	if refresh || syncState.MachineRulesTracked == "" {
		// Extend the expiry of every item kept (when tracking starts, every item kept is written anyway)
		forgotten := map[string]bool{}
		for _, sortKey := range forget {
			forgotten[sortKey] = true
		}
		for sortKey, delivery := range tracked {
			if _, ok := set[sortKey]; !ok && !forgotten[sortKey] {
				set[sortKey] = delivery
			}
		}
		refreshedAt = clock.RFC3339(now)
	}

	summary := syncState.MachineRulesTracked
	if len(set) > 0 || len(forget) > 0 {
		if summary != "" && summary != syncstate.MachineRulesTrackedDirty {
			// "" (not tracked yet) already makes every sync read the items and the rows, so it is kept until the end
			if err := p.setMachineRulesTracked(machineID, syncState.SyncSeq, summary, syncstate.MachineRulesTrackedDirty, ""); err != nil {
				return nil, 0, err
			}
			summary = syncstate.MachineRulesTrackedDirty
		}
		if err := machinerules.WriteDeliveries(p.batchWriter, machineID, set, forget, now); err != nil {
			return nil, 0, p.invalidateAfterItemWrites(machineID, err)
		}
	}
	for sortKey, delivery := range set {
		tracked[sortKey] = delivery
	}
	for _, sortKey := range forget {
		delete(tracked, sortKey)
	}
	if digest := machinerules.DeliveryDigest(tracked); digest != summary || refreshedAt != "" {
		if err := p.setMachineRulesTracked(machineID, syncState.SyncSeq, summary, digest, refreshedAt); err != nil {
			if len(set) > 0 || len(forget) > 0 {
				return nil, 0, p.invalidateAfterItemWrites(machineID, err)
			}
			return nil, 0, err
		}
	}
	return replacements, removalsFirstSentUpTo, nil
}

// setMachineRulesTracked is syncstate.SetMachineRulesTracked, except that a failed condition is only
// syncstate.ErrSyncSuperseded (answered 409) when the sync state now belongs to a sync other than syncSeq, and
// errNoSyncState (answered 400) when it is gone (expired or forgotten). The condition also fails when the summary
// changed under the same sync: a concurrent invocation of the same last page (Santa's retry of a request that timed
// out) wrote it first, or an abandoned older sync invalidated it. That sync can still succeed, and Santa retries a 500
// but gives up on a 409.
func (p machineRulePage) setMachineRulesTracked(machineID string, syncSeq int, expected, summary, refreshedAt string) error {
	err := syncstate.SetMachineRulesTracked(p.updater, machineID, syncSeq, expected, summary, refreshedAt)
	if !errors.Is(err, syncstate.ErrSyncSuperseded) {
		return err
	}
	current, rerr := syncstate.GetByMachineID(p.getter, machineID)
	if rerr != nil {
		return fmt.Errorf("failed to read the sync state after a failed machine rules tracked update: %w", rerr)
	}
	if current == nil {
		// Expired or forgotten mid-request: like any ruledownload without a sync state, the Mac has to preflight again
		return errNoSyncState
	}
	if current.SyncSeq == syncSeq {
		return errors.New("the machine rules tracked changed during the ruledownload of the machine's current sync")
	}
	return err
}

// invalidateAfterItemWrites handles a failure after this ruledownload sent tracking item writes, and returns err.
//
// The item writes are not conditional (BatchWriteItem cannot be), so they may have landed after a newer sync of the
// machine took over: e.g. Santa gave up on this slow sync and started another, which read the items, recorded a rule
// it sent again, and wrote a summary of what it saw, and then this ruledownload's write deleted that item. The newer
// summary would then hide a rule the Mac holds, and its removal would never be sent. So the summary is replaced, on
// whichever sync's row, by a value that makes the next sync read the items again (which brings them back in line with
// the rules that apply). When this ruledownload is still the machine's current sync, the summary is already dirty
// and this changes nothing that matters.
func (p machineRulePage) invalidateAfterItemWrites(machineID string, err error) error {
	if ierr := syncstate.InvalidateMachineRulesTracked(p.updater, machineID); ierr != nil {
		log.Printf("  Failed to invalidate the machine rules tracked after an interrupted update: %v", ierr)
	}
	return err
}

// replacementsFor returns what to send for machine rules the Mac may hold but that no longer apply, in the order of
// their rule sort keys as given: the global rule for the same identifier and type if there is one Santa can apply, and
// otherwise a REMOVE.
func (p machineRulePage) replacementsFor(sortKeys []string) ([]tailRule, error) {
	if len(sortKeys) == 0 {
		return nil, nil
	}
	globalRules, err := globalrules.GetGlobalRulesBySortKeysConsistently(p.batchGetter, sortKeys)
	if err != nil {
		return nil, fmt.Errorf("failed to read the global rules machine rules fall back to: %w", err)
	}

	replacements := make([]tailRule, 0, len(sortKeys))
	for _, sortKey := range sortKeys {
		ruleType, identifier, ok := rules.RuleTypeIdentifierFromSortKey(sortKey)
		if !ok {
			// Only Rudolph writes these keys, from rule sort keys; there is nothing that could be removed on the Mac
			log.Printf("  Ignoring a delivered machine rule key that is not a rule sort key")
			continue
		}
		if globalRule := globalRules[sortKey]; globalRule != nil && globalRule.RuleType == ruleType && rules.ValidatePolicy(globalRule.SantaRule) == nil {
			replacements = append(replacements, tailRule{sortKey: sortKey, rule: globalRule.SantaRule})
			continue
		}
		// No global rule, or one Santa cannot apply (e.g. the legacy ALLOWLIST_TRANSITIVE policy): the Mac drops the rule
		replacements = append(replacements, tailRule{sortKey: sortKey, rule: rules.SantaRule{
			RuleType:   ruleType,
			Policy:     rudolphtypes.RulePolicyRemove,
			Identifier: identifier,
		}})
	}
	return replacements, nil
}
