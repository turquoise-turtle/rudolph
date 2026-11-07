// Package audit is Rudolph's audit trail: who changed rules, configuration, groups or machines, when, and how.
//
// Every write the CLI makes records an entry (actor "cli:<OS user>@<host name>"), and rudolph-web records its own
// with the actor "web:<Entra user>". Entries are kept in the store table:
//
//	PK: Audit#<YYYY-MM-DD>                      one partition per UTC day
//	SK: <unix nanoseconds, 19 digits>#<random>  sorts by time; the suffix keeps entries of the same instant apart
//
// Entries never expire (they carry no ExpiresAfter), are written with a conditional put so one is never overwritten,
// and are read with List, which queries day partitions from the newest back and never scans the table.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	partitionPrefix = "Audit#"
	dataType        = "Audit"
	dayLayout       = "2006-01-02"
	maxPage         = 1000
)

// Entry is one change recorded in the audit trail
type Entry struct {
	Time time.Time `dynamodbav:"-"`
	// Actor is who made the change: "cli:<OS user>@<host name>" or "web:<Entra user>"
	Actor string `dynamodbav:"Actor"`
	// Action is what was done, e.g. "rule.add" or "config.temporary-mode"
	Action string `dynamodbav:"Action"`
	// Target is what it was done to: "global", "machine:<machine ID>", "group:<name>" or "ungrouped"
	Target string `dynamodbav:"Target"`
	// Summary describes the change for people
	Summary string `dynamodbav:"Summary"`
	// ExpiresAfter is always 0: audit entries are kept. It is here so that readers can check that.
	ExpiresAfter int64 `dynamodbav:"ExpiresAfter,omitempty"`
}

type item struct {
	PK       string `dynamodbav:"PK"`
	SK       string `dynamodbav:"SK"`
	DataType string `dynamodbav:"DataType"`
	// RecordedAt is the entry's time as RFC 3339 with nanoseconds, for people reading the table directly
	RecordedAt string `dynamodbav:"RecordedAt"`
	Entry
}

// Writer is what recording an entry needs
type Writer interface {
	dynamodb.PutItemIfNotExistsAPI
}

// Reader is what listing entries needs
type Reader interface {
	dynamodb.QueryAPI
}

// partitionKey returns the partition of the UTC day of t
func partitionKey(t time.Time) string {
	return partitionPrefix + t.UTC().Format(dayLayout)
}

// sortKey orders entries by time: zero-padded so that string order is time order (for times after 1970)
func sortKey(t time.Time) (string, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return fmt.Sprintf("%019d#%s", t.UnixNano(), hex.EncodeToString(suffix)), nil
}

// Record adds an entry to the audit trail at the clock's current time. The entry's Time and ExpiresAfter are ignored.
func Record(timeProvider clock.TimeProvider, client Writer, entry Entry) error {
	switch {
	case entry.Actor == "":
		return errors.New("an audit entry needs an actor")
	case entry.Action == "":
		return errors.New("an audit entry needs an action")
	case entry.Target == "":
		return errors.New("an audit entry needs a target")
	}
	entry.Time = timeProvider.Now().UTC()
	entry.ExpiresAfter = 0
	sk, err := sortKey(entry.Time)
	if err != nil {
		return fmt.Errorf("failed to make the audit entry's key: %w", err)
	}
	row := item{
		PK:         partitionKey(entry.Time),
		SK:         sk,
		DataType:   dataType,
		RecordedAt: entry.Time.Format(time.RFC3339Nano),
		Entry:      entry,
	}
	if _, err := client.PutItemIfNotExists(row); err != nil {
		return fmt.Errorf("failed to record the audit entry: %w", err)
	}
	return nil
}

// List returns at most limit entries recorded at or after since, newest first. It queries one partition per UTC day,
// from today back to the day of since.
func List(client Reader, since time.Time, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	since = since.UTC()
	var entries []Entry
	firstDay := since.Truncate(24 * time.Hour)
	for day := time.Now().UTC().Truncate(24 * time.Hour); !day.Before(firstDay) && len(entries) < limit; day = day.Add(-24 * time.Hour) {
		dayEntries, err := listDay(client, partitionKey(day), since, limit-len(entries))
		if err != nil {
			return nil, err
		}
		entries = append(entries, dayEntries...)
	}
	return entries, nil
}

func listDay(client Reader, pk string, since time.Time, limit int) ([]Entry, error) {
	var entries []Entry
	var start map[string]types.AttributeValue
	for {
		out, err := client.Query(&awsdynamodb.QueryInput{
			KeyConditionExpression: aws.String("PK = :pk AND SK >= :since"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":    &types.AttributeValueMemberS{Value: pk},
				":since": &types.AttributeValueMemberS{Value: fmt.Sprintf("%019d", since.UnixNano())},
			},
			ScanIndexForward:  aws.Bool(false),
			Limit:             aws.Int32(int32(min(limit-len(entries), maxPage))),
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to read the audit trail: %w", err)
		}
		for _, raw := range out.Items {
			var row item
			if err := attributevalue.UnmarshalMap(raw, &row); err != nil {
				return nil, fmt.Errorf("failed to read an audit entry: %w", err)
			}
			entry := row.Entry
			if entry.Time, err = time.Parse(time.RFC3339Nano, row.RecordedAt); err != nil {
				return nil, fmt.Errorf("audit entry %s %s has no valid time: %w", row.PK, row.SK, err)
			}
			entries = append(entries, entry)
		}
		start = out.LastEvaluatedKey
		if len(start) == 0 || len(entries) >= limit {
			return entries, nil
		}
	}
}
