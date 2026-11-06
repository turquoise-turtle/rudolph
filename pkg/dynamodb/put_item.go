package dynamodb

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type PutItemAPI interface {
	PutItem(item interface{}) (*dynamodb.PutItemOutput, error)
}

func (dbc concreteDynamoDBClient) PutItem(item interface{}) (*dynamodb.PutItemOutput, error) {
	return putItem(dbc.tableName, &dbc.awsclient, item, dbc.timeout)
}

type dynamodbPutItemAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

func putItem(tableName string, api dynamodbPutItemAPI, item interface{}, timeout time.Duration) (*dynamodb.PutItemOutput, error) {
	return putItemWithCondition(tableName, api, item, nil, timeout)
}

// PutItemIfNotExistsAPI creates an item only if no item with the same primary key exists yet
type PutItemIfNotExistsAPI interface {
	PutItemIfNotExists(item interface{}) (*dynamodb.PutItemOutput, error)
}

// PutItemIfNotExists writes the item unless an item with its primary key already exists; in that case the
// returned error satisfies IsConditionalCheckFailed.
func (dbc concreteDynamoDBClient) PutItemIfNotExists(item interface{}) (*dynamodb.PutItemOutput, error) {
	return putItemWithCondition(dbc.tableName, &dbc.awsclient, item, aws.String("attribute_not_exists(PK)"), dbc.timeout)
}

// PutItemIfAPI writes an item only while a condition on the item it replaces holds
type PutItemIfAPI interface {
	// PutItemIf is PutItem with a condition; when it does not hold, the item is left unchanged and the returned error
	// satisfies IsConditionalCheckFailed.
	PutItemIf(item interface{}, condition expression.ConditionBuilder) (*dynamodb.PutItemOutput, error)
}

func (dbc concreteDynamoDBClient) PutItemIf(item interface{}, condition expression.ConditionBuilder) (*dynamodb.PutItemOutput, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), dbc.timeout)
	defer cancel()

	putItemItem, err := attributevalue.MarshalMap(item)
	if err != nil {
		return nil, err
	}
	expr, err := expression.NewBuilder().WithCondition(condition).Build()
	if err != nil {
		return nil, err
	}
	return dbc.awsclient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 aws.String(dbc.tableName),
		Item:                      putItemItem,
		ConditionExpression:       expr.Condition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
}

func putItemWithCondition(tableName string, api dynamodbPutItemAPI, item interface{}, condition *string, timeout time.Duration) (*dynamodb.PutItemOutput, error) {
	ctx := context.TODO()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	putItemItem, err := attributevalue.MarshalMap(item)

	if err != nil {
		return nil, err
	}

	params := &dynamodb.PutItemInput{
		TableName:           aws.String(tableName),
		Item:                putItemItem,
		ConditionExpression: condition,
	}

	return api.PutItem(ctx, params)
}

// IsConditionalCheckFailed reports whether err was caused by the condition of a conditional write not being met
func IsConditionalCheckFailed(err error) bool {
	var conditionalCheckFailed *types.ConditionalCheckFailedException
	return errors.As(err, &conditionalCheckFailed)
}
