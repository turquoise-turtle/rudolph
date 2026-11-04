package postflight

import (
	"log"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
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

// syncStateUpdater closes out a sync: it marks the sync as completed and records what the client reported about
// the sync it just finished, in a single update of the sync state row. It reads nothing: what the completed sync
// delivered (feed position, machine-specific rules) is already on the row and is taken over by the next preflight.
// When the sync was a clean sync requested by the machine config's clean sync flag, it also clears the flag.
type syncStateUpdater interface {
	updatePostflight(machineID string, request *PostflightRequest) error
}

type concreteSyncStateUpdater struct {
	updater dynamodb.UpdateItemAPI
	// configUpdater clears the clean sync flag of the machine config; nil leaves it to the next preflight
	configUpdater dynamodb.UpdateItemIfAPI
	timeProvider  clock.TimeProvider
}

func (c concreteSyncStateUpdater) updatePostflight(machineID string, request *PostflightRequest) (err error) {
	var closed *syncstate.ClosedSync
	if request == nil {
		// A client that sent no postflight body reported no outcome.
		closed, err = syncstate.UpdatePostflightDate(c.timeProvider, c.updater, machineID)
	} else {
		closed, err = syncstate.UpdatePostflightOutcome(
			c.timeProvider,
			c.updater,
			machineID,
			string(request.SyncType),
			request.RulesReceived,
			request.RulesProcessed,
			request.RulesHash, // bounded by parseRequest
		)
	}
	if err != nil || !closed.ConfigCleanSync || c.configUpdater == nil {
		return err
	}
	// The clean sync the machine config asked for completed. If clearing the flag fails, the next preflight sees the
	// completed clean sync and clears it.
	if cerr := machineconfiguration.ClearMachineCleanSync(c.configUpdater, machineID, closed.ConfigCleanSyncRequest); cerr != nil {
		log.Printf("%v", cerr)
	}
	return nil
}

// updatePostflightDate closes out a sync the client reported nothing about: only the timestamps are updated (the
// feed cursor it writes is superseded, see syncstate.UpdatePostflightItem).
func (c concreteSyncStateUpdater) updatePostflightDate(machineID string) error {
	_, err := syncstate.UpdatePostflightDate(c.timeProvider, c.updater, machineID)
	return err
}
