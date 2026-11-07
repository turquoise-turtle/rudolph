package dynamodb

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// https://aws.amazon.com/blogs/developer/aws-sdk-for-go-version-2-general-availability/

type DynamoDBClient interface {
	DeleteItemAPI
	DeleteItemIfAPI
	GetItemAPI
	BatchGetItemAPI
	BatchWriteItemAPI
	ClockSkewAPI
	PutItemAPI
	PutItemIfNotExistsAPI
	UpdateItemAPI
	UpdateItemIfAPI
	UpdateItemRemoveAPI
	QueryAPI
	TransactWriteItemsAPI
	ScanAPI
}

type concreteDynamoDBClient struct {
	awsclient dynamodb.Client
	tableName string
	timeout   time.Duration
}

// GetClient returns a client of the table in the region, or an error when the AWS configuration (profile,
// credentials source) cannot be loaded.
func GetClient(inputTableName string, region string) (DynamoDBClient, error) {
	return GetClientWithTimeout(inputTableName, region, defaultTimeout)
}

// GetClientWithTimeout is GetClient with the timeout of each request.
func GetClientWithTimeout(inputTableName string, region string, timeout time.Duration) (DynamoDBClient, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("unable to load the AWS configuration: %w", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	return concreteDynamoDBClient{
		awsclient: *client,
		tableName: inputTableName,
		timeout:   timeout,
	}, nil
}

const (
	defaultTimeoutMS = 5000
	defaultTimeout   = defaultTimeoutMS * time.Millisecond
)
