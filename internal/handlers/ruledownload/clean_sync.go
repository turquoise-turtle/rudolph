package ruledownload

import (
	"encoding/json"
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
	ddbCursor := cursor.GetLastEvaluatedKey()
	globalRules, lastEvaluatedKey, err := globalrules.GetPaginatedGlobalRules(d.queryer, cursor.BatchSize, ddbCursor)
	if err != nil {
		log.Printf("  GetPaginatedGlobalRules Error %s", err.Error())
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	// Never log the rules or the pagination key itself: they identify the binaries a fleet blocks or allows.
	log.Printf("  Ruledownload clean page: %d global rule(s), more pages: %t", len(globalRules), lastEvaluatedKey != nil)

	rules := make([]rules.SantaRule, len(globalRules))
	for i, rule := range globalRules {
		rules[i] = rule.SantaRule
	}

	return lastPageOrNext(machineID, cursor, rules, lastEvaluatedKey, d.machineRules, nil)
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

// lastPageOrNext returns a page of global or feed rules. On the last one, the machine-specific rules are appended
// and no cursor is returned, which ends the sync's ruledownload; otherwise the cursor for the next page is returned.
// feedServedUpTo is only used on the last page (see machineRulePage.rules).
func lastPageOrNext(
	machineID string,
	cursor ruledownloadCursor,
	pageRules []rules.SantaRule,
	lastEvaluatedKey *dynamodb.PrimaryKey,
	machineRules machineRulePage,
	feedServedUpTo *time.Time,
) (*events.APIGatewayProxyResponse, error) {
	if lastEvaluatedKey == nil {
		machineSpecific, err := machineRules.rules(machineID, cursor.syncState, feedServedUpTo)
		if err != nil {
			log.Printf("  Failed to build the machine rules page: %v", err)
			return response.APIResponse(http.StatusInternalServerError, response.ErrInternalServerErrorResponse)
		}
		responseRules := DDBRulesToResponseRules(append(pageRules, machineSpecific...))
		if cursor.Strategy == ruledownloadStrategyClean && len(responseRules) == 0 {
			responseRules = append(responseRules, cleanSyncPlaceholderRule())
		}
		return response.APIResponse(
			http.StatusOK,
			RuledownloadResponse{
				Rules: responseRules,
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
		log.Printf("  Failed to marshal the ruledownload cursor")
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	return response.APIResponse(
		http.StatusOK,
		RuledownloadResponse{
			Rules:  DDBRulesToResponseRules(pageRules),
			Cursor: string(jsonCursor),
		},
	)
}
