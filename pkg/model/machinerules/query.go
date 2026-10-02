package machinerules

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// @deprecated
func GetMachineRules(client dynamodb.QueryAPI, machineID string) (items *[]MachineRuleRow, err error) {
	partitionKey := machineRulePK(machineID)

	keyConditionExpression := aws.String("PK = :pk")
	expressionAttributeValues := map[string]types.AttributeValue{
		":pk": &types.AttributeValueMemberS{Value: partitionKey},
	}

	// Every rule of the machine is returned, however many pages DynamoDB splits them into (1 MB each)
	rows := []MachineRuleRow{}
	var exclusiveStartKey map[string]types.AttributeValue
	for {
		input := &awsdynamodb.QueryInput{
			ConsistentRead:            aws.Bool(false),
			ExpressionAttributeValues: expressionAttributeValues,
			KeyConditionExpression:    keyConditionExpression,
			ExclusiveStartKey:         exclusiveStartKey,
		}

		result, eerr := client.Query(input)
		if eerr != nil {
			err = fmt.Errorf("failed to read rules from DynamoDB for partitionKey %q: %w", partitionKey, eerr)
			return
		}

		var page []MachineRuleRow
		err = attributevalue.UnmarshalListOfMaps(result.Items, &page)
		if err != nil {
			err = fmt.Errorf("failed to unmarshal result from DynamoDB: %w", err)
			return
		}
		rows = append(rows, page...)

		if len(result.LastEvaluatedKey) == 0 {
			break
		}
		exclusiveStartKey = result.LastEvaluatedKey
	}
	items = &rows
	return
}
