package eventupload

import (
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/firehose"
)

func sendToFirehose(firehoseClient firehose.FirehoseClient, machineID string, forwardedEvents []interface{}) error {
	err := firehoseClient.Send(machineID, firehose.FirehoseEvents{Items: forwardedEvents})
	if err != nil {
		log.Printf("upload to firehose was not successful: %s", err.Error())
		return fmt.Errorf("failed to events to AWS Firehose: %w", err)
	}

	return nil
}
