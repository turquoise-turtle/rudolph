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

func GetClient(kinesisStreamName string, awsregion string) KinesisClient {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(awsregion))
	if err != nil {
		// The Lambda cannot forward events without credentials or a region; fail at start-up, as the v1 session did.
		panic(fmt.Sprintf("unable to load the AWS configuration for Kinesis: %v", err))
	}

	return client{
		kinesisStream:  kinesisStreamName,
		kinesisService: awskinesis.NewFromConfig(cfg),
	}
}

type client struct {
	kinesisStream  string
	kinesisService *awskinesis.Client
}
