package ruledownload

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"

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
// table's TTL) must also leave the Mac. The sync state records which machine rules the Mac holds (see
// syncstate.SyncState); each of those that no longer applies is sent as the global rule for the same identifier and
// type if there is one, so the Mac falls back to the fleet rule, and as a REMOVE otherwise.
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
	queryer dynamodb.QueryAPI
	getter  dynamodb.GetItemAPI
	updater dynamodb.UpdateItemAPI
	deleter dynamodb.DeleteItemIfAPI
	timer   clock.TimeProvider
}

// rules returns the machine-specific rules to send at the end of the sync's last page, and records on the sync
// state which of them the Mac will hold if the sync completes.
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
	applying := map[string]bool{}
	var served []string
	for _, row := range *rows {
		if !row.AppliesAt(now) {
			continue
		}
		if row.Policy == rudolphtypes.RulePolicyAllowlistTransitive {
			// Never sent to Santa (see DDBRulesToResponseRules), so the Mac cannot hold it
			continue
		}
		page = append(page, row.SantaRule)
		applying[row.SortKey] = true
		served = append(served, row.SortKey)
	}

	// The rules the Mac may hold from earlier syncs. Sync states written before delivery was tracked do not say; those
	// versions of Rudolph sent every row of the machine on every sync, whether it still applied or not.
	var mayHold []string
	if syncState.MachineRulesDelivered.Known() {
		mayHold = syncState.MachineRulesDelivered.Keys()
	} else {
		for _, row := range *rows {
			mayHold = append(mayHold, row.SortKey)
		}
		sort.Strings(mayHold)
	}

	stopped := 0
	for _, sortKey := range mayHold {
		if applying[sortKey] {
			continue
		}
		replacement, ok, err := p.replacementFor(sortKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		page = append(page, replacement)
		stopped++
	}

	// Never log the rules themselves: they identify the binaries a machine is allowed or blocked from running.
	log.Printf("  Ruledownload machine rules: %d in force, %d no longer applying", len(served), stopped)

	servedKeys := syncstate.NewMachineRuleKeys(served...)
	if !servedKeys.Equal(syncState.MachineRulesServed) {
		if err := syncstate.UpdateMachineRulesServed(p.updater, machineID, servedKeys); err != nil {
			return nil, err
		}
	}

	// Rows marked for deletion are no longer needed once delivery is tracked: whether the Mac still has to be told
	// is decided by the delivered set above, not by the row. Without tracking (rows written by older versions of
	// Rudolph), the rows are what says the Mac may hold these rules, so they are kept until tracking starts.
	if syncState.MachineRulesDelivered.Known() {
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

// replacementFor returns what to send for a machine rule the Mac may hold but that no longer applies: the global rule
// for the same identifier and type if there is one Santa can apply, and otherwise a REMOVE.
func (p machineRulePage) replacementFor(sortKey string) (rule rules.SantaRule, ok bool, err error) {
	ruleType, identifier, ok := rules.RuleTypeIdentifierFromSortKey(sortKey)
	if !ok {
		// Only Rudolph writes these keys, from rule sort keys; there is nothing that could be removed on the Mac
		log.Printf("  Ignoring a delivered machine rule key that is not a rule sort key")
		return rules.SantaRule{}, false, nil
	}

	globalRule, err := globalrules.GetGlobalRuleBySortKeyConsistently(p.getter, sortKey)
	if err != nil {
		return rules.SantaRule{}, false, fmt.Errorf("failed to read the global rule a machine rule falls back to: %w", err)
	}
	if globalRule != nil && globalRule.RuleType == ruleType && rules.ValidatePolicy(globalRule.SantaRule) == nil {
		return globalRule.SantaRule, true, nil
	}

	// No global rule, or one Santa cannot apply (e.g. the legacy ALLOWLIST_TRANSITIVE policy): the Mac drops the rule
	return rules.SantaRule{
		RuleType:   ruleType,
		Policy:     rudolphtypes.RulePolicyRemove,
		Identifier: identifier,
	}, true, nil
}
