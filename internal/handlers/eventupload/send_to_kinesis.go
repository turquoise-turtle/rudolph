package eventupload

import (
	"context"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/kinesis"
)

func sendToKinesis(ctx context.Context, kinesisClient kinesis.KinesisClient, machineID string, forwardedEvents []interface{}) error {
	var err error
	events := kinesis.KinesisEvents{Items: forwardedEvents}
	if c, ok := kinesisClient.(kinesis.ContextClient); ok {
		err = c.SendContext(ctx, machineID, events)
	} else {
		err = kinesisClient.Send(machineID, events)
	}
	if err = dropUnstorable(machineID, err); err != nil {
		log.Printf("Kinesis Failed: %s", err)
		return fmt.Errorf("failed to events to AWS kinesis: %w", err)
	}

	return nil
}
