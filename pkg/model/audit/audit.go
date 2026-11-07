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
// the table, or ListPage, which does the same one page at a time with an opaque cursor. RecordContext, ListContext and
// ListPageContext do the same with a client of the AWS SDK, for programs such as rudolph-web that do not use
// pkg/dynamodb.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	// maxPageLimit caps ListPage's limit: a page is for people to read, and it keeps limit+1 from overflowing.
	maxPageLimit = 1000
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

// ErrInvalidCursor is returned by ListPage for a cursor it did not make
var ErrInvalidCursor = errors.New("invalid audit cursor")

// List returns at most limit entries recorded at or after since, newest first. It queries one partition per UTC day,
// from tomorrow (so that entries from a writer whose clock runs ahead past midnight are not missed) back to the day
// of since.
func List(client Reader, since time.Time, limit int) ([]Entry, error) {
	rows, err := list(client.Query, since, limit, position{})
	return entries(rows), err
}

// ListContext is List with a client of the AWS SDK and the table's name
func ListContext(ctx context.Context, client SDKReader, table string, since time.Time, limit int) ([]Entry, error) {
	rows, err := list(sdkQuery(ctx, client, table), since, limit, position{})
	return entries(rows), err
}

// ListPage is List one page at a time. cursor is "" for the first page, then the next returned by the previous page;
// next is "" once there are no more entries. Paging with the same since returns every entry once, newest first, across
// day partitions. A limit above 1000 is treated as 1000. The cursor is opaque but not signed: it names the last entry
// returned, and ListPage refuses one it could not have made (malformed, or dated after tomorrow) with ErrInvalidCursor.
func ListPage(client Reader, since time.Time, limit int, cursor string) (page []Entry, next string, err error) {
	return listPage(client.Query, since, limit, cursor)
}

// ListPageContext is ListPage with a client of the AWS SDK and the table's name
func ListPageContext(ctx context.Context, client SDKReader, table string, since time.Time, limit int, cursor string) (page []Entry, next string, err error) {
	return listPage(sdkQuery(ctx, client, table), since, limit, cursor)
}

type queryFunc func(*awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error)

func sdkQuery(ctx context.Context, client SDKReader, table string) queryFunc {
	return func(in *awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error) {
		in.TableName = aws.String(table)
		return client.Query(ctx, in)
	}
}

// position is the key of the last entry a page returned: the next page starts after it. The zero position starts at
// the newest entry.
type position struct {
	day time.Time
	sk  string
}

func (p position) isZero() bool { return p.sk == "" }

// sortKeyFormat is what sortKey makes: 19 digits of Unix nanoseconds, '#', 12 hex digits
var sortKeyFormat = regexp.MustCompile(`^[0-9]{19}#[0-9a-f]{12}$`)

func encodeCursor(p position) string {
	return base64.RawURLEncoding.EncodeToString([]byte(p.day.Format(dayLayout) + "|" + p.sk))
}

func decodeCursor(cursor string) (position, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil {
		return position{}, ErrInvalidCursor
	}
	dayText, sk, ok := strings.Cut(string(raw), "|")
	if !ok || !sortKeyFormat.MatchString(sk) {
		return position{}, ErrInvalidCursor
	}
	day, err := time.Parse(dayLayout, dayText)
	if err != nil || day.Format(dayLayout) != dayText {
		return position{}, ErrInvalidCursor
	}
	nanos, err := strconv.ParseInt(sk[:19], 10, 64)
	if err != nil || partitionKey(time.Unix(0, nanos)) != partitionKey(day) {
		// An entry is stored in the partition of the day of its own time.
		return position{}, ErrInvalidCursor
	}
	if day.After(time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)) {
		// No page starts after tomorrow; a forged cursor dated years ahead would make list query every day back.
		return position{}, ErrInvalidCursor
	}
	return position{day: day, sk: sk}, nil
}

