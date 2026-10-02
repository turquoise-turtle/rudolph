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
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	awsv1 "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/firehose"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/aws/smithy-go"
)

// The SDK v1 clients are built exactly like pkg/firehose and pkg/kinesis build theirs: no endpoint configuration.
func v1Session(t *testing.T) *session.Session {
	t.Helper()
	return session.Must(session.NewSession())
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
	svc := firehose.New(v1Session(t), awsv1.NewConfig().WithRegion("us-east-1"))
	out, err := svc.PutRecordBatch(&firehose.PutRecordBatchInput{
		DeliveryStreamName: awsv1.String("stream"),
		Records:            []*firehose.Record{{Data: []byte("ok")}, {Data: []byte("bad")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if *out.FailedPutCount != 1 || out.RequestResponses[0].RecordId == nil || *out.RequestResponses[1].ErrorCode != "ServiceUnavailableException" {
		t.Fatalf("unexpected response %v", out)
	}
	if recs := s.FirehoseRecords(); len(recs) != 1 || string(recs[0].Data) != "ok" || recs[0].Stream != "stream" {
		t.Fatalf("accepted records = %v", recs)
	}

	// Limits: 500 records per request.
	var tooMany []*firehose.Record
	for i := 0; i < FirehoseMaxRecordsPerRequest+1; i++ {
		tooMany = append(tooMany, &firehose.Record{Data: []byte("x")})
	}
	_, err = svc.PutRecordBatch(&firehose.PutRecordBatchInput{DeliveryStreamName: awsv1.String("stream"), Records: tooMany})
	var aerr awserr.Error
	if !errors.As(err, &aerr) || aerr.Code() != "InvalidArgumentException" {
		t.Fatalf("501 records must be rejected, got %v", err)
	}
	// A record above 1000 KiB.
	_, err = svc.PutRecordBatch(&firehose.PutRecordBatchInput{DeliveryStreamName: awsv1.String("stream"), Records: []*firehose.Record{{Data: make([]byte, FirehoseMaxBytesPerRecord+1)}}})
	if err == nil {
		t.Fatal("an oversized record must be rejected")
	}
	if len(s.Violations()) != 2 {
		t.Fatalf("violations = %v", s.Violations())
	}

	s.FirehoseRequestFault = func(call int) *Fault { return &InternalFailure }
	svc = firehose.New(v1Session(t), awsv1.NewConfig().WithRegion("us-east-1").WithMaxRetries(0))
	if _, err := svc.PutRecordBatch(&firehose.PutRecordBatchInput{DeliveryStreamName: awsv1.String("stream"), Records: []*firehose.Record{{Data: []byte("x")}}}); err == nil {
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
	svc := kinesis.New(v1Session(t), awsv1.NewConfig().WithRegion("us-east-1"))
	out, err := svc.PutRecords(&kinesis.PutRecordsInput{
		StreamName: awsv1.String("stream"),
		Records: []*kinesis.PutRecordsRequestEntry{
			{Data: []byte("ok"), PartitionKey: awsv1.String("m")},
			{Data: []byte("bad"), PartitionKey: awsv1.String("m")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if *out.FailedRecordCount != 1 || *out.Records[1].ErrorCode != "ProvisionedThroughputExceededException" {
		t.Fatalf("unexpected response %v", out)
	}
	if recs := s.KinesisRecords(); len(recs) != 1 || recs[0].PartitionKey != "m" {
		t.Fatalf("accepted records = %v", recs)
	}

	big := make([]byte, 600*1024)
	var records []*kinesis.PutRecordsRequestEntry
	for i := 0; i < 9; i++ { // 5.4 MB
		records = append(records, &kinesis.PutRecordsRequestEntry{Data: big, PartitionKey: awsv1.String("m")})
	}
	_, err = svc.PutRecords(&kinesis.PutRecordsInput{StreamName: awsv1.String("stream"), Records: records})
	var aerr awserr.Error
	if !errors.As(err, &aerr) || aerr.Code() != "ValidationException" {
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
