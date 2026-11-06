//go:build integration

package ddblocal

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

type row struct {
	dynamodb.PrimaryKey
	Value string `dynamodbav:"Value"`
}

func TestRudolphClientTalksToDynamoDBLocalAndIsCounted(t *testing.T) {
	Reset(t)

	if _, err := Client(t).PutItem(row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "s"}, "v"}); err != nil {
		t.Fatalf("PutItem through Rudolph's client: %v", err)
	}
	out, err := Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(TableName)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 {
		t.Fatalf("expected 1 item in DynamoDB Local, got %d", out.Count)
	}
	if got := Counts()["PutItem"]; got != 1 {
		t.Fatalf("proxy counted %d PutItem calls, want 1 (counts: %v)", got, Counts())
	}

	Reset(t)
	out, _ = Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(TableName)})
	if out.Count != 0 {
		t.Fatalf("Reset must empty the table, got %d items", out.Count)
	}
}

// The test table is created from the Terraform definition, so every index production has exists in tests too.
func TestTableIsCreatedFromTheTerraformDefinition(t *testing.T) {
	Reset(t)
	tf, err := os.ReadFile(StoreTerraformFile())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := Raw(t).DescribeTable(context.Background(), &awsdynamodb.DescribeTableInput{TableName: aws.String(TableName)})
	if err != nil {
		t.Fatal(err)
	}
	indexes := map[string]bool{}
	for _, gsi := range desc.Table.GlobalSecondaryIndexes {
		indexes[*gsi.IndexName] = true
		if !strings.Contains(string(tf), `"`+*gsi.IndexName+`"`) {
			t.Errorf("index %s is not in %s", *gsi.IndexName, StoreTerraformFile())
		}
	}
	for _, want := range []string{"DataType_MachineID", "SerialNum_DataType_MachineID"} {
		if !indexes[want] {
			t.Errorf("index %s missing from the test table (have %v)", want, indexes)
		}
	}
	if got := strings.Count(string(tf), "global_secondary_index {"); got != len(indexes) {
		t.Errorf("Terraform defines %d indexes, the test table has %d", got, len(indexes))
	}
}

// ConsumedCapacity reports what DynamoDB Local charges, including global secondary index writes. Like DynamoDB, it
// charges an index only when a write changes the item's index keys or projected attributes, and a write by the
// larger of the old and new item size.
func TestConsumedCapacityIncludesIndexWrites(t *testing.T) {
	Reset(t)
	type sensorLike struct {
		dynamodb.PrimaryKey
		DataType  string `dynamodbav:"DataType"`
		MachineID string `dynamodbav:"MachineID"`
		SerialNum string `dynamodbav:"SerialNum"`
		Pad       string `dynamodbav:"Pad"`
	}
	item := sensorLike{dynamodb.PrimaryKey{PartitionKey: "Machine#x", SortKey: "Current"}, "SensorData", "x", "C02X", "small"}
	if _, err := Client(t).PutItem(item); err != nil {
		t.Fatal(err)
	}
	c := ConsumedCapacity()
	if c.WriteUnits != 3 || c.TableWriteUnits != 1 || c.IndexWriteUnits["DataType_MachineID"] != 1 || c.IndexWriteUnits["SerialNum_DataType_MachineID"] != 1 {
		t.Fatalf("a new item in both indexes should cost 1 WCU on the table and 1 per index: %s", c)
	}

	ResetCounts()
	item.Pad = strings.Repeat("p", 1500)
	if _, err := Client(t).PutItem(item); err != nil {
		t.Fatal(err)
	}
	if c := ConsumedCapacity(); c.WriteUnits != 2 || c.TableWriteUnits != 2 {
		t.Fatalf("rewriting a 1.5 KB item with unchanged index attributes should cost 2 WCU and nothing on the indexes: %s", c)
	}

	ResetCounts()
	if _, err := Client(t).GetItem(item.PrimaryKey, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Client(t).GetItem(item.PrimaryKey, false); err != nil {
		t.Fatal(err)
	}
	if c := ConsumedCapacity(); c.ReadUnits != 1.5 {
		t.Fatalf("a strongly consistent read of a small item costs 1 RCU, an eventually consistent one 0.5: %s", c)
	}
}

func TestFailNextAnswersWithAFaultAndTheSDKRetriesThrottling(t *testing.T) {
	Reset(t)
	FailNext(t, "PutItem", 1, FaultThrottled)
	if _, err := Client(t).PutItem(row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "1"}, "v"}); err != nil {
		t.Fatalf("the SDK should have retried a single throttled request: %v", err)
	}
	if got := Counts()["PutItem"]; got != 2 {
		t.Fatalf("expected the throttled attempt and its retry, got %d PutItem calls", got)
	}

	FailNext(t, "PutItem", 1, FaultAccessDenied)
	_, err := Client(t).PutItem(row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "2"}, "v"})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDeniedException" {
		t.Fatalf("expected AccessDeniedException, got %v", err)
	}
	if item, _ := Client(t).GetItem(dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "2"}, true); len(item.Item) != 0 {
		t.Fatal("a call answered with a fault must not reach DynamoDB Local")
	}
}

