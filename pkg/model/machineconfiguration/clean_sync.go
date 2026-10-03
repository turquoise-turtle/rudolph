package machineconfiguration

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
)

// updateCleanSync is the fragment written to clear the clean sync flag
type updateCleanSync struct {
	CleanSync bool `dynamodbav:"CleanSync"`
}

// clearMachineCleanSync clears the clean sync flag of a machine-specific config once it has been acted on, so the
// flag makes exactly one sync clean. It only changes a row that still has the flag set; a row that was deleted or
// already cleared in the meantime is left alone.
func clearMachineCleanSync(client dynamodb.UpdateItemIfAPI, machineID string) error {
	_, err := client.UpdateItemIf(
		dynamodb.PrimaryKey{
			PartitionKey: machineConfigurationPK(machineID),
			SortKey:      machineConfigurationSK(),
		},
		updateCleanSync{CleanSync: false},
		expression.Equal(expression.Name("CleanSync"), expression.Value(true)),
	)
	if dynamodb.IsConditionalCheckFailed(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("clearing the clean sync flag of machine config %s failed: %w", machineID, err)
	}
	return nil
}
