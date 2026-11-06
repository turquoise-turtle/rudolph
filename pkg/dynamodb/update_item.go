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

type UpdateItemAPI interface {
	UpdateItem(key PrimaryKey, item interface{}) (*dynamodb.UpdateItemOutput, error)
}

func (dbc concreteDynamoDBClient) UpdateItem(key PrimaryKey, item interface{}) (*dynamodb.UpdateItemOutput, error) {
	return updateItemToDynamoDB(dbc.tableName, &dbc.awsclient, key, item, dbc.timeout)
}

type dynamodbUpdateItemAPI interface {
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// UpdateItemIfAPI updates an existing item only while a condition on it holds
type UpdateItemIfAPI interface {
	// UpdateItemIf is UpdateItem with an additional condition; when it does not hold, the item is left unchanged
	// and the returned error satisfies IsConditionalCheckFailed.
	UpdateItemIf(key PrimaryKey, item interface{}, condition expression.ConditionBuilder) (*dynamodb.UpdateItemOutput, error)
}

func (dbc concreteDynamoDBClient) UpdateItemIf(key PrimaryKey, item interface{}, condition expression.ConditionBuilder) (*dynamodb.UpdateItemOutput, error) {
	return updateItemWithCondition(dbc.tableName, &dbc.awsclient, key, item, &condition, dbc.timeout)
}

// UpdateItemRemoveAPI updates an existing item, setting some attributes and removing others
type UpdateItemRemoveAPI interface {
	// UpdateItemRemove is UpdateItem that also removes the named attributes; removing an attribute the item does
	// not have is not an error.
	UpdateItemRemove(key PrimaryKey, item interface{}, remove []string) (*dynamodb.UpdateItemOutput, error)
	// UpdateItemRemoveIf is UpdateItemRemove with an additional condition; when it does not hold, the item is left
	// unchanged and the returned error satisfies IsConditionalCheckFailed.
	UpdateItemRemoveIf(key PrimaryKey, item interface{}, remove []string, condition expression.ConditionBuilder) (*dynamodb.UpdateItemOutput, error)
}

func (dbc concreteDynamoDBClient) UpdateItemRemove(key PrimaryKey, item interface{}, remove []string) (*dynamodb.UpdateItemOutput, error) {
	return updateItem(dbc.tableName, &dbc.awsclient, key, item, remove, nil, dbc.timeout)
}

func (dbc concreteDynamoDBClient) UpdateItemRemoveIf(key PrimaryKey, item interface{}, remove []string, condition expression.ConditionBuilder) (*dynamodb.UpdateItemOutput, error) {
	return updateItem(dbc.tableName, &dbc.awsclient, key, item, remove, &condition, dbc.timeout)
}

func updateItemToDynamoDB(tableName string, api dynamodbUpdateItemAPI, updateKey PrimaryKey, updateFields interface{}, timeout time.Duration) (*dynamodb.UpdateItemOutput, error) {
	return updateItemWithCondition(tableName, api, updateKey, updateFields, nil, timeout)
}

func updateItemWithCondition(tableName string, api dynamodbUpdateItemAPI, updateKey PrimaryKey, updateFields interface{}, condition *expression.ConditionBuilder, timeout time.Duration) (*dynamodb.UpdateItemOutput, error) {
	return updateItem(tableName, api, updateKey, updateFields, nil, condition, timeout)
}

func updateItem(tableName string, api dynamodbUpdateItemAPI, updateKey PrimaryKey, updateFields interface{}, remove []string, condition *expression.ConditionBuilder, timeout time.Duration) (*dynamodb.UpdateItemOutput, error) {
	ctx := context.TODO()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	key, err := attributevalue.MarshalMap(updateKey)
	if err != nil {
		return nil, err
	}

	// Conditional expression that requires the PK must match the PartitionKey provided
	conditions := expression.Equal(expression.Name("PK"), expression.Value(updateKey.PartitionKey))
	if condition != nil {
		conditions = conditions.And(*condition)
	}

	updateItems, err := attributevalue.MarshalMap(updateFields)
	if err != nil {
		return nil, err
	}

	var updates expression.UpdateBuilder
	for key, value := range updateItems {
		updates = updates.Set(expression.Name(key), expression.Value(value))
	}
	// DynamoDB refuses an expression that names an attribute twice ("Two document paths overlap")
	removed := map[string]bool{}
	for _, name := range remove {
		if _, set := updateItems[name]; set || removed[name] {
			continue
		}
		removed[name] = true
		updates = updates.Remove(expression.Name(name))
	}

	expr, err := expression.NewBuilder().
		WithUpdate(updates).
		WithCondition(conditions).
		Build()

	if err != nil {
		return nil, err
	}

	input := &dynamodb.UpdateItemInput{
		Key:                       key,
		TableName:                 aws.String(tableName),
		UpdateExpression:          expr.Update(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		ConditionExpression:       expr.Condition(),
		ReturnValues:              types.ReturnValueAllNew,
	}

	return api.UpdateItem(ctx, input)
}

// UpdateExpressionAPI updates an item with a raw update expression, for updates the field-by-field SET of UpdateItem
// cannot express (nested map paths, ADD and DELETE on sets, if_not_exists). Unlike UpdateItem it does not require the
// item to exist: an update without a condition creates it.
type UpdateExpressionAPI interface {
	// UpdateItemExpression updates the item at key. condition may be empty; names and values may be nil. When the
	// condition does not hold, the returned error satisfies IsConditionalCheckFailed.
	UpdateItemExpression(key PrimaryKey, update string, condition string, names map[string]string, values map[string]types.AttributeValue) (*dynamodb.UpdateItemOutput, error)
}

func (dbc concreteDynamoDBClient) UpdateItemExpression(key PrimaryKey, update string, condition string, names map[string]string, values map[string]types.AttributeValue) (*dynamodb.UpdateItemOutput, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), dbc.timeout)
	defer cancel()

	keyInput, err := attributevalue.MarshalMap(key)
	if err != nil {
		return nil, err
	}
	input := &dynamodb.UpdateItemInput{
		Key:                       keyInput,
		TableName:                 aws.String(dbc.tableName),
		UpdateExpression:          aws.String(update),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	}
	if condition != "" {
		input.ConditionExpression = aws.String(condition)
	}
	return dbc.awsclient.UpdateItem(ctx, input)
}
