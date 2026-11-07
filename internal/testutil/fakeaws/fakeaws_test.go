package fakeaws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	firehosetypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	kinesistypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"
)

// The clients are built exactly like pkg/firehose and pkg/kinesis build theirs: default configuration, no endpoint
// override (Use sets AWS_ENDPOINT_URL_<SERVICE>).
func sdkConfig(t *testing.T, retries int) aws.Config {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion("us-east-1"), config.WithRetryMaxAttempts(retries))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func TestFirehosePutRecordBatchThroughTheSDK(t *testing.T) {
	s := Start(t)
	s.Use(t)
	s.FirehoseRecordFault = func(data []byte, attempt int) string {
		if string(data) == "bad" && attempt == 1 {
			return "ServiceUnavailableException"
		}
		return ""
	}
	ctx := context.Background()
	svc := firehose.NewFromConfig(sdkConfig(t, 3))
	out, err := svc.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{
		DeliveryStreamName: aws.String("stream"),
		Records:            []firehosetypes.Record{{Data: []byte("ok")}, {Data: []byte("bad")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt32(out.FailedPutCount) != 1 || out.RequestResponses[0].RecordId == nil || aws.ToString(out.RequestResponses[1].ErrorCode) != "ServiceUnavailableException" {
		t.Fatalf("unexpected response %+v", out)
	}
	if recs := s.FirehoseRecords(); len(recs) != 1 || string(recs[0].Data) != "ok" || recs[0].Stream != "stream" {
		t.Fatalf("accepted records = %v", recs)
	}

	// Limits: 500 records per request.
	var tooMany []firehosetypes.Record
	for i := 0; i < FirehoseMaxRecordsPerRequest+1; i++ {
		tooMany = append(tooMany, firehosetypes.Record{Data: []byte("x")})
	}
	_, err = svc.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{DeliveryStreamName: aws.String("stream"), Records: tooMany})
	if apiCode(err) != "InvalidArgumentException" {
		t.Fatalf("501 records must be rejected, got %v", err)
	}
	// A record above 1000 KiB.
	_, err = svc.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{DeliveryStreamName: aws.String("stream"), Records: []firehosetypes.Record{{Data: make([]byte, FirehoseMaxBytesPerRecord+1)}}})
	if err == nil {
		t.Fatal("an oversized record must be rejected")
	}
	if len(s.Violations()) != 2 {
		t.Fatalf("violations = %v", s.Violations())
	}

	s.FirehoseRequestFault = func(call int) *Fault { return &InternalFailure }
	svc = firehose.NewFromConfig(sdkConfig(t, 1))
	if _, err := svc.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{DeliveryStreamName: aws.String("stream"), Records: []firehosetypes.Record{{Data: []byte("x")}}}); err == nil {
		t.Fatal("a request fault must reach the caller")
	}
}

func TestKinesisPutRecordsThroughTheSDK(t *testing.T) {
	s := Start(t)
	s.Use(t)
	s.KinesisRecordFault = func(data []byte, attempt int) string {
		if string(data) == "bad" {
			return "ProvisionedThroughputExceededException"
		}
		return ""
	}
	ctx := context.Background()
	svc := kinesis.NewFromConfig(sdkConfig(t, 3))
	out, err := svc.PutRecords(ctx, &kinesis.PutRecordsInput{
		StreamName: aws.String("stream"),
		Records: []kinesistypes.PutRecordsRequestEntry{
			{Data: []byte("ok"), PartitionKey: aws.String("m")},
			{Data: []byte("bad"), PartitionKey: aws.String("m")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt32(out.FailedRecordCount) != 1 || aws.ToString(out.Records[1].ErrorCode) != "ProvisionedThroughputExceededException" {
		t.Fatalf("unexpected response %+v", out)
	}
	if recs := s.KinesisRecords(); len(recs) != 1 || recs[0].PartitionKey != "m" {
		t.Fatalf("accepted records = %v", recs)
	}

	big := make([]byte, 600*1024)
	var records []kinesistypes.PutRecordsRequestEntry
	for i := 0; i < 9; i++ { // 5.4 MB
		records = append(records, kinesistypes.PutRecordsRequestEntry{Data: big, PartitionKey: aws.String("m")})
	}
	_, err = svc.PutRecords(ctx, &kinesis.PutRecordsInput{StreamName: aws.String("stream"), Records: records})
	if apiCode(err) != "ValidationException" {
		t.Fatalf("a request above 5 MiB must be rejected, got %v", err)
	}
}

func TestLambdaInvokeThroughTheSDK(t *testing.T) {
	s := Start(t)
	s.Use(t)
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	svc := lambda.NewFromConfig(cfg)
	_, err = svc.Invoke(context.Background(), &lambda.InvokeInput{
		FunctionName: aws.String("sink"), Qualifier: aws.String("$LATEST"),
		InvocationType: lambdatypes.InvocationTypeEvent, Payload: []byte(`{"items":[1]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	inv := s.Invocations()
	if len(inv) != 1 || inv[0].FunctionName != "sink" || inv[0].Qualifier != "$LATEST" || inv[0].InvocationType != "Event" {
		t.Fatalf("invocations = %+v", inv)
	}

	_, err = svc.Invoke(context.Background(), &lambda.InvokeInput{
		FunctionName: aws.String("sink"), InvocationType: lambdatypes.InvocationTypeEvent,
		Payload: []byte(`"` + strings.Repeat("x", LambdaMaxAsyncPayloadBytes) + `"`),
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "RequestEntityTooLargeException" {
		t.Fatalf("an async payload above 256 KB must be rejected, got %v", err)
	}
	if len(s.Violations()) != 1 {
		t.Fatalf("violations = %v", s.Violations())
	}
}

func TestOnlyAWSEndpointsAreRedirected(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("other")) }))
	defer other.Close()
	s := Start(t)
	s.Use(t)
	resp, err := http.Get(other.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(s.Requests()) != 0 {
		t.Fatalf("a non-AWS request reached the fake: %v", s.Requests())
	}
}
