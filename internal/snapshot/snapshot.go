// Package snapshot holds what Rudolph's scheduled jobs (internal/ranker, internal/fleet) share when they publish a
// snapshot to the events table: rows are written first, under a partition named after a snapshot ID that sorts by
// time, and only then is a pointer moved to that ID, on the condition that it does not already name a newer one. A
// reader therefore never sees a half-written snapshot, a failed run leaves the previous one in place, and of two
// overlapping runs the one that started later wins.
package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// writeAttempts bounds the BatchWriteItem calls for one batch whose items DynamoDB keeps returning unprocessed
// (throttling); the run then fails and the pointer stays on the previous snapshot.
const writeAttempts = 8

// MaxBatch is the most rows one BatchWriteItem takes.
const MaxBatch = 25

// Writer is the part of *dynamodb.Client that writes a snapshot.
type Writer interface {
	BatchWriteItem(ctx context.Context, in *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

// ID returns a snapshot ID that sorts by time and is unique even for runs in the same instant.
func ID(now time.Time) (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(suffix), nil
}

// WriteBatch writes at most MaxBatch rows, sending again what DynamoDB returns unprocessed.
func WriteBatch(ctx context.Context, client Writer, table string, requests []types.WriteRequest) error {
	pending := map[string][]types.WriteRequest{table: requests}
	for attempt := 1; ; attempt++ {
		out, err := client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
		if err != nil {
			return fmt.Errorf("snapshot rows: %w", err)
		}
		if len(out.UnprocessedItems[table]) == 0 {
			return nil
		}
		if attempt >= writeAttempts {
			return fmt.Errorf("%d snapshot rows still unprocessed after %d attempts", len(out.UnprocessedItems[table]), attempt)
		}
		pending = out.UnprocessedItems
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(50<<min(attempt, 6)) * time.Millisecond):
		}
	}
}

// MovePointer writes the pointer item, which must hold the snapshot's ID as its Snapshot attribute, unless the stored
// pointer already names a newer snapshot; moved is then false. Runs can overlap (EventBridge Scheduler may invoke
// twice, an operator may invoke the Lambda by hand); one that started earlier must not move the pointer back.
func MovePointer(ctx context.Context, client Writer, table string, item map[string]types.AttributeValue, id string) (moved bool, err error) {
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 aws.String(table),
		Item:                      item,
		ConditionExpression:       aws.String("attribute_not_exists(PK) OR #snapshot < :snapshot"),
		ExpressionAttributeNames:  map[string]string{"#snapshot": "Snapshot"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":snapshot": &types.AttributeValueMemberS{Value: id}},
	})
	var failed *types.ConditionalCheckFailedException
	if errors.As(err, &failed) {
		return false, nil
	}
	return err == nil, err
}

// Parallel runs fn for 0..n-1 with at most limit calls at once and returns every error. After the first error no new
// calls start.
func Parallel(ctx context.Context, limit, n int, fn func(ctx context.Context, i int) error) error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	work := make(chan int)
	for w := 0; w < min(limit, n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := fn(ctx, i); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		mu.Lock()
		failed := len(errs) > 0
		mu.Unlock()
		if failed || ctx.Err() != nil {
			break
		}
		work <- i
	}
	close(work)
	wg.Wait()
	if len(errs) == 0 && ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(errs...)
}
