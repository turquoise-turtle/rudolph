package ruledownload

import (
	"fmt"
	"log"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

// feedPageSize is the most feed rules sent on one incremental ruledownload page.
//
// Feed pages are not sized by the configured batch size (which Santa uses for event uploads): every sync re-reads
// the feed rows of the overlap before its cursor (see syncstate.FeedSyncCursorOverlap), and reading them in pages of
// 50 would cost a request per 50 rows on every sync. DynamoDB returns at most 1 MB per query, which bounds a page too,
// as does the response budget (see rulePage).
const feedPageSize = 5000

type feedRuleDownloader interface {
	handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error)
}

type concreteFeedRuleDownloader struct {
	queryer      dynamodb.QueryAPI
	machineRules machineRulePage
}

func (d concreteFeedRuleDownloader) handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error) {
	if cursor.onTail() {
		// Every feed row has been sent; this page goes on with the machine-specific rules
		return lastPageOrNext(machineID, cursor, nil, nil, d.machineRules, tailFeedServedUpTo(cursor))
	}
	ddbCursor := cursor.GetLastEvaluatedKey()

	feedRules, lastEvaluatedKey, err := feedrules.GetPaginatedFeedRules(d.queryer, feedPageSize, ddbCursor)
	if err != nil {
		return response.InternalServerError(fmt.Errorf("reading a page of feed rules: %w", err), struct{}{})
	}

	// A query returns up to 1 MB of rows, which is more than a response can carry when the rules' text needs much
	// escaping. The rows that do not fit are left to the next page, which starts after the last one that did.
	page := newRulePage(d.machineRules.budget(), cursor.SilentBlocklistOnly)
	for i, rule := range feedRules {
		if !page.add(rule.SantaRule) {
			feedRules = feedRules[:i]
			lastEvaluatedKey = &feedRules[i-1].PrimaryKey
			break
		}
	}

	// Never log the rules or the pagination key itself: they identify the binaries a fleet blocks or allows.
	log.Printf("  Ruledownload feed page: %d feed rule(s), more pages: %t", len(feedRules), lastEvaluatedKey != nil)

	rules := make([]rules.SantaRule, len(feedRules))
	for i, rule := range feedRules {
		rules[i] = rule.SantaRule
	}

	servedUpTo := feedServedUpTo(cursor, feedRules)
	// Should the machine-specific rules go on over further pages, their cursor carries what the last of them needs to
	// work servedUpTo out again (see tailFeedServedUpTo): the key of the newest feed row served, or none
	switch {
	case len(feedRules) > 0:
		cursor.ruledownloadCursorDDBLastEvaluatedKey = ruledownloadCursorDDBLastEvaluatedKey(feedRules[len(feedRules)-1].PrimaryKey)
	case cursor.PageNumber <= 1:
		cursor.ruledownloadCursorDDBLastEvaluatedKey = ruledownloadCursorDDBLastEvaluatedKey{}
	}
	return lastPageOrNext(machineID, cursor, rules, lastEvaluatedKey, d.machineRules, servedUpTo)
}

// tailFeedServedUpTo is feedServedUpTo for a page of machine-specific rules after the feed's last page, from the key
// that page left in the cursor: the newest feed row the sync served, or no key when it served none.
func tailFeedServedUpTo(cursor ruledownloadCursor) *time.Time {
	if cursor.SortKey == "" {
		return &time.Time{}
	}
	stamp, err := feedrules.TimeOfSortKey(cursor.SortKey)
	if err != nil {
		return nil
	}
	return &stamp
}

// feedServedUpTo returns the stamp of the newest feed row a sync served once it serves this page, or nil when it
// cannot tell. The feed is read in sort key order, so that is the last row of this page; on a later page without rows
// it is the row the previous page ended on. A sync whose only page has no rows served none: the zero time.
func feedServedUpTo(cursor ruledownloadCursor, page []*feedrules.FeedRuleRow) *time.Time {
	if len(page) == 0 && cursor.PageNumber <= 1 {
		return &time.Time{}
	}
	sortKey := cursor.SortKey
	if len(page) > 0 {
		sortKey = page[len(page)-1].SortKey
	}
	stamp, err := feedrules.TimeOfSortKey(sortKey)
	if err != nil {
		return nil
	}
	return &stamp
}
