//go:build integration

package ddblocal

import (
	"context"
	"os"
	"testing"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

func TestRudolphClientTalksToDynamoDBLocalAndIsCounted(t *testing.T) {
	Reset(t)

	type row struct {
		dynamodb.PrimaryKey
		Value string `dynamodbav:"Value"`
	}
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
