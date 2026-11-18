package ruledownload

// Input handling review: bodies and cursors a holder of the fleet's sync token can send to /ruledownload that no
// Santa sends. The cursor is returned to the client as plain JSON and taken back as it comes.

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

const inputHandlingMachineID = "AAAAAAAA-A00A-1234-1234-5864377B4831"

func inputHandlingRequest(body string) events.APIGatewayProxyRequest {
	return events.APIGatewayProxyRequest{
		HTTPMethod:     "POST",
		Resource:       "/ruledownload/{machine_id}",
		Headers:        map[string]string{"Content-Type": "application/json"},
		PathParameters: map[string]string{"machine_id": inputHandlingMachineID},
		Body:           body,
	}
}

// strictQueryer answers a Query the way DynamoDB answers the ones a forged cursor produces: a Limit below 1, or an
// ExclusiveStartKey outside the partition being queried, is a ValidationException. Any other query fails the test,
// as none of the cursors below should get that far with a valid one.
type strictQueryer struct {
	t       *testing.T
	queries int
}

func (q *strictQueryer) Query(input *awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error) {
	q.queries++
	if input.Limit != nil && *input.Limit < 1 {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "Limit must be greater than or equal to 1"}
	}
	if start, ok := input.ExclusiveStartKey["PK"].(*types.AttributeValueMemberS); ok {
		for _, value := range input.ExpressionAttributeValues {
			if queried, ok := value.(*types.AttributeValueMemberS); ok && queried.Value != start.Value {
				return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "The provided starting key is invalid"}
			}
		}
	}
	q.t.Errorf("unexpected valid query: %+v", input)
	return &awsdynamodb.QueryOutput{}, nil
}

// realHandler is the handler as Boot wires it, with the real cursor service and the real global and feed
// downloaders, over a table that only strictQueryer stands in for. A request carrying a cursor never reads the sync
// state before its page is queried, so no other table access is needed for the cursors tested here.
func realHandler(t *testing.T) (*PostRuledownloadHandler, *strictQueryer) {
	queryer := &strictQueryer{t: t}
	return &PostRuledownloadHandler{
		booted:        true,
		cursorService: concreteRuledownloadCursorService{},
		ghandler:      concreteGlobalRuleDownloader{queryer: queryer},
		fhandler:      concreteFeedRuleDownloader{queryer: queryer},
		mhandler: mockMachineRuleDownloader(func(string, *RuledownloadRequest) (*events.APIGatewayProxyResponse, error) {
			t.Error("the machine page was requested")
			return nil, nil
		}),
	}, queryer
}

// A body of JSON `null` unmarshals without error and leaves the request pointer nil. The handler must answer 400
// instead of dereferencing it: a panic ends the Lambda's process, API Gateway answers 502, the lambda_errors alarm
// counts it and the next request pays a cold start.
func TestRuledownload_NullBodyIsABadRequestNotAPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the handler panicked on a body of null: %v", r)
		}
	}()
	h, _ := realHandler(t)
	resp, err := h.Handle(inputHandlingRequest(`null`))
	if err != nil || resp == nil || resp.StatusCode != 400 {
		t.Fatalf("a body of null was answered %+v, %v; want 400", resp, err)
	}
}

// A cursor the server could never have issued is a bad request. Today each of these is answered 500 (after a query
// DynamoDB refuses, for most of them), which the api_5xx alarm counts as a server failure and which Santa retries.
func TestRuledownload_ForgedCursorIsABadRequestNotAServerError(t *testing.T) {
	for name, cursor := range map[string]string{
		"JSON null":                           `null`,
		"empty object":                        `{}`,
		"no strategy":                         `{"batch_size":50,"page":2}`,
		"unknown strategy":                    `{"strategy":99,"batch_size":50,"page":2}`,
		"negative strategy":                   `{"strategy":-1,"batch_size":50,"page":2}`,
		"clean page without a batch size":     `{"strategy":1,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"clean page with a negative batch":    `{"strategy":1,"batch_size":-5,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"batch size that overflows the limit": `{"strategy":1,"batch_size":2147483648,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"clean page from another partition":   `{"strategy":1,"batch_size":50,"page":2,"pk":"MachineRules#` + inputHandlingMachineID + `","sk":"Binary#aa"}`,
		"feed page from another partition":    `{"strategy":2,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`,
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"cursor": cursor})
			if err != nil {
				t.Fatal(err)
			}
			h, queryer := realHandler(t)
			resp, err := h.Handle(inputHandlingRequest(string(body)))
			if err != nil || resp == nil {
				t.Fatalf("ruledownload failed: %v %+v", err, resp)
			}
			if resp.StatusCode != 400 {
				t.Errorf("cursor %s was answered %d (%d queries sent to DynamoDB), want 400 without querying", cursor, resp.StatusCode, queryer.queries)
			}
		})
	}
}
