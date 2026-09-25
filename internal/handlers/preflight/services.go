package preflight

import (
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
)

type cleanSyncService interface {
	determineCleanSync(machineID string, preflightRequest *PreflightRequest, syncState *syncstate.SyncStateRow) (bool, error)
}

type concreteCleanSyncService struct {
	timeProvider clock.TimeProvider
}

func getCleanSyncService(timeProvider clock.TimeProvider) cleanSyncService {
	return concreteCleanSyncService{
		timeProvider: timeProvider,
	}
}

type stateTrackingService interface {
	saveSensorDataFromPreflightRequest(machineID string, request *PreflightRequest) error
	getSyncState(machineID string) (syncState *syncstate.SyncStateRow, err error)
	saveSyncState(machineID string, requestCleanSync bool, lastCleanSync string, batchSize int, feedSyncCursor string) error
	getFeedSyncStateCursor(syncState *syncstate.SyncStateRow) (string, bool)
}

type concreteStateTrackingService struct {
	getter       dynamodb.GetItemAPI
	putter       dynamodb.PutItemAPI
	timeProvider clock.TimeProvider
}

func getStateTrackingService(dbClient dynamodb.DynamoDBClient, timeProvider clock.TimeProvider) stateTrackingService {
	return concreteStateTrackingService{
		getter:       dbClient,
		putter:       dbClient,
		timeProvider: timeProvider,
	}
}

func (c concreteStateTrackingService) saveSensorDataFromPreflightRequest(machineID string, request *PreflightRequest) error {
	sensorData := sensordata.NewSensorData(
		c.timeProvider,
		machineID,
		request.SerialNumber,
		request.OSVersion,
		request.OSBuild,
		request.SantaVersion,
		request.ClientMode,
		request.RequestCleanSync,
		request.PrimaryUser,
		request.CertificateRuleCount,
		request.BinaryRuleCount,
		request.CDHashRuleCount,
		request.TeamIDRuleCount,
		request.SigningIDRuleCount,
		request.CompilerRuleCount,
		request.TransitiveRuleCount,
	)
	// The rest of the inventory a sensor reports about its host; the web view and the CLI show it, and it is
	// the only place an operator can see which Mac a machine ID belongs to.
	sensorData.Hostname = request.Hostname
	sensorData.ModelIdentifier = request.ModelIdentifier
	sensorData.SIPStatus = request.SIPStatus
	sensorData.PrimaryUserGroups = request.PrimaryUserGroups
	// Every string stored here was bounded by PreflightRequest.validate.
	sensorData.RulesHash = request.RulesHash

	_, err := c.putter.PutItem(sensorData)
	return err
}

func (c concreteStateTrackingService) getSyncState(machineID string) (syncState *syncstate.SyncStateRow, err error) {
	return syncstate.GetByMachineID(c.getter, machineID)
}

func (c concreteStateTrackingService) saveSyncState(machineID string, requestCleanSync bool, lastCleanSync string, batchSize int, feedSyncCursor string) error {
	syncState := syncstate.CreateNewSyncState(
		c.timeProvider,
		machineID,
		requestCleanSync,
		lastCleanSync,
		batchSize,
		feedSyncCursor,
	)
	_, err := c.putter.PutItem(syncState)
	return err
}
