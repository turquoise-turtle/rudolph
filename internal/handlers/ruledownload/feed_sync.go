package ruledownload

import (
	"log"
	"net/http"

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
// 50 would cost a request per 50 rows on every sync. DynamoDB returns at most 1 MB per query, which bounds a page too.
const feedPageSize = 5000

type feedRuleDownloader interface {
	handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error)
}

type concreteFeedRuleDownloader struct {
	queryer      dynamodb.QueryAPI
	machineRules machineRulePage
}

func (d concreteFeedRuleDownloader) handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error) {
	ddbCursor := cursor.GetLastEvaluatedKey()

	feedRules, lastEvaluatedKey, err := feedrules.GetPaginatedFeedRules(d.queryer, feedPageSize, ddbCursor)
	if err != nil {
		log.Printf("  GetPaginatedFeedRules Error %s", err.Error())
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	// Never log the rules or the pagination key itself: they identify the binaries a fleet blocks or allows.
	log.Printf("  Ruledownload feed page: %d feed rule(s), more pages: %t", len(feedRules), lastEvaluatedKey != nil)

	rules := make([]rules.SantaRule, len(feedRules))
	for i, rule := range feedRules {
		rules[i] = rule.SantaRule
	}

	return lastPageOrNext(machineID, cursor, rules, lastEvaluatedKey, d.machineRules)
}
