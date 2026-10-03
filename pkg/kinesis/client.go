package kinesis

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	awskinesis "github.com/aws/aws-sdk-go/service/kinesis"
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
	sess := awssession.Must(awssession.NewSession())

	return client{
		kinesisStream:  kinesisStreamName,
		kinesisService: awskinesis.New(sess, aws.NewConfig().WithRegion(awsregion)),
	}
}

type client struct {
	kinesisStream  string
	kinesisService *awskinesis.Kinesis
}
