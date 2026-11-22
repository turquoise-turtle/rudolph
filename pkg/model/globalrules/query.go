package globalrules

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func PingDatabase(client dynamodb.QueryAPI) (err error) {
	_, _, err = GetPaginatedGlobalRules(client, 1, nil)
	return
}

func GetPaginatedGlobalRules(
	client dynamodb.QueryAPI,
	limit int,
	exclusiveStartKey *dynamodb.PrimaryKey,
) (
	items []*GlobalRuleRow,
	lastEvaluatedKey *dynamodb.PrimaryKey,
	err error,
) {
	if limit <= 0 {
		err = errors.New("invalid limit/batchsize specified")
		return
	}
	return QueryGlobalRules(client, limit, exclusiveStartKey)
}

// QueryGlobalRules is one Query of the global rules, in sort key order, after exclusiveStartKey. With a limit of 0
// DynamoDB returns as many rules as fit in 1 MB, which is what a caller that sizes its pages itself wants: a limit in
// rules costs a request per page however small the rules are.
func QueryGlobalRules(
	client dynamodb.QueryAPI,
	limit int,
	exclusiveStartKey *dynamodb.PrimaryKey,
) (
	items []*GlobalRuleRow,
	lastEvaluatedKey *dynamodb.PrimaryKey,
	err error,
) {
	partitionKey := globalRulesPK

	keyCond := expression.KeyEqual(
		expression.Key("PK"), expression.Value(partitionKey),
	)

	var exclusiveStartKeyInput map[string]types.AttributeValue
	if exclusiveStartKey != nil {
		exclusiveStartKeyInput, err = attributevalue.MarshalMap(exclusiveStartKey)
		if err != nil {
			err = fmt.Errorf("failed to marshall exclusiveStartKey: %w", err)
			return
		}
	}

	expr, err := expression.NewBuilder().WithKeyCondition(keyCond).Build()
	if err != nil {
		return
	}

	input := &awsdynamodb.QueryInput{
		ConsistentRead:            aws.Bool(false),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		KeyConditionExpression:    expr.KeyCondition(),
		ExclusiveStartKey:         exclusiveStartKeyInput,
	}
	if limit > 0 {
		input.Limit = aws.Int32(int32(limit))
	}

	// log.Printf("Executing DynamoDB Query:\n%+v", input)

	result, err := client.Query(input)
	if err != nil {
		err = fmt.Errorf("failed to read rules from DynamoDB for partitionKey %q: %w", partitionKey, err)
		return
	}

	if result.LastEvaluatedKey != nil {
		err = attributevalue.UnmarshalMap(result.LastEvaluatedKey, &lastEvaluatedKey)
		if err != nil {
			err = fmt.Errorf("failed to UnmarshalMap LastEvaluatedKey: %w", err)
			return
		}
	}

	err = attributevalue.UnmarshalListOfMaps(result.Items, &items)
	if err != nil {
		err = fmt.Errorf("failed to UnmarshalListOfMaps result from DynamoDB: %w", err)
		return
	}

	// To support legacy SHA256 types, we must transform the datasets before returning
	for _, item := range items {
		if item.SHA256 != "" && item.Identifier == "" {
			item.Identifier = item.SHA256
		}
	}
	return
}
