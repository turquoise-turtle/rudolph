package ruledownload

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

// This function handles the clean sync
// We paginate through all global rules, handing off the cursor to the sensor in every response.
// When we hit the final page of global rules, we mark down
type globalRuleDownloader interface {
	handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error)
}

type concreteGlobalRuleDownloader struct {
	queryer dynamodb.QueryAPI
}

func (d concreteGlobalRuleDownloader) handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error) {
	ddbCursor := cursor.GetLastEvaluatedKey()
	globalRules, lastEvaluatedKey, err := globalrules.GetPaginatedGlobalRules(d.queryer, cursor.BatchSize, ddbCursor)
	if err != nil {
		log.Printf("  GetPaginatedGlobalRules Error %s", err.Error())
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	// Never log the rules or the pagination key itself: they identify the binaries a fleet blocks or allows.
	log.Printf("  Ruledownload clean page: %d global rule(s), more pages: %t", len(globalRules), lastEvaluatedKey != nil)

	nextCursor := cursor.CloneForNextPage()
	if lastEvaluatedKey == nil {
		nextCursor.SetStrategy(ruledownloadStrategyMachine)
	} else {
		nextCursor.SetDynamodbLastEvaluatedKey(lastEvaluatedKey)
	}

	rules := make([]rules.SantaRule, len(globalRules))
	for i, rule := range globalRules {
		rules[i] = rule.SantaRule
	}

	// Marshal the cursor to a string
	jsonCursor, err := json.Marshal(nextCursor)
	if err != nil {
		log.Printf("  json.Marshal Error %s", err.Error())
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	return response.APIResponse(
		http.StatusOK,
		RuledownloadResponse{
			Rules:  DDBRulesToResponseRules(rules),
			Cursor: string(jsonCursor),
		},
	)
}
