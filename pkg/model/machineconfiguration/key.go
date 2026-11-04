package machineconfiguration

import "github.com/airbnb/rudolph/pkg/dynamodb"

// MachineConfigurationKey is the primary key of a machine's own configuration row
func MachineConfigurationKey(machineID string) dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{
		PartitionKey: machineConfigurationPK(machineID),
		SortKey:      machineConfigurationSK(),
	}
}
