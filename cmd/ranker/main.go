// The ranker Lambda, run by an EventBridge Scheduler schedule (hourly by default): it rebuilds the web view's
// top-blocked, top-allowed and top-unknown lists in the events table named by EVENTS_DYNAMODB_NAME, reading global
// rules from the store table named by DYNAMODB_NAME. See internal/ranker.
package main

import (
	"context"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/airbnb/rudolph/internal/ranker"
)

func main() {
	table := os.Getenv("EVENTS_DYNAMODB_NAME")
	if table == "" {
		log.Fatal("ranker: EVENTS_DYNAMODB_NAME is not set")
	}
	// Without the store table the lists are still built; the unknown lists just can't say which binaries a global
	// rule already covers.
	storeTable := os.Getenv("DYNAMODB_NAME")
	if storeTable == "" {
		log.Print("ranker: DYNAMODB_NAME is not set; unknown binaries will not be marked with global rules")
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(os.Getenv("REGION")))
	if err != nil {
		log.Fatalf("ranker: AWS configuration: %v", err)
	}
	client := dynamodb.NewFromConfig(cfg)
	lambda.Start(func(ctx context.Context) error {
		return ranker.Run(ctx, ranker.Options{Client: client, Table: table, StoreTable: storeTable})
	})
}
