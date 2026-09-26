package dynamodb

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DeleteItemAPI interface {
	DeleteItem(key PrimaryKey) (*dynamodb.DeleteItemOutput, error)
}

func (dbc concreteDynamoDBClient) DeleteItem(key PrimaryKey) (*dynamodb.DeleteItemOutput, error) {
	return deleteItemFromDynamoDB(dbc.tableName, &dbc.awsclient, key, dbc.timeout)
}

type dynamodbDeleteItemAPI interface {
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

func deleteItemFromDynamoDB(tableName string, api dynamodbDeleteItemAPI, key PrimaryKey, timeout time.Duration) (*dynamodb.DeleteItemOutput, error) {
	return deleteItemWithCondition(tableName, api, key, nil, timeout)
}

// DeleteItemIfAPI deletes an item only while a condition on it holds
type DeleteItemIfAPI interface {
	DeleteItemIf(key PrimaryKey, condition expression.ConditionBuilder) (*dynamodb.DeleteItemOutput, error)
}

// DeleteItemIf deletes the item only if the condition holds; otherwise the returned error satisfies
// IsConditionalCheckFailed and the item is left unchanged.
func (dbc concreteDynamoDBClient) DeleteItemIf(key PrimaryKey, condition expression.ConditionBuilder) (*dynamodb.DeleteItemOutput, error) {
	return deleteItemWithCondition(dbc.tableName, &dbc.awsclient, key, &condition, dbc.timeout)
}

func deleteItemWithCondition(tableName string, api dynamodbDeleteItemAPI, key PrimaryKey, condition *expression.ConditionBuilder, timeout time.Duration) (*dynamodb.DeleteItemOutput, error) {
	ctx := context.TODO()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var keyInput map[string]types.AttributeValue
	keyInput, _ = attributevalue.MarshalMap(key)

	params := &dynamodb.DeleteItemInput{
		Key:       keyInput,
		TableName: aws.String(tableName),
	}
	if condition != nil {
		expr, err := expression.NewBuilder().WithCondition(*condition).Build()
		if err != nil {
			return nil, err
		}
		params.ConditionExpression = expr.Condition()
		params.ExpressionAttributeNames = expr.Names()
		params.ExpressionAttributeValues = expr.Values()
	}

	return api.DeleteItem(ctx, params)
}
