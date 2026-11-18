package ruledownload

// Review of 9580b58: cursors that pass ruledownloadCursor.validate and still give DynamoDB an ExclusiveStartKey it
// refuses, so the request is answered 500 as before the fix.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

// keyCheckingQueryer refuses an ExclusiveStartKey the way DynamoDB does: both key attributes must be non-empty
// strings and the sort key at most 1024 bytes. Any other query is answered with an empty last page.
type keyCheckingQueryer struct {
	queries int
}

func (q *keyCheckingQueryer) Query(input *awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error) {
	q.queries++
	if input.ExclusiveStartKey != nil {
		for _, name := range []string{"PK", "SK"} {
			value, ok := input.ExclusiveStartKey[name].(*types.AttributeValueMemberS)
			if !ok || value.Value == "" {
				return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "The provided starting key is invalid: an AttributeValue for a key attribute cannot contain an empty string value. Key: " + name}
			}
			if name == "SK" && len(value.Value) > 1024 {
				return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "Size of rangekey has exceeded the maximum size limit of 1024 bytes"}
			}
		}
	}
	return &awsdynamodb.QueryOutput{}, nil
}

func TestReview_CursorWithAKeyDynamoDBRefusesIsABadRequest(t *testing.T) {
	for name, cursor := range map[string]string{
		"clean page, sort key without a partition key": `{"strategy":1,"batch_size":50,"page":2,"sk":"Binary#aa"}`,
		"clean page, partition key without a sort key": `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules"}`,
		"feed page, sort key without a partition key":  `{"strategy":2,"batch_size":50,"page":2,"sk":"2026-01-01T00:00:00Z#Binary#aa"}`,
		"feed page, partition key without a sort key":  `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed"}`,
		"clean page, sort key over 1024 bytes":         `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"` + strings.Repeat("a", 1025) + `"}`,
		"feed page, sort key over 1024 bytes":          `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"` + strings.Repeat("a", 1025) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"cursor": cursor})
			if err != nil {
				t.Fatal(err)
			}
			queryer := &keyCheckingQueryer{}
			h := &PostRuledownloadHandler{
				booted:        true,
				cursorService: concreteRuledownloadCursorService{},
				ghandler:      concreteGlobalRuleDownloader{queryer: queryer},
				fhandler:      concreteFeedRuleDownloader{queryer: queryer},
				mhandler: mockMachineRuleDownloader(func(string, *RuledownloadRequest) (*events.APIGatewayProxyResponse, error) {
					t.Error("the machine page was requested")
					return nil, nil
				}),
			}
			resp, err := h.Handle(inputHandlingRequest(string(body)))
			if resp == nil {
				t.Fatalf("ruledownload failed: %v", err)
			}
			if resp.StatusCode != 400 || queryer.queries != 0 {
				t.Errorf("cursor %.80s was answered %d after %d queries, want 400 without querying", cursor, resp.StatusCode, queryer.queries)
			}
		})
	}
}
