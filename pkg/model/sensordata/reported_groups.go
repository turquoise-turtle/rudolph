package sensordata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
)

const (
	reportedGroupsSK = "ReportedGroups"

	// The item is only written when the reported list changes, so it is rewritten this often anyway to keep it from
	// expiring (sensorDataExpiresAfterInDays) while the Mac keeps reporting the same groups.
	ReportedGroupsRefreshAfter = 30 * 24 * time.Hour
)

// ReportedGroups holds the primary user groups a Mac reports in preflight. They are kept out of the sensor data row,
// which every preflight rewrites: a directory-bound Mac can report hundreds of groups, and DynamoDB bills a write by
// the size of the whole item. Preflight writes this item only when the list changed (see ReportedGroupsHash).
type ReportedGroups struct {
	dynamodb.PrimaryKey
	PrimaryUserGroups []string `dynamodbav:"PrimaryUserGroups"`
	Time              string   `dynamodbav:"Time"`
	ExpiresAfter      int64    `dynamodbav:"ExpiresAfter"`
}

// NewReportedGroups returns the item recording groups as reported by machineID's preflight.
func NewReportedGroups(timeProvider clock.TimeProvider, machineID string, groups []string) ReportedGroups {
	if groups == nil {
		// Stored as an empty list rather than left out: the item then says the Mac reported no groups
		groups = []string{}
	}
	return ReportedGroups{
		PrimaryKey: dynamodb.PrimaryKey{
			PartitionKey: SensorDataPK(machineID),
			SortKey:      reportedGroupsSK,
		},
		PrimaryUserGroups: groups,
		Time:              clock.RFC3339(timeProvider.Now()),
		ExpiresAfter:      GetSensorDataExpiresAfter(timeProvider),
	}
}

// ReportedGroupsPKSK returns the keys of machineID's reported groups item.
func ReportedGroupsPKSK(machineID string) (string, string) {
	return SensorDataPK(machineID), reportedGroupsSK
}

// ReportedGroupsHash is a short digest of a reported list of groups, kept on the sync state so that preflight can
// tell whether the list changed without reading the item. It is empty for an empty list, the value a sync state has
// when it never recorded one, so a Mac that never reports groups never writes the item. Order counts: a reordered
// list is rewritten, which is harmless.
func ReportedGroupsHash(groups []string) string {
	if len(groups) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(groups)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}
