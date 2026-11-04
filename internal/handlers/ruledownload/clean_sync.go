package ruledownload

import (
	"encoding/json"
	"errors"
	"fmt"
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
	queryer      dynamodb.QueryAPI
	machineRules machineRulePage
}

func (d concreteGlobalRuleDownloader) handle(machineID string, cursor ruledownloadCursor) (*events.APIGatewayProxyResponse, error) {
	ddbCursor := cursor.GetLastEvaluatedKey()
	globalRules, lastEvaluatedKey, err := globalrules.GetPaginatedGlobalRules(d.queryer, cursor.BatchSize, ddbCursor)
	if err != nil {
		return response.InternalServerError(fmt.Errorf("reading a page of global rules: %w", err), struct{}{})
	}

	// Never log the rules or the pagination key itself: they identify the binaries a fleet blocks or allows.
	log.Printf("  Ruledownload clean page: %d global rule(s), more pages: %t", len(globalRules), lastEvaluatedKey != nil)

	rules := make([]rules.SantaRule, len(globalRules))
	for i, rule := range globalRules {
		rules[i] = rule.SantaRule
	}

	return lastPageOrNext(machineID, cursor, rules, lastEvaluatedKey, d.machineRules)
}

// lastPageOrNext returns a page of global or feed rules. On the last one, the machine-specific rules are appended
// and no cursor is returned, which ends the sync's ruledownload; otherwise the cursor for the next page is returned.
func lastPageOrNext(
	machineID string,
	cursor ruledownloadCursor,
	pageRules []rules.SantaRule,
	lastEvaluatedKey *dynamodb.PrimaryKey,
	machineRules machineRulePage,
) (*events.APIGatewayProxyResponse, error) {
	if lastEvaluatedKey == nil {
		machineSpecific, err := machineRules.rules(machineID, cursor.syncState)
		if err != nil {
			return response.InternalServerError(fmt.Errorf("building the machine rules page: %w", err), response.ErrInternalServerErrorResponse)
		}
		return response.APIResponse(
			http.StatusOK,
			RuledownloadResponse{
				Rules: DDBRulesToResponseRules(append(pageRules, machineSpecific...)),
				// Omit the cursor to signal to the sensor that there are no more pages to paginate through
			},
		)
	}

	// Here we inherit the preexisting cursor strategy
	nextCursor := cursor.CloneForNextPage()
	nextCursor.SetDynamodbLastEvaluatedKey(lastEvaluatedKey)

	// Marshal the cursor to a string
	jsonCursor, err := json.Marshal(nextCursor)
	if err != nil {
		// The error is not logged: json.UnsupportedValueError embeds the value it could not marshal.
		return response.InternalServerError(errors.New("failed to marshal the ruledownload cursor"), struct{}{})
	}

	return response.APIResponse(
		http.StatusOK,
		RuledownloadResponse{
			Rules:  DDBRulesToResponseRules(pageRules),
			Cursor: string(jsonCursor),
		},
	)
}
