package flags

import (
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// MachineIDLookupAPI reads what ResolveMachineID needs to find the spelling a machine ID is stored under
type MachineIDLookupAPI interface {
	dynamodb.BatchGetItemAPI
	dynamodb.QueryAPI
}

// ValidateMachineIDFlag refuses a machine ID given on the command line that is not a UUID in its canonical
// 8-4-4-4-12 form. The API refuses any other machine ID, so a rule or configuration stored under one would never
// reach a Mac.
func ValidateMachineIDFlag(machineID string) error {
	if err := types.ValidateMachineID(machineID); err != nil {
		return fmt.Errorf("--machine %q is not a machine UUID (8-4-4-4-12 hexadecimal digits, such as 01234567-89AB-CDEF-0123-456789ABCDEF)", machineID)
	}
	return nil
}

// ResolveMachineID returns the exact spelling under which a machine ID given on the command line is stored.
//
// The server keeps a Mac's machine ID exactly as the Mac sends it and matches it exactly, including case. Santa sends
// the hardware UUID, which is upper case, but its MachineID setting can be any case. So the ID is looked up as typed,
// in upper case and in lower case, and the first spelling found is used:
//
//  1. the one a Mac syncs under (its sensor data row);
//  2. else one with a machine configuration;
//  3. else one with machine rules (written by older CLIs, which stored the ID as typed);
//  4. else upper case, the form Santa sends by default.
//
// Steps 1 and 2 are one BatchGetItem request; step 3 reads at most one key per spelling, and only when no Mac and no
// configuration is known under any of them.
func ResolveMachineID(client MachineIDLookupAPI, machineID string) (string, error) {
	if err := ValidateMachineIDFlag(machineID); err != nil {
		return "", err
	}
	spellings := []string{machineID}
	for _, s := range []string{strings.ToUpper(machineID), strings.ToLower(machineID)} {
		if s != spellings[len(spellings)-1] && s != spellings[0] {
			spellings = append(spellings, s)
		}
	}

	sensorKeys := make([]dynamodb.PrimaryKey, len(spellings))
	configKeys := make([]dynamodb.PrimaryKey, len(spellings))
	for i, s := range spellings {
		pk, sk := sensordata.MachineIDSensorDataPKSK(s)
		sensorKeys[i] = dynamodb.PrimaryKey{PartitionKey: pk, SortKey: sk}
		configKeys[i] = machineconfiguration.MachineConfigurationKey(s)
	}
	items, err := client.BatchGetItems(append(append([]dynamodb.PrimaryKey{}, sensorKeys...), configKeys...), false)
	if err != nil {
		return "", fmt.Errorf("failed to look up machine %s: %w", machineID, err)
	}
	stored := map[dynamodb.PrimaryKey]bool{}
	for _, item := range items {
		pk, _ := item["PK"].(*awstypes.AttributeValueMemberS)
		sk, _ := item["SK"].(*awstypes.AttributeValueMemberS)
		if pk != nil && sk != nil {
			stored[dynamodb.PrimaryKey{PartitionKey: pk.Value, SortKey: sk.Value}] = true
		}
	}

	var synced []string
	for i, s := range spellings {
		if stored[sensorKeys[i]] {
			synced = append(synced, s)
		}
	}
	if len(synced) > 1 {
		fmt.Printf("[WARNING] Macs have synced as %s; using %s\n", strings.Join(synced, " and "), synced[0])
	}
	if len(synced) > 0 {
		return synced[0], nil
	}
	for i, s := range spellings {
		if stored[configKeys[i]] {
			return s, nil
		}
	}
	for _, s := range spellings {
		found, err := machinerules.HasMachineRules(client, s)
		if err != nil {
			return "", err
		}
		if found {
			return s, nil
		}
	}
	return strings.ToUpper(machineID), nil
}

// ResolveMachineID replaces the machine ID given with --machine, if any, by the spelling it is stored under (see
// ResolveMachineID). Commands call it once they have a DynamoDB client, before GetMachineID.
func (t *TargetFlags) ResolveMachineID(client MachineIDLookupAPI) error {
	if t.IsGlobal || t.MachineID == "" {
		return nil
	}
	machineID, err := ResolveMachineID(client, t.MachineID)
	if err != nil {
		return err
	}
	t.MachineID = machineID
	return nil
}
