package ruledownload

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/response"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

// This function handles the clean sync
// We paginate through all global rules, handing off the cursor to the sensor in every response.
// When we hit the final page of global rules, we mark down
type globalRuleDownloader interface {
	handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error)
}

type concreteGlobalRuleDownloader struct {
	queryer      dynamodb.QueryAPI
	machineRules machineRulePage
}

func (d concreteGlobalRuleDownloader) handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error) {
	if cursor.onTail() {
		// Every global rule has been sent; this page goes on with the machine-specific rules
		return lastPageOrNext(machineID, cursor, nil, nil, d.machineRules, nil)
	}
	// Pages are sized by what the response can carry, not by the cursor's batch size: that is Santa's event upload
	// batch size (50 by default), which made a clean sync of 80,000 rules 1,600 requests, each rounding its read
	// capacity up. DynamoDB returns at most 1 MB per query, so a page is read with as many queries as it takes.
	page := newRulePage(d.machineRules.budget(), cursor.SilentBlocklistOnly)
	var pageRules []rules.SantaRule
	// nextKey is the key the next page starts after: the last rule on this page, or nil after the last global rule
	nextKey := cursor.GetLastEvaluatedKey()
	started := time.Now()
	queries := 0
read:
	for limit := 0; limit >= 0; limit = page.queryLimit() {
		globalRules, lastEvaluatedKey, err := globalrules.QueryGlobalRules(d.queryer, limit, nextKey)
		if err != nil {
			return response.InternalServerError(fmt.Errorf("reading a page of global rules: %w", err), struct{}{})
		}
		queries++
		for _, rule := range globalRules {
			if !page.add(rule.SantaRule) {
				// The rest of what this query read is left to the next page, which starts after the last rule that fitted
				break read
			}
			pageRules = append(pageRules, rule.SantaRule)
			nextKey = &rule.PrimaryKey
		}
		nextKey = lastEvaluatedKey
		if lastEvaluatedKey == nil || time.Since(started) > pageReadTime {
			break
		}
	}

	// Never log the rules or the pagination key itself: they identify the binaries a fleet blocks or allows.
	log.Printf("  Ruledownload clean page: %d global rule(s) in %d bytes from %d queries, more pages: %t", len(pageRules), page.bytes, queries, nextKey != nil)

	// A cursor on the machine-specific rules of a clean sync carries no key (see ruledownloadCursor.TailSection)
	cursor.ruledownloadCursorDDBLastEvaluatedKey = ruledownloadCursorDDBLastEvaluatedKey{}
	return lastPageOrNext(machineID, cursor, pageRules, nextKey, d.machineRules, nil)
}

// cleanSyncPlaceholderIdentifier is the identifier of the rule sent by a clean sync that has no rule to send. It is
// a well-formed SHA-256 that no binary has in practice; removing it from a Mac changes nothing.
const cleanSyncPlaceholderIdentifier = "0000000000000000000000000000000000000000000000000000000000000000"

// cleanSyncPlaceholderRule is sent, alone, on the last page of a clean sync that would otherwise send no rule.
//
// Santa clears its rules at a clean sync only when the download produced at least one rule it could parse
// (SNTSyncRuleDownload.mm returns early without any cleanup otherwise). Without a rule, a clean sync of a Mac that no
// rule applies to any more (e.g. its machine-specific rules expired while it was offline, together with its sync
// state and delivery tracking) would leave every rule the Mac holds in place, and nothing on the server would remember
// them. A REMOVE for an identifier no binary has is a rule Santa parses, so the Mac clears its rules as asked.
func cleanSyncPlaceholderRule() RuledownloadRule {
	return RuledownloadRule{
		RuleType:   rudolphtypes.RuleTypeBinary,
		Policy:     rudolphtypes.RulePolicyRemove,
		Identifier: cleanSyncPlaceholderIdentifier,
	}
}

// lastPageOrNext returns a page of a sync: pageRules, the global or feed rules the caller read for it, and, once
// those are exhausted (no lastEvaluatedKey), the machine-specific rules after them. The response carries a cursor
// while there is more: the next global or feed page, or the rest of the machine-specific rules when they do not fit
// this response. Without a cursor it is the sync's last page.
// feedServedUpTo is only used on the last page (see machineRulePage.recordLastPageServed).
func lastPageOrNext(
	machineID string,
	cursor ruledownloadCursor,
	pageRules []rules.SantaRule,
	lastEvaluatedKey *dynamodb.PrimaryKey,
	machineRules machineRulePage,
	feedServedUpTo *time.Time,
) (*events.APIGatewayProxyResponse, error) {
	// The caller kept pageRules within the budget; what they leave of it is what the machine-specific rules get
	page := newRulePage(machineRules.budget(), cursor.SilentBlocklistOnly)
	for _, rule := range pageRules {
		page.put(rule)
	}

	// Here we inherit the preexisting cursor strategy
	nextCursor := cursor.CloneForNextPage()
	if lastEvaluatedKey != nil {
		nextCursor.SetDynamodbLastEvaluatedKey(lastEvaluatedKey)
	} else {
		tail, err := machineRules.tail(machineID, cursor.syncState, cursor.SyncSeq)
		if err != nil {
			return syncErrorResponse("building the machine rules page", err)
		}
		position, more := tail.fill(page, cursor.tailPosition())
		if !more {
			if cursor.Strategy == ruledownloadStrategyClean && len(page.rules) == 0 {
				page.rules = append(page.rules, cleanSyncPlaceholderRule())
			}
			machineRules.recordLastPageServed(machineID, tail, feedServedUpTo)
			return response.APIResponse(
				http.StatusOK,
				RuledownloadResponse{
					Rules: page.rules,
					// Omit the cursor to signal to the sensor that there are no more pages to paginate through
				},
			)
		}
		log.Printf("  Ruledownload machine rules: more pages")
		nextCursor.TailSection, nextCursor.TailSortKey = position.section, position.sortKey
		// Not a key to read on from any more: see ruledownloadCursor.TailSection
		nextCursor.ruledownloadCursorDDBLastEvaluatedKey = cursor.ruledownloadCursorDDBLastEvaluatedKey
	}

	// Marshal the cursor to a string
	jsonCursor, err := json.Marshal(nextCursor)
	if err != nil {
		// The error is not logged: json.UnsupportedValueError embeds the value it could not marshal.
		return response.InternalServerError(errors.New("failed to marshal the ruledownload cursor"), struct{}{})
	}

	return response.APIResponse(
		http.StatusOK,
		RuledownloadResponse{
			Rules:  page.rules,
			Cursor: string(jsonCursor),
		},
	)
}
