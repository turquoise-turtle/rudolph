package firehose

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/firehose"
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
	firehoseService    *firehose.Firehose
}

func GetClient(firehoseStreamName string, awsregion string) FirehoseClient {
	sess := session.Must(session.NewSession())

	return client{
		firehoseStreamName: firehoseStreamName,
		firehoseService:    firehose.New(sess, aws.NewConfig().WithRegion(awsregion)),
	}
}
