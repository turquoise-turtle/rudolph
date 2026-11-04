package machineconfiguration

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
)

// updateCleanSync is the fragment written to clear the clean sync flag
type updateCleanSync struct {
	CleanSync bool `dynamodbav:"CleanSync"`
}

// newCleanSyncRequest returns a new identifier for a clean sync request (see MachineConfiguration.CleanSyncRequest)
func newCleanSyncRequest() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on the platforms Rudolph runs on
		panic(fmt.Sprintf("reading random bytes failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// ClearMachineCleanSync clears the clean sync flag of a machine-specific config once the clean sync it requested is
// confirmed, so the flag makes exactly one clean sync happen. request is the CleanSyncRequest the clean sync performed
// ("" for a flag stored without one). Only a row that still holds that request is changed: a row that was deleted,
// cleared in the meantime, or whose flag was set again since (a new request) is left alone.
func ClearMachineCleanSync(client dynamodb.UpdateItemIfAPI, machineID string, request string) error {
	condition := expression.Equal(expression.Name("CleanSync"), expression.Value(true))
	if request == "" {
		condition = condition.And(expression.AttributeNotExists(expression.Name("CleanSyncRequest")))
	} else {
		condition = condition.And(expression.Equal(expression.Name("CleanSyncRequest"), expression.Value(request)))
	}
	_, err := client.UpdateItemIf(
		dynamodb.PrimaryKey{
			PartitionKey: machineConfigurationPK(machineID),
			SortKey:      machineConfigurationSK(),
		},
		updateCleanSync{CleanSync: false},
		condition,
	)
	if dynamodb.IsConditionalCheckFailed(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("clearing the clean sync flag of machine config %s failed: %w", machineID, err)
	}
	return nil
}