func TestLeaveUnprocessedReturnsPartOfABatch(t *testing.T) {
	Reset(t)
	LeaveUnprocessed(t, 1)
	puts := []interface{}{
		row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "1"}, "v"},
		row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "2"}, "v"},
		row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "3"}, "v"},
	}
	if err := Client(t).BatchWriteItems(puts, nil); err != nil {
		t.Fatal(err)
	}
	if got := Counts()["BatchWriteItem"]; got != 2 {
		t.Fatalf("Rudolph's client should have resent the unprocessed items once, got %d BatchWriteItem calls", got)
	}
	for _, sk := range []string{"1", "2", "3"} {
		if item, _ := Client(t).GetItem(dynamodb.PrimaryKey{PartitionKey: "p", SortKey: sk}, true); len(item.Item) == 0 {
			t.Errorf("item %s missing after the retry", sk)
		}
	}
}

func TestPauseFirstHoldsACallUntilReleased(t *testing.T) {
	Reset(t)
	pause := PauseFirst(t, func(c *Call) bool { return c.Op == "PutItem" && strings.Contains(c.InputJSON(), `"paused"`) })
	done := make(chan error)
	go func() {
		_, err := Client(t).PutItem(row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "paused"}, "v"})
		done <- err
	}()
	select {
	case <-pause.Reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the call was never paused")
	}
	// Other calls are not held.
	if _, err := Client(t).PutItem(row{dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "other"}, "v"}); err != nil {
		t.Fatal(err)
	}
	if item, _ := Client(t).GetItem(dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "paused"}, true); len(item.Item) != 0 {
		t.Fatal("the paused call reached DynamoDB Local before it was released")
	}
	pause.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if item, _ := Client(t).GetItem(dynamodb.PrimaryKey{PartitionKey: "p", SortKey: "paused"}, true); len(item.Item) == 0 {
		t.Fatal("the released call was not executed")
	}
}

// LoseResponse: the write happens, but the client gets the fault.
func TestLoseResponseAppliesTheCallButAnswersAFault(t *testing.T) {
	Reset(t)
	lost := false
	AddHook(t, Hook{LoseResponse: func(c *Call) *Fault {
		if c.Op == "PutItem" && !lost {
			lost = true
			return &Fault{Status: 400, Type: "ValidationException", Message: "lost"}
		}
		return nil
	}})
	item := map[string]awstypes.AttributeValue{
		"PK": &awstypes.AttributeValueMemberS{Value: "Lost"},
		"SK": &awstypes.AttributeValueMemberS{Value: "Response"},
	}
	if _, err := Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(TableName), Item: item}); err == nil {
		t.Fatal("the client got no error")
	}
	out, err := Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{TableName: aws.String(TableName), Key: item, ConsistentRead: aws.Bool(true)})
	if err != nil || len(out.Item) == 0 {
		t.Fatalf("the write was not applied: %v %v", err, out.Item)
	}
}
