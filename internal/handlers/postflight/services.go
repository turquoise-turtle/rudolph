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

type syncStateUpdater interface {
	updatePostflightDate(machineID string) error
}

// syncOutcomeUpdater is the richer form of syncStateUpdater: besides closing out the sync it also stores
// what the client reported about it. A syncStateUpdater that implements it is preferred, so that the
// outcome and the postflight timestamps are written to the sync state row in a single update.
type syncOutcomeUpdater interface {
	syncStateUpdater
	updatePostflightOutcome(machineID string, request *PostflightRequest) error
}

type concreteSyncStateUpdater struct {
	updater      dynamodb.UpdateItemAPI
	timeProvider clock.TimeProvider
}

func (c concreteSyncStateUpdater) updatePostflightDate(machineID string) (err error) {
	return syncstate.UpdatePostflightDate(c.timeProvider, c.updater, machineID)
}

func (c concreteSyncStateUpdater) updatePostflightOutcome(machineID string, request *PostflightRequest) (err error) {
	if request == nil {
		// A client that sent no postflight body reported no outcome.
		return c.updatePostflightDate(machineID)
	}
	return syncstate.UpdatePostflightOutcome(
		c.timeProvider,
		c.updater,
		machineID,
		request.SyncType,
		request.RulesReceived,
		request.RulesProcessed,
	)
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
