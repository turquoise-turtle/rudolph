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

func GetClient(firehoseStreamName string, awsregion string) FirehoseClient {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(awsregion))
	if err != nil {
		// The Lambda cannot forward events without credentials or a region; fail at start-up, as the v1 session did.
		panic(fmt.Sprintf("unable to load the AWS configuration for Firehose: %v", err))
	}

	return client{
		firehoseStreamName: firehoseStreamName,
		firehoseService:    firehose.NewFromConfig(cfg),
	}
}
