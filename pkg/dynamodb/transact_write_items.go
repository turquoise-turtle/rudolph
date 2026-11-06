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

type TransactWriteItemsAPI interface {
	TransactWriteItems(items []types.TransactWriteItem, idempotencyToken *string) (*dynamodb.TransactWriteItemsOutput, error)
	CreateTransactPutItem(item interface{}) (*types.TransactWriteItem, error)
	CreateTransactUpdateItem(key PrimaryKey, item interface{}) (*types.TransactWriteItem, error)
	CreateTransactUpdateExistingItem(key PrimaryKey, item interface{}) (*types.TransactWriteItem, error)
	CreateTransactDeleteItem(key PrimaryKey) (*types.TransactWriteItem, error)
}

func (dbc concreteDynamoDBClient) TransactWriteItems(items []types.TransactWriteItem, idempotencyToken *string) (*dynamodb.TransactWriteItemsOutput, error) {
	return transactWriteItems(&dbc.awsclient, items, idempotencyToken, dbc.timeout)
}

type dynamodbTransactWriteItemsAPI interface {
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

func transactWriteItems(api dynamodbTransactWriteItemsAPI, items []types.TransactWriteItem, idempotencyToken *string, timeout time.Duration) (*dynamodb.TransactWriteItemsOutput, error) {
	ctx := context.TODO()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	input := &dynamodb.TransactWriteItemsInput{
		TransactItems: items,
	}

	if idempotencyToken != nil {
		input.ClientRequestToken = idempotencyToken
	}

	return api.TransactWriteItems(ctx, input)
}

func (dbc concreteDynamoDBClient) CreateTransactPutItem(item interface{}) (writeItem *types.TransactWriteItem, err error) {
	putItem, err := attributevalue.MarshalMap(item)
	if err != nil {
		return nil, err
	}

	writeItem = &types.TransactWriteItem{
		Put: &types.Put{
			Item:      putItem,
			TableName: aws.String(dbc.tableName),
		},
	}
	return
}

func (dbc concreteDynamoDBClient) CreateTransactUpdateItem(primaryKey PrimaryKey, updateFields interface{}) (writeItem *types.TransactWriteItem, err error) {
	return dbc.createTransactUpdateItem(primaryKey, updateFields, false)
}

// CreateTransactUpdateExistingItem is CreateTransactUpdateItem for an item that must already exist: if it does not
// (e.g. it was deleted since it was read), the transaction is cancelled instead of creating a partial item, and
// TransactWriteItems returns an error that satisfies IsTransactionConditionalCheckFailed.
func (dbc concreteDynamoDBClient) CreateTransactUpdateExistingItem(primaryKey PrimaryKey, updateFields interface{}) (writeItem *types.TransactWriteItem, err error) {
	return dbc.createTransactUpdateItem(primaryKey, updateFields, true)
}

func (dbc concreteDynamoDBClient) createTransactUpdateItem(primaryKey PrimaryKey, updateFields interface{}, mustExist bool) (writeItem *types.TransactWriteItem, err error) {
	key, err := attributevalue.MarshalMap(primaryKey)
	if err != nil {
		return nil, err
	}

	updateItems, err := attributevalue.MarshalMap(updateFields)
	if err != nil {
		return nil, err
	}

	var updates expression.UpdateBuilder
	for key, value := range updateItems {
		updates = updates.Set(expression.Name(key), expression.Value(value))
	}

	builder := expression.NewBuilder().WithUpdate(updates)
	if mustExist {
		builder = builder.WithCondition(expression.AttributeExists(expression.Name("PK")))
	}
	expr, err := builder.Build()
	if err != nil {
		return nil, err
	}

	writeItem = &types.TransactWriteItem{
		Update: &types.Update{
			Key:                       key,
			UpdateExpression:          expr.Update(),
			ConditionExpression:       expr.Condition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			TableName:                 aws.String(dbc.tableName),
		},
	}
	return
}

// IsTransactionConditionalCheckFailed reports whether err is a transaction cancelled because the condition of one of
// its items was not met
func IsTransactionConditionalCheckFailed(err error) bool {
	var cancelled *types.TransactionCanceledException
	if !errors.As(err, &cancelled) {
		return false
	}
	for _, reason := range cancelled.CancellationReasons {
		if aws.ToString(reason.Code) == "ConditionalCheckFailed" {
			return true
		}
	}
	return false
}

func (dbc concreteDynamoDBClient) CreateTransactDeleteItem(key PrimaryKey) (writeItem *types.TransactWriteItem, err error) {
	keyInput, err := attributevalue.MarshalMap(key)
	if err != nil {
		return nil, err
	}

	writeItem = &types.TransactWriteItem{
		Delete: &types.Delete{
			Key:       keyInput,
			TableName: aws.String(dbc.tableName),
		},
	}
	return
}

// TransactUpdateExpressionAPI builds transaction updates from a raw update expression, for updates the
// field-by-field SET of CreateTransactUpdateItem cannot express (REMOVE, or ADD and DELETE on sets).
type TransactUpdateExpressionAPI interface {
	CreateTransactUpdateExpression(key PrimaryKey, update string, condition string, names map[string]string, values map[string]types.AttributeValue) (*types.TransactWriteItem, error)
}

// CreateTransactUpdateExpression builds a transaction update of the item at key in this client's table. condition may
// be empty; names and values may be nil.
func (dbc concreteDynamoDBClient) CreateTransactUpdateExpression(key PrimaryKey, update string, condition string, names map[string]string, values map[string]types.AttributeValue) (*types.TransactWriteItem, error) {
	keyInput, err := attributevalue.MarshalMap(key)
	if err != nil {
		return nil, err
	}
	writeItem := &types.TransactWriteItem{
		Update: &types.Update{
			Key:                       keyInput,
			UpdateExpression:          aws.String(update),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
			TableName:                 aws.String(dbc.tableName),
		},
	}
	if condition != "" {
		writeItem.Update.ConditionExpression = aws.String(condition)
	}
	return writeItem, nil
}
