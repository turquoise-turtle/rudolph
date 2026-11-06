// The ranker Lambda, run hourly by EventBridge: it rebuilds the web view's top-blocked and top-allowed lists in the
// events table named by EVENTS_DYNAMODB_NAME. See internal/ranker.
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
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(os.Getenv("REGION")))
	if err != nil {
		log.Fatalf("ranker: AWS configuration: %v", err)
	}
	client := dynamodb.NewFromConfig(cfg)
	lambda.Start(func(ctx context.Context) error {
		return ranker.Run(ctx, ranker.Options{Client: client, Table: table})
	})
}
