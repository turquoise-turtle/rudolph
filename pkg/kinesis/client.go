package kinesis

import (
	"context"

	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	awskinesis "github.com/aws/aws-sdk-go-v2/service/kinesis"
)

// KinesisEventUploadEvent is a single event entry that appends the MachineID with the EventUploadEvent details
type KinesisEvents struct {
	Items []interface{}
}

type KinesisClient interface {
	Send(machineID string, events KinesisEvents) (err error)
}

// ContextClient is a KinesisClient whose sends can be bounded by a context's deadline. GetClient returns one.
type ContextClient interface {
	KinesisClient
	SendContext(ctx context.Context, machineID string, events KinesisEvents) error
}

// NewClient returns a client of the stream, built from the default AWS configuration (so AWS_ENDPOINT_URL_* and the
// usual credential sources apply), or an error if that configuration cannot be loaded.
func NewClient(streamName string, awsregion string) (ContextClient, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(awsregion))
	if err != nil {
		return nil, fmt.Errorf("unable to load the AWS configuration for Kinesis: %w", err)
	}
	return client{
		kinesisStream:  streamName,
		kinesisService: awskinesis.NewFromConfig(cfg),
	}, nil
}

// GetClient is NewClient for callers that cannot handle the error: it panics if the configuration cannot be loaded.
// Lambdas use NewClient so that Boot reports the error.
func GetClient(streamName string, awsregion string) KinesisClient {
	c, err := NewClient(streamName, awsregion)
	if err != nil {
		panic(err)
	}
	return c
}

type client struct {
	kinesisStream  string
	kinesisService *awskinesis.Client
}
