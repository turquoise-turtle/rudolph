//go:build integration

package handlers

// Helpers for batch 4 tests: moving stored time back (instead of the clock forward), simulated TTL deletion, and
// writing many rows quickly.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// timestampAttributes hold RFC 3339 times; millisecondAttributes hold sync numbers, which are at least the preflight
// time in milliseconds (see syncstate.SyncState.SyncSeq).
var (
	timestampAttributes   = []string{"PreflightAt", "PostflightAt", "LastCleanSync", "FeedSyncCursor", "MachineRulesTrackingRefreshedAt", "Time", "RuledownloadStartedAt", "RuledownloadFinishedAt"}
	millisecondAttributes = []string{"SyncSeq", "RemovalFirstSentInSync"}
)

func shiftTimestamp(v string, d time.Duration) string {
	ts, err := clock.ParseRFC3339(v)
	if err != nil {
		return v
	}
	return clock.RFC3339(ts.Add(-d))
}

func shiftNumber(av awstypes.AttributeValue, by int64) awstypes.AttributeValue {
	n, ok := av.(*awstypes.AttributeValueMemberN)
	if !ok {
		return av
	}
	v, err := strconv.ParseInt(n.Value, 10, 64)
	if err != nil || v == 0 {
		return av
	}
	return &awstypes.AttributeValueMemberN{Value: strconv.FormatInt(v-by, 10)}
}

// ageRows makes every stored item look as if it had been written d earlier: expiry times, timestamps, sync numbers
// and the time-ordered sort keys of the rules feed all move back by d. Moving stored time back is equivalent to
// moving Rudolph's clock forward, which the handlers do not allow.
func ageRows(t *testing.T, d time.Duration) {
	t.Helper()
	for _, item := range scanAll(t) {
		if v, ok := item["ExpiresAfter"]; ok {
			item["ExpiresAfter"] = shiftNumber(v, int64(d/time.Second))
		}
		for _, name := range timestampAttributes {
			if s, ok := item[name].(*awstypes.AttributeValueMemberS); ok {
				item[name] = &awstypes.AttributeValueMemberS{Value: shiftTimestamp(s.Value, d)}
			}
		}
		for _, name := range millisecondAttributes {
			if v, ok := item[name]; ok {
				item[name] = shiftNumber(v, d.Milliseconds())
			}
		}
		if list, ok := item["CompletedSyncs"].(*awstypes.AttributeValueMemberL); ok {
			for _, element := range list.Value {
				if m, ok := element.(*awstypes.AttributeValueMemberM); ok {
					if s, ok := m.Value["PreflightAt"].(*awstypes.AttributeValueMemberS); ok {
						m.Value["PreflightAt"] = &awstypes.AttributeValueMemberS{Value: shiftTimestamp(s.Value, d)}
					}
					if v, ok := m.Value["Seq"]; ok {
						m.Value["Seq"] = shiftNumber(v, d.Milliseconds())
					}
				}
			}
		}
		pk := item["PK"].(*awstypes.AttributeValueMemberS).Value
		sk := item["SK"].(*awstypes.AttributeValueMemberS).Value
		if pk == "RulesFeed" {
			if i := strings.Index(sk, "#"); i > 0 {
				deleteItem(t, pk, sk)
				item["SK"] = &awstypes.AttributeValueMemberS{Value: shiftTimestamp(sk[:i], d) + sk[i:]}
			}
		}
		if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: item}); err != nil {
			t.Fatal(err)
		}
	}
	handlers = newHandlers() // a later Lambda instance
}

func deleteItem(t *testing.T, pk, sk string) {
	t.Helper()
	if _, err := ddblocal.Raw(t).DeleteItem(context.Background(), &awsdynamodb.DeleteItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: pk},
			"SK": &awstypes.AttributeValueMemberS{Value: sk},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// expireTTL deletes every item whose ExpiresAfter (the table's TTL attribute) has passed, as DynamoDB eventually does.
// It returns the keys deleted.
func expireTTL(t *testing.T) []string {
	t.Helper()
	now := time.Now().Unix()
	var deleted []string
	for _, item := range scanAll(t) {
		n, ok := item["ExpiresAfter"].(*awstypes.AttributeValueMemberN)
		if !ok {
			continue
		}
		v, _ := strconv.ParseInt(n.Value, 10, 64)
		if v == 0 || v >= now {
			continue
		}
		pk := item["PK"].(*awstypes.AttributeValueMemberS).Value
		sk := item["SK"].(*awstypes.AttributeValueMemberS).Value
		deleteItem(t, pk, sk)
		deleted = append(deleted, pk+"/"+sk)
	}
	return deleted
}

// batchPut writes items with BatchWriteItem, 25 at a time.
func batchPut(t *testing.T, items []map[string]any) {
	t.Helper()
	for start := 0; start < len(items); start += 25 {
		var requests []awstypes.WriteRequest
		for _, item := range items[start:min(start+25, len(items))] {
			av, err := attributevalue.MarshalMap(item)
			if err != nil {
				t.Fatal(err)
			}
			requests = append(requests, awstypes.WriteRequest{PutRequest: &awstypes.PutRequest{Item: av}})
		}
		request := map[string][]awstypes.WriteRequest{ddblocal.TableName: requests}
		for len(request) > 0 {
			out, err := ddblocal.Raw(t).BatchWriteItem(context.Background(), &awsdynamodb.BatchWriteItemInput{RequestItems: request})
			if err != nil {
				t.Fatal(err)
			}
			request = out.UnprocessedItems
		}
	}
}

func trackingItems(t *testing.T, machineID string) map[string]map[string]awstypes.AttributeValue {
	t.Helper()
	out := map[string]map[string]awstypes.AttributeValue{}
	for _, item := range scanAll(t) {
		if item["PK"].(*awstypes.AttributeValueMemberS).Value == "MachineRuleDelivery#"+machineID {
			out[item["SK"].(*awstypes.AttributeValueMemberS).Value] = item
		}
	}
	return out
}

func numberAttr(t *testing.T, item map[string]awstypes.AttributeValue, name string) int64 {
	t.Helper()
	n, ok := item[name].(*awstypes.AttributeValueMemberN)
	if !ok {
		return 0
	}
	v, err := strconv.ParseInt(n.Value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func stringAttr(item map[string]awstypes.AttributeValue, name string) string {
	if s, ok := item[name].(*awstypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

// failingStage returns a transport that answers the given stage with status while *fail is true.
func failingStage(base func(events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error), stage string, status int, fail *bool) func(events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	return func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		if *fail && req.Resource == "/"+stage+"/{machine_id}" {
			return &events.APIGatewayProxyResponse{StatusCode: status, Body: `{}`}, nil
		}
		return base(req)
	}
}

func shaN(i int) string { return fmt.Sprintf("%064x", i) }
