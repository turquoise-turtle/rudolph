package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type blockingQueryAPI struct{}

func (blockingQueryAPI) Query(ctx context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Query used the package default (5 s) instead of the client's timeout, unlike every other operation.
func TestQuery_UsesTheClientTimeout(t *testing.T) {
	start := time.Now()
	_, err := query("table", blockingQueryAPI{}, &dynamodb.QueryInput{}, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want a deadline error", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("query waited %v; want about the 50 ms timeout", d)
	}
}
