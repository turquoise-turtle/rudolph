package firehose

import (
	"context"

	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
)

type FirehoseClient interface {
	Send(machineID string, events FirehoseEvents) (err error)
}

// ContextClient is a FirehoseClient whose sends can be bounded by a context's deadline. GetClient returns one.
type ContextClient interface {
	FirehoseClient
	SendContext(ctx context.Context, machineID string, events FirehoseEvents) error
}

type client struct {
	firehoseStreamName string
	firehoseService    *firehose.Client
}

// NewClient returns a client of the stream, built from the default AWS configuration (so AWS_ENDPOINT_URL_* and the
// usual credential sources apply), or an error if that configuration cannot be loaded.
func NewClient(streamName string, awsregion string) (ContextClient, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(awsregion))
	if err != nil {
		return nil, fmt.Errorf("unable to load the AWS configuration for Firehose: %w", err)
	}
	return client{
		firehoseStreamName: streamName,
		firehoseService:    firehose.NewFromConfig(cfg),
	}, nil
}

// GetClient is NewClient for callers that cannot handle the error: it panics if the configuration cannot be loaded.
// Lambdas use NewClient so that Boot reports the error.
func GetClient(streamName string, awsregion string) FirehoseClient {
	c, err := NewClient(streamName, awsregion)
	if err != nil {
		panic(err)
	}
	return c
}