func listPage(query queryFunc, since time.Time, limit int, cursor string) ([]Entry, string, error) {
	var after position
	if cursor != "" {
		var err error
		if after, err = decodeCursor(cursor); err != nil {
			return nil, "", err
		}
	}
	if limit <= 0 {
		return nil, "", nil
	}
	limit = min(limit, maxPageLimit)
	// One entry more than the page tells whether there is another page, so the last page has no next.
	rows, err := list(query, since, limit+1, after)
	if err != nil {
		return nil, "", err
	}
	if len(rows) <= limit {
		return entries(rows), "", nil
	}
	rows = rows[:limit]
	last, err := positionOf(rows[limit-1])
	if err != nil {
		return nil, "", err
	}
	return entries(rows), encodeCursor(last), nil
}

// positionOf returns the position of a row from its sort key, which is what decodeCursor checks the day against
func positionOf(row item) (position, error) {
	if !sortKeyFormat.MatchString(row.SK) {
		return position{}, fmt.Errorf("audit entry %s %s has a malformed sort key", row.PK, row.SK)
	}
	nanos, err := strconv.ParseInt(row.SK[:19], 10, 64)
	if err != nil {
		return position{}, fmt.Errorf("audit entry %s %s has a malformed sort key: %w", row.PK, row.SK, err)
	}
	return position{day: time.Unix(0, nanos).UTC().Truncate(24 * time.Hour), sk: row.SK}, nil
}

func entries(rows []item) []Entry {
	if rows == nil {
		return nil
	}
	out := make([]Entry, len(rows))
	for i, row := range rows {
		out[i] = row.Entry
	}
	return out
}

// list returns at most limit rows at or after since, newest first, starting after the position after
func list(query queryFunc, since time.Time, limit int, after position) ([]item, error) {
	if limit <= 0 {
		return nil, nil
	}
	since = since.UTC()
	sinceKey := fmt.Sprintf("%019d", since.UnixNano())
	firstDay := since.Truncate(24 * time.Hour)
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if !after.isZero() {
		day = after.day.UTC().Truncate(24 * time.Hour)
	}
	var rows []item
	for ; !day.Before(firstDay) && len(rows) < limit; day = day.Add(-24 * time.Hour) {
		var start map[string]types.AttributeValue
		if !after.isZero() && day.Equal(after.day.UTC().Truncate(24*time.Hour)) {
			if after.sk < sinceKey {
				// The cursor is from a page with an earlier since: nothing in this day is both after it and at or
				// after since (and DynamoDB refuses a start key outside the key condition).
				continue
			}
			start = map[string]types.AttributeValue{
				"PK": &types.AttributeValueMemberS{Value: partitionKey(day)},
				"SK": &types.AttributeValueMemberS{Value: after.sk},
			}
		}
		dayRows, err := listDay(query, partitionKey(day), sinceKey, limit-len(rows), start)
		if err != nil {
			return nil, err
		}
		rows = append(rows, dayRows...)
	}
	return rows, nil
}

func listDay(query queryFunc, pk string, sinceKey string, limit int, start map[string]types.AttributeValue) ([]item, error) {
	var rows []item
	for {
		out, err := query(&awsdynamodb.QueryInput{
			KeyConditionExpression: aws.String("PK = :pk AND SK >= :since"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":    &types.AttributeValueMemberS{Value: pk},
				":since": &types.AttributeValueMemberS{Value: sinceKey},
			},
			ScanIndexForward:  aws.Bool(false),
			Limit:             aws.Int32(int32(min(limit-len(rows), maxPage))),
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
			if row.Time, err = time.Parse(time.RFC3339Nano, row.RecordedAt); err != nil {
				return nil, fmt.Errorf("audit entry %s %s has no valid time: %w", row.PK, row.SK, err)
			}
			rows = append(rows, row)
		}
		start = out.LastEvaluatedKey
		if len(start) == 0 || len(rows) >= limit {
			return rows, nil
		}
	}
}
