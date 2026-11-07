// Package audit is Rudolph's audit trail: who changed rules, configuration, groups or machines, when, and how.
//
// Every write the CLI makes records an entry (actor "cli:<OS user>@<host name>"), and rudolph-web records its own
// with the actor "web:<Entra user>". Entries are kept in the store table:
//
//	PK: Audit#<YYYY-MM-DD>                      one partition per UTC day
//	SK: <unix nanoseconds, 19 digits>#<random>  sorts by time; the suffix keeps entries of the same instant apart
//
// Entries never expire (they carry no ExpiresAfter), are written with a conditional put so one is never overwritten,
// and are read with List, which queries day partitions from tomorrow (for writers whose clock runs ahead) back and never scans
// the table. RecordContext and ListContext do the same with a client of the AWS SDK, for programs such as rudolph-web
// that do not use pkg/dynamodb.
package audit

import (
	"context"
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

// Writer is what Record writes with: Rudolph's DynamoDB client (pkg/dynamodb), or an adapter of another client.
// PutItemIfNotExists must not overwrite an existing item: it writes only under the condition attribute_not_exists(PK)
// (an entry's key is unique, so the condition only guards against a broken random source or a replayed write). A
// client of the AWS SDK can use RecordContext instead, which builds that request itself.
type Writer interface {
	dynamodb.PutItemIfNotExistsAPI
}

// Reader is what List reads with: Rudolph's DynamoDB client, which sets the table name on the query. A client of the
// AWS SDK can use ListContext instead.
type Reader interface {
	dynamodb.QueryAPI
}

// SDKWriter is the PutItem of the AWS SDK's DynamoDB client (*dynamodb.Client)
type SDKWriter interface {
	PutItem(ctx context.Context, in *awsdynamodb.PutItemInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.PutItemOutput, error)
}

// SDKReader is the Query of the AWS SDK's DynamoDB client (*dynamodb.Client)
type SDKReader interface {
	Query(ctx context.Context, in *awsdynamodb.QueryInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.QueryOutput, error)
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

// newItem checks the entry and returns the item that stores it at the clock's current time
func newItem(timeProvider clock.TimeProvider, entry Entry) (*item, error) {
	switch {
	case entry.Actor == "":
		return nil, errors.New("an audit entry needs an actor")
	case entry.Action == "":
		return nil, errors.New("an audit entry needs an action")
	case entry.Target == "":
		return nil, errors.New("an audit entry needs a target")
	}
	entry.Time = timeProvider.Now().UTC()
	entry.ExpiresAfter = 0
	sk, err := sortKey(entry.Time)
	if err != nil {
		return nil, fmt.Errorf("failed to make the audit entry's key: %w", err)
	}
	return &item{
		PK:         partitionKey(entry.Time),
		SK:         sk,
		DataType:   dataType,
		RecordedAt: entry.Time.Format(time.RFC3339Nano),
		Entry:      entry,
	}, nil
}

// Record adds an entry to the audit trail at the clock's current time. It refuses an entry without an actor, action
// or target; the entry's Time and ExpiresAfter are ignored.
func Record(timeProvider clock.TimeProvider, client Writer, entry Entry) error {
	row, err := newItem(timeProvider, entry)
	if err != nil {
		return err
	}
	if _, err := client.PutItemIfNotExists(row); err != nil {
		return fmt.Errorf("failed to record the audit entry: %w", err)
	}
	return nil
}

// PutItemInput returns the conditional PutItem request that records the entry in the table at the clock's current
// time, for callers that send requests themselves. It refuses an entry as Record does.
func PutItemInput(timeProvider clock.TimeProvider, table string, entry Entry) (*awsdynamodb.PutItemInput, error) {
	row, err := newItem(timeProvider, entry)
	if err != nil {
		return nil, err
	}
	av, err := attributevalue.MarshalMap(row)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal the audit entry: %w", err)
	}
	return &awsdynamodb.PutItemInput{
		TableName:           aws.String(table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	}, nil
}

// RecordContext is Record with a client of the AWS SDK and the table's name
func RecordContext(ctx context.Context, timeProvider clock.TimeProvider, client SDKWriter, table string, entry Entry) error {
	in, err := PutItemInput(timeProvider, table, entry)
	if err != nil {
		return err
	}
	if _, err := client.PutItem(ctx, in); err != nil {
		return fmt.Errorf("failed to record the audit entry: %w", err)
	}
	return nil
}

// List returns at most limit entries recorded at or after since, newest first. It queries one partition per UTC day,
// from tomorrow (so that entries from a writer whose clock runs ahead past midnight are not missed) back to the day
// of since.
func List(client Reader, since time.Time, limit int) ([]Entry, error) {
	return list(client.Query, since, limit)
}

// ListContext is List with a client of the AWS SDK and the table's name
func ListContext(ctx context.Context, client SDKReader, table string, since time.Time, limit int) ([]Entry, error) {
	return list(func(in *awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error) {
		in.TableName = aws.String(table)
		return client.Query(ctx, in)
	}, since, limit)
}

type queryFunc func(*awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error)

func list(query queryFunc, since time.Time, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	since = since.UTC()
	var entries []Entry
	firstDay := since.Truncate(24 * time.Hour)
	for day := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour); !day.Before(firstDay) && len(entries) < limit; day = day.Add(-24 * time.Hour) {
		dayEntries, err := listDay(query, partitionKey(day), since, limit-len(entries))
		if err != nil {
			return nil, err
		}
		entries = append(entries, dayEntries...)
	}
	return entries, nil
}

func listDay(query queryFunc, pk string, since time.Time, limit int) ([]Entry, error) {
	var entries []Entry
	var start map[string]types.AttributeValue
	for {
		out, err := query(&awsdynamodb.QueryInput{
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
