package ruledownload

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	"github.com/airbnb/rudolph/pkg/response"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

// The machine-specific rules of a sync are sent at the end of its last page, after the global rules (clean sync) or
// the rules feed (incremental sync), so that they take precedence over them. They are re-sent on every sync.
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

// concreteMachineRuleDownloader serves a page holding only the machine-specific rules. Current syncs get them on
// their last global or feed page; this page is only requested with cursors issued by earlier versions of Rudolph.
type concreteMachineRuleDownloader struct {
	machineRules machineRulePage
}

func (d concreteMachineRuleDownloader) handle(machineID string, ruledownloadRequest *RuledownloadRequest) (*events.APIGatewayProxyResponse, error) {
	log.Printf("  Ruledownload machine page")

	machineRules, err := d.machineRules.rules(machineID, nil)
	if err != nil {
		log.Printf("  Failed to build the machine rules page: %v", err)
		return response.APIResponse(http.StatusInternalServerError, response.ErrInternalServerErrorResponse)
	}

	// The lack of a cursor in this response signals to the sensor that there is no more stuff to paginate over.
	return response.APIResponse(
		http.StatusOK,
		RuledownloadResponse{
			Rules: DDBRulesToResponseRules(machineRules),
			// Omit the cursor to signal to the sensor that there are no more pages to paginate through
		},
	)
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
}

// rules returns the machine-specific rules to send at the end of the sync's last page, and records which rules the
// Mac may hold from now on.
//
// syncState is the machine's sync state as read at the start of this ruledownload; when nil, it is read again.
func (p machineRulePage) rules(machineID string, syncState *syncstate.SyncStateRow) ([]rules.SantaRule, error) {
	if syncState == nil {
		var err error
		syncState, err = syncstate.GetByMachineID(p.getter, machineID)
		if err != nil {
			return nil, fmt.Errorf("failed to read the sync state: %w", err)
		}
		if syncState == nil {
			return nil, errors.New("no sync state: ruledownload without a preflight")
		}
	}

	rows, err := machinerules.GetMachineRules(p.queryer, machineID)
	if err != nil {
		return nil, err
	}

	now := p.timer.Now()
	var page []rules.SantaRule
	applying := map[string]machinerules.Delivery{}
	for _, row := range *rows {
		if !row.AppliesAt(now) {
			continue
		}
		if row.Policy == rudolphtypes.RulePolicyAllowlistTransitive {
			// Never sent to Santa (see DDBRulesToResponseRules), so the Mac cannot hold it
			continue
		}
		page = append(page, row.SantaRule)
		applying[row.SortKey] = machinerules.Delivery{}
	}

	stopped := 0
	refresh := trackingNeedsRefresh(syncState, now)
	if refresh || machinerules.DeliveryDigest(applying) != syncState.MachineRulesTracked {
		// Something was added or stopped applying, a removal is pending, delivery is not tracked yet, the summary may
		// be stale (MachineRulesTrackedDirty), or the tracking items' expiry must be extended
		replacements, err := p.trackDeliveries(machineID, syncState, *rows, applying, refresh, now)
		if err != nil {
			return nil, err
		}
		page = append(page, replacements...)
		stopped = len(replacements)
	}

	// Never log the rules themselves: they identify the binaries a machine is allowed or blocked from running.
	log.Printf("  Ruledownload machine rules: %d in force, %d no longer applying", len(applying), stopped)

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
	return page, nil
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
// the rules the Mac may hold that no longer apply.
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
) ([]rules.SantaRule, error) {
	tracked, err := machinerules.GetDeliveries(p.queryer, machineID)
	if err != nil {
		return nil, err
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
		switch {
		case delivery.RemovalFirstSentInSync == 0:
			set[sortKey] = machinerules.Delivery{RemovalFirstSentInSync: syncState.SyncSeq}
			stopped = append(stopped, sortKey)
		case syncstate.DeliveryConfirmed(delivery.RemovalFirstSentInSync, syncState.CompletedSyncs):
			forget = append(forget, sortKey)
		default:
			stopped = append(stopped, sortKey)
		}
	}
	sort.Strings(stopped)

	replacements, err := p.replacementsFor(stopped)
	if err != nil {
		return nil, err
	}
	if !record {
		return replacements, nil
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
			if err := syncstate.SetMachineRulesTracked(p.updater, machineID, syncState.SyncSeq, summary, syncstate.MachineRulesTrackedDirty, ""); err != nil {
				return nil, err
			}
			summary = syncstate.MachineRulesTrackedDirty
		}
		if err := machinerules.WriteDeliveries(p.batchWriter, machineID, set, forget, now); err != nil {
			return nil, err
		}
	}
	for sortKey, delivery := range set {
		tracked[sortKey] = delivery
	}
	for _, sortKey := range forget {
		delete(tracked, sortKey)
	}
	if digest := machinerules.DeliveryDigest(tracked); digest != summary || refreshedAt != "" {
		if err := syncstate.SetMachineRulesTracked(p.updater, machineID, syncState.SyncSeq, summary, digest, refreshedAt); err != nil {
			return nil, err
		}
	}
	return replacements, nil
}

// replacementsFor returns what to send for machine rules the Mac may hold but that no longer apply, by rule sort key:
// the global rule for the same identifier and type if there is one Santa can apply, and otherwise a REMOVE.
func (p machineRulePage) replacementsFor(sortKeys []string) ([]rules.SantaRule, error) {
	if len(sortKeys) == 0 {
		return nil, nil
	}
	globalRules, err := globalrules.GetGlobalRulesBySortKeysConsistently(p.batchGetter, sortKeys)
	if err != nil {
		return nil, fmt.Errorf("failed to read the global rules machine rules fall back to: %w", err)
	}

	replacements := make([]rules.SantaRule, 0, len(sortKeys))
	for _, sortKey := range sortKeys {
		ruleType, identifier, ok := rules.RuleTypeIdentifierFromSortKey(sortKey)
		if !ok {
			// Only Rudolph writes these keys, from rule sort keys; there is nothing that could be removed on the Mac
			log.Printf("  Ignoring a delivered machine rule key that is not a rule sort key")
			continue
		}
		if globalRule := globalRules[sortKey]; globalRule != nil && globalRule.RuleType == ruleType && rules.ValidatePolicy(globalRule.SantaRule) == nil {
			replacements = append(replacements, globalRule.SantaRule)
			continue
		}
		// No global rule, or one Santa cannot apply (e.g. the legacy ALLOWLIST_TRANSITIVE policy): the Mac drops the rule
		replacements = append(replacements, rules.SantaRule{
			RuleType:   ruleType,
			Policy:     rudolphtypes.RulePolicyRemove,
			Identifier: identifier,
		})
	}
	return replacements, nil
}
