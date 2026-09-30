package dynamodb

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	// DynamoDB limits: keys per BatchGetItem, and requests per BatchWriteItem
	batchGetLimit   = 100
	batchWriteLimit = 25
	// How many times unprocessed keys or items (throttling) are retried before giving up
	batchMaxAttempts = 8
)

// BatchGetItemAPI reads several items by primary key
type BatchGetItemAPI interface {
	// BatchGetItems returns the items that exist among keys, in no particular order. It makes one BatchGetItem request
	// per 100 keys (plus retries of keys DynamoDB did not process).
	BatchGetItems(keys []PrimaryKey, consistentRead bool) ([]map[string]types.AttributeValue, error)
}

// BatchWriteItemAPI writes and deletes several items
type BatchWriteItemAPI interface {
	// BatchWriteItems puts every item and deletes every key, one BatchWriteItem request per 25 of them (plus retries of
	// requests DynamoDB did not process). The writes are not a transaction.
	BatchWriteItems(puts []interface{}, deletes []PrimaryKey) error
}

func (dbc concreteDynamoDBClient) BatchGetItems(keys []PrimaryKey, consistentRead bool) (items []map[string]types.AttributeValue, err error) {
	for start := 0; start < len(keys); start += batchGetLimit {
		chunk := keys[start:min(start+batchGetLimit, len(keys))]
		requestKeys := make([]map[string]types.AttributeValue, len(chunk))
		for i, key := range chunk {
			requestKeys[i], err = attributevalue.MarshalMap(key)
			if err != nil {
				return nil, err
			}
		}
		request := map[string]types.KeysAndAttributes{
			dbc.tableName: {Keys: requestKeys, ConsistentRead: aws.Bool(consistentRead)},
		}
		for attempt := 0; len(request) > 0; attempt++ {
			if attempt == batchMaxAttempts {
				return nil, fmt.Errorf("BatchGetItem left keys unprocessed after %d attempts", batchMaxAttempts)
			}
			backoff(attempt)
			output, err := dbc.batchGetItem(request)
			if err != nil {
				return nil, err
			}
			items = append(items, output.Responses[dbc.tableName]...)
			request = output.UnprocessedKeys
		}
	}
	return items, nil
}

func (dbc concreteDynamoDBClient) batchGetItem(request map[string]types.KeysAndAttributes) (*dynamodb.BatchGetItemOutput, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), dbc.timeout)
	defer cancel()
	return dbc.awsclient.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: request})
}

func (dbc concreteDynamoDBClient) BatchWriteItems(puts []interface{}, deletes []PrimaryKey) error {
	var requests []types.WriteRequest
	for _, item := range puts {
		av, err := attributevalue.MarshalMap(item)
		if err != nil {
			return err
		}
		requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: av}})
	}
	for _, key := range deletes {
		av, err := attributevalue.MarshalMap(key)
		if err != nil {
			return err
		}
		requests = append(requests, types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: av}})
	}

	for start := 0; start < len(requests); start += batchWriteLimit {
		request := map[string][]types.WriteRequest{
			dbc.tableName: requests[start:min(start+batchWriteLimit, len(requests))],
		}
		for attempt := 0; len(request) > 0; attempt++ {
			if attempt == batchMaxAttempts {
				return fmt.Errorf("BatchWriteItem left requests unprocessed after %d attempts", batchMaxAttempts)
			}
			backoff(attempt)
			output, err := dbc.batchWriteItem(request)
			if err != nil {
				return err
			}
			request = output.UnprocessedItems
		}
	}
	return nil
}

func (dbc concreteDynamoDBClient) batchWriteItem(request map[string][]types.WriteRequest) (*dynamodb.BatchWriteItemOutput, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), dbc.timeout)
	defer cancel()
	return dbc.awsclient.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: request})
}

// backoff waits before retrying requests DynamoDB did not process (it throttled them)
func backoff(attempt int) {
	if attempt > 0 {
		time.Sleep(time.Duration(25<<attempt) * time.Millisecond)
	}
}
