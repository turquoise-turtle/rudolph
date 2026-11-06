package sensordata

import (
	"fmt"
	"strings"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
)

const (
	sensorDataPKPrefix  = "Machine#"
	sensorDataDefaultSK = "Current"

	sensorDataExpiresAfterInDays int = 90

	lastSeenDayLayout = "2006-01-02"
)

// SensorData encapsulation for a DDB row encapsulating data that is uploaded in preflight
type SensorData struct {
	dynamodb.PrimaryKey
	MachineID string `dynamodbav:"MachineID,omitempty"`
	SerialNum string `dynamodbav:"SerialNum,omitempty"`
	// SerialKey is the key of the web view's serial index, omitted when empty for the same reason as HostnameKey.
	SerialKey string `dynamodbav:"SerialKey,omitempty"`
	// Hostname and HostnameKey are omitted when empty: HostnameKey is the key of the web view's hostname index,
	// and DynamoDB rejects a PutItem that stores an empty string in an index key attribute.
	Hostname        string `dynamodbav:"Hostname,omitempty"`
	HostnameKey     string `dynamodbav:"HostnameKey,omitempty"`
	ModelIdentifier string `dynamodbav:"ModelIdentifier"`
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
	// LastSeenDay is the UTC date of the last preflight. Unlike Time it changes at most once a day, so the web
	// view's machine list indexes can project it without an index write on every sync.
	LastSeenDay  string         `dynamodbav:"LastSeenDay,omitempty"`
	ExpiresAfter int64          `dynamodbav:"ExpiresAfter,omitempty"`
	DataType     types.DataType `dynamodbav:"DataType,omitempty"`
}

// HostnameKey is the form of a hostname the web view searches by prefix: trimmed and lower case, so the search is
// case-insensitive and a blank hostname has no key.
func HostnameKey(hostname string) string {
	return strings.ToLower(strings.TrimSpace(hostname))
}

// SerialKey is the form of a serial number the web view searches by prefix: trimmed and upper case. Apple serials
// are upper case, but some virtual machines report mixed case, which an upper-case prefix search would never match
// against the serial as reported. A blank serial has no key.
func SerialKey(serialNumber string) string {
	return strings.ToUpper(strings.TrimSpace(serialNumber))
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

// LastSeenDay returns the UTC calendar date of t in the form stored in SensorData.LastSeenDay.
func LastSeenDay(t time.Time) string {
	return t.UTC().Format(lastSeenDayLayout)
}

func GetDataType() types.DataType {
	return types.DataTypeSensorData
}
