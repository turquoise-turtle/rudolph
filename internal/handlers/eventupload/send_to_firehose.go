package eventupload

import (
	"context"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/firehose"
)

func sendToFirehose(ctx context.Context, firehoseClient firehose.FirehoseClient, machineID string, forwardedEvents []interface{}) error {
	var err error
	events := firehose.FirehoseEvents{Items: forwardedEvents}
	if c, ok := firehoseClient.(firehose.ContextClient); ok {
		err = c.SendContext(ctx, machineID, events)
	} else {
		err = firehoseClient.Send(machineID, events)
	}
	if err = dropUnstorable(machineID, err); err != nil {
		log.Printf("upload to firehose was not successful: %s", err.Error())
		return fmt.Errorf("failed to events to AWS Firehose: %w", err)
	}

	return nil
}
