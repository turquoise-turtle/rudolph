package machinerules

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// HasMachineRules reports whether any machine rule row is stored under machineID, reading at most one key
func HasMachineRules(client dynamodb.QueryAPI, machineID string) (bool, error) {
	result, err := client.Query(&awsdynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: machineRulePK(machineID)}},
		ProjectionExpression:      aws.String("PK"),
		Limit:                     aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("failed to look for machine rules of %s: %w", machineID, err)
	}
	return len(result.Items) > 0, nil
}
