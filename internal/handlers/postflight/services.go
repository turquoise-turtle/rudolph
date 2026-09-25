package postflight

import (
	"log"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
)

func archiveSyncState(client dynamodb.DynamoDBClient, machineID string) (err error) {
	syncState, err := syncstate.GetByMachineID(client, machineID)
	if err != nil {
		return
	}

	err = syncstate.Archive(client, *syncState)
	if err != nil {
		log.Printf("Failed to archive syncState")
		return
	}

	return
}

// syncStateUpdater closes out a sync: it records the postflight timestamps and what the client reported about
// the sync it just finished, in a single update of the sync state row.
type syncStateUpdater interface {
	updatePostflight(machineID string, request *PostflightRequest) error
}

type concreteSyncStateUpdater struct {
	updater      dynamodb.UpdateItemAPI
	timeProvider clock.TimeProvider
}

func (c concreteSyncStateUpdater) updatePostflight(machineID string, request *PostflightRequest) (err error) {
	if request == nil {
		// A client that sent no postflight body reported no outcome.
		return c.updatePostflightDate(machineID)
	}
	return syncstate.UpdatePostflightOutcome(
		c.timeProvider,
		c.updater,
		machineID,
		string(request.SyncType),
		request.RulesReceived,
		request.RulesProcessed,
		request.RulesHash, // bounded by parseRequest
	)
}

// updatePostflightDate closes out a sync the client reported nothing about: only the timestamps and the feed
// cursor are updated.
func (c concreteSyncStateUpdater) updatePostflightDate(machineID string) (err error) {
	return syncstate.UpdatePostflightDate(c.timeProvider, c.updater, machineID)
}

type staleRuleDestroyer interface {
	destroyMachineRulesMarkedForDeletion(machineID string) error
}

type concreteRuleDestroyer struct {
	queryer dynamodb.QueryAPI
	deleter dynamodb.DeleteItemAPI
}

func (c concreteRuleDestroyer) destroyMachineRulesMarkedForDeletion(machineID string) (err error) {
	log.Printf("Now evicting stale machine rules")

	keysToDelete, err := machinerules.GetPrimaryKeysByMachineIDWhereMarkedForDeletion(c.queryer, machineID)
	if err != nil {
		return
	}

	log.Printf("Found %d stale machine rules to delete", len(*keysToDelete))

	for _, keyToDelete := range *keysToDelete {
		_, err = c.deleter.DeleteItem(keyToDelete)
		if err != nil {
			return
		}
	}

	return
}
