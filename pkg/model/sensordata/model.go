package sensordata

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
)

const (
	sensorDataPKPrefix  = "Machine#"
	sensorDataDefaultSK = "Current"

	sensorDataExpiresAfterInDays int = 90
)

// SensorData encapsulation for a DDB row encapsulating data that is uploaded in preflight
type SensorData struct {
	dynamodb.PrimaryKey
	MachineID            string           `dynamodbav:"MachineID"`
	SerialNum            string           `dynamodbav:"SerialNum"`
	Hostname             string           `dynamodbav:"Hostname"`
	ModelIdentifier      string           `dynamodbav:"ModelIdentifier"`
	// SIPStatus is a pointer so that "SIP disabled" (0) is distinguishable from "the sensor reported none":
	// omitempty leaves out only a nil pointer, a reported 0 is stored.
	SIPStatus            *uint32          `dynamodbav:"SIPStatus,omitempty"`
	OSVersion            string           `dynamodbav:"OSVersion"`
	OSBuild              string           `dynamodbav:"OSBuild"`
	SantaVersion         string           `dynamodbav:"SantaVersion"`
	ClientMode           types.ClientMode `dynamodbav:"ClientMode"`
	RequestCleanSync     bool             `dynamodbav:"RequestCleanSync"`
	PrimaryUser          string           `dynamodbav:"PrimaryUser"`
	PrimaryUserGroups    []string         `dynamodbav:"PrimaryUserGroups,omitempty"`
	RulesHash            string           `dynamodbav:"RulesHash,omitempty"`
	RuleCount            int              `dynamodbav:"RuleCount"`
	CertificateRuleCount int              `dynamodbav:"CertificateRuleCount"`
	BinaryRuleCount      int              `dynamodbav:"BinaryRuleCount"`
	CDHashRuleCount      int              `dynamodbav:"CDHashRuleCount"`
	TeamIDRuleCount      int              `dynamodbav:"TeamIDRuleCount"`
	SigningIDRuleCount   int              `dynamodbav:"SigningIDRuleCount"`
	CompilerRuleCount    int              `dynamodbav:"CompilerRuleCount"`
	TransitiveRuleCount  int              `dynamodbav:"TransitiveRuleCount"`
	Time                 string           `dynamodbav:"Time"`
	ExpiresAfter         int64            `dynamodbav:"ExpiresAfter,omitempty"`
	DataType             types.DataType   `dynamodbav:"DataType"`
}

// MachineIDSensorDataPKSK returns the partition and sort keys for a machine id
func MachineIDSensorDataPKSK(machineID string) (string, string) {
	return SensorDataPK(machineID), sensorDataDefaultSK
}

func SensorDataPK(machineID string) string {
	return fmt.Sprintf("%s%s", sensorDataPKPrefix, machineID)
}

func GetSensorDataExpiresAfter(timeProvider clock.TimeProvider) int64 {
	return clock.Unixtimestamp(timeProvider.Now().UTC().AddDate(0, 0, sensorDataExpiresAfterInDays))
}

func GetDataType() types.DataType {
	return types.DataTypeSensorData
}
