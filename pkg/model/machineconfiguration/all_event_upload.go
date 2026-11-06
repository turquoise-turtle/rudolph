package machineconfiguration

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// AllEventUploadEnabledAnywhere reports whether EnableAllEventUpload is on in the global config or in any machine
// config. Machine configs have no index, so this scans the table (CLI only) unless the global config has it on; it
// stops at the first machine config found.
func AllEventUploadEnabledAnywhere(client interface {
	dynamodb.GetItemAPI
	dynamodb.ScanAPI
}) (bool, error) {
	global, err := GetGlobalConfig(client)
	if err != nil {
		return false, fmt.Errorf("failed to read the global config: %w", err)
	}
	if global != nil && global.EnableAllEventUpload != nil && *global.EnableAllEventUpload {
		return true, nil
	}

	in := &awsdynamodb.ScanInput{
		FilterExpression:     aws.String("begins_with(PK, :machine) AND SK = :config AND EnableAllEventUpload = :on"),
		ProjectionExpression: aws.String("PK"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{
			":machine": &awstypes.AttributeValueMemberS{Value: machineConfigurationPKPrefix},
			":config":  &awstypes.AttributeValueMemberS{Value: currentSK},
			":on":      &awstypes.AttributeValueMemberBOOL{Value: true},
		},
	}
	for {
		out, err := client.Scan(in)
		if err != nil {
			return false, fmt.Errorf("failed to read the machine configs: %w", err)
		}
		if len(out.Items) > 0 {
			return true, nil
		}
		if len(out.LastEvaluatedKey) == 0 {
			return false, nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
	}
}
