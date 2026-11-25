// The fleet Lambda, run by an EventBridge Scheduler schedule (once a day by default): it rebuilds the web view's fleet
// health lists and Lockdown readiness figures in the events table named by EVENTS_DYNAMODB_NAME, from the Macs,
// configuration, groups and global rules in the store table named by DYNAMODB_NAME. See internal/fleet and
// docs/fleet-health.md.
//
// A run that fails returns its error, so Lambda counts it in Errors: the job's only alarm is "no successful run for
// N days", built from Lambda's own metrics.
package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/airbnb/rudolph/internal/fleet"
)

func main() {
	table := os.Getenv("EVENTS_DYNAMODB_NAME")
	if table == "" {
		log.Fatal("fleet: EVENTS_DYNAMODB_NAME is not set")
	}
	storeTable := os.Getenv("DYNAMODB_NAME")
	if storeTable == "" {
		log.Fatal("fleet: DYNAMODB_NAME is not set")
	}
	// Optional; the default is internal/fleet's.
	maxMacs := 0
	if v := os.Getenv("FLEET_MAX_MACS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			log.Fatalf("fleet: FLEET_MAX_MACS %q is not a positive number", v)
		}
		maxMacs = n
	}
	// An environment deployed before a setting moved may still set it on the function; the job says so and carries on.
	for _, line := range ignoredEnvironment(os.Getenv) {
		log.Print(line)
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(os.Getenv("REGION")))
	if err != nil {
		log.Fatalf("fleet: AWS configuration: %v", err)
	}
	client := dynamodb.NewFromConfig(cfg)
	lambda.Start(func(ctx context.Context) error {
		return fleet.Run(ctx, fleet.Options{Client: client, Table: table, StoreTable: storeTable, MaxMacs: maxMacs})
	})
}
