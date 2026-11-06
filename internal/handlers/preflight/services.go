package preflight

import (
	"log"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
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
	saveReportedGroups(machineID string, groups []string, prevSyncState *syncstate.SyncStateRow) (reportedGroupsRecord, error)
	saveSyncState(machineID string, requestCleanSync bool, lastCleanSync string, batchSize int, feedSyncCursor string, rulesHash string, prevSyncState *syncstate.SyncStateRow, configCleanSync *syncstate.ConfigCleanSyncProgress, reportedGroups reportedGroupsRecord, policies clientPolicies) error
	getFeedSyncStateCursor(syncState *syncstate.SyncStateRow) (string, bool)
}

type concreteStateTrackingService struct {
	getter       dynamodb.GetItemAPI
	putter       dynamodb.PutItemAPI
	timeProvider clock.TimeProvider
	// groupsUpdater writes the machine's reported group membership, judged against groupsCache's registry; when
	// either is nil, reported membership is not recorded
	groupsUpdater dynamodb.UpdateExpressionAPI
	groupsCache   *groups.Cache
}

func getStateTrackingService(dbClient dynamodb.DynamoDBClient, timeProvider clock.TimeProvider) stateTrackingService {
	service := concreteStateTrackingService{
		getter:       dbClient,
		putter:       dbClient,
		timeProvider: timeProvider,
	}
	if updater, ok := dbClient.(dynamodb.UpdateExpressionAPI); ok {
		service.groupsUpdater = updater
		service.groupsCache = groups.NewCache(timeProvider)
	}
	return service
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
	sensorData.HostnameKey = sensordata.HostnameKey(request.Hostname)
	sensorData.ModelIdentifier = request.ModelIdentifier
	sensorData.SIPStatus = request.SIPStatus
	// Every string stored here was bounded by PreflightRequest.validate.
	sensorData.RulesHash = request.RulesHash

	_, err := c.putter.PutItem(sensorData)
	return err
}

// reportedGroupsRecord is what the sync state records about the reported groups item (see saveReportedGroups).
type reportedGroupsRecord struct {
	hash      string
	writtenAt string
	// groupsBasis is groups.ReportedBasis of the membership stored on the machine's groups item
	groupsBasis string
}

// saveReportedGroups writes the primary user groups the Mac reported to their own item when they differ from what the
// previous sync recorded, or when the item is due a refresh before it expires. The sync state carries a digest of the
// list, so an unchanged list costs no read and no write. If this sync's sync state is never saved, the next preflight
// compares with the older record and at worst writes the same list again.
func (c concreteStateTrackingService) saveReportedGroups(machineID string, reported []string, prevSyncState *syncstate.SyncStateRow) (reportedGroupsRecord, error) {
	record, err := c.saveReportedGroupsItem(machineID, reported, prevSyncState)
	if err != nil {
		return record, err
	}
	record.groupsBasis = c.saveReportedMembership(machineID, reported, prevSyncState)
	return record, nil
}

// saveReportedMembership stores on the machine's groups item which registered groups the Mac's reported list makes it
// a member of, when that differs from what the previous sync recorded, and returns the basis to record. The registry
// comes from a cache, so a group created or changed in the meantime is taken into account within groups.CacheDuration.
// A failure is not fatal: the previous membership stays in force and the next preflight tries again.
func (c concreteStateTrackingService) saveReportedMembership(machineID string, reported []string, prevSyncState *syncstate.SyncStateRow) string {
	prevBasis := ""
	if prevSyncState != nil {
		prevBasis = prevSyncState.GroupsBasis
	}
	if c.groupsUpdater == nil || c.groupsCache == nil {
		return prevBasis
	}
	registry, err := c.groupsCache.Registry(c.getter)
	if err != nil {
		log.Printf("  Reported groups not checked: %v", err)
		return prevBasis
	}
	membership := groups.ReportedMembership(registry, reported)
	if _, dropped := groups.NormaliseList(reported); dropped > 0 {
		// Never log the names: they can identify the user
		log.Printf("  Ignored %d reported groups whose names Rudolph does not accept", dropped)
	}
	basis := groups.ReportedBasis(membership)
	if basis == prevBasis {
		return prevBasis
	}
	if err := groups.SetReported(c.timeProvider, c.groupsUpdater, machineID, membership); err != nil {
		log.Printf("  %v", err)
		return prevBasis
	}
	return basis
}

// saveReportedGroupsItem writes the reported groups item (see saveReportedGroups)
func (c concreteStateTrackingService) saveReportedGroupsItem(machineID string, groups []string, prevSyncState *syncstate.SyncStateRow) (reportedGroupsRecord, error) {
	now := c.timeProvider.Now()
	hash := sensordata.ReportedGroupsHash(groups)
	prevHash := ""
	if prevSyncState != nil {
		prevHash = prevSyncState.ReportedGroupsHash
	}
	if hash == prevHash {
		if hash == "" {
			return reportedGroupsRecord{}, nil
		}
		writtenAt, err := clock.ParseRFC3339(prevSyncState.ReportedGroupsWrittenAt)
		if err == nil && now.Sub(writtenAt) < sensordata.ReportedGroupsRefreshAfter {
			return reportedGroupsRecord{hash: hash, writtenAt: prevSyncState.ReportedGroupsWrittenAt}, nil
		}
	}
	if _, err := c.putter.PutItem(sensordata.NewReportedGroups(c.timeProvider, machineID, groups)); err != nil {
		return reportedGroupsRecord{}, err
	}
	if hash == "" {
		// The item now says the Mac reports no groups; nothing needs refreshing
		return reportedGroupsRecord{}, nil
	}
	return reportedGroupsRecord{hash: hash, writtenAt: clock.RFC3339(now)}, nil
}

func (c concreteStateTrackingService) getSyncState(machineID string) (syncState *syncstate.SyncStateRow, err error) {
	return syncstate.GetByMachineID(c.getter, machineID)
}

// clientPolicies is what the sync state records about the policies the Mac's Santa understands (see
// syncstate.SyncState.SilentBlocklistOnly and HoldsDowngradedPolicies).
type clientPolicies struct {
	silentBlocklistOnly     bool
	holdsDowngradedPolicies bool
}

// configCleanSync is not nil when this sync is a clean sync performing the clean sync flag of the machine config.
func (c concreteStateTrackingService) saveSyncState(machineID string, requestCleanSync bool, lastCleanSync string, batchSize int, feedSyncCursor string, rulesHash string, prevSyncState *syncstate.SyncStateRow, configCleanSync *syncstate.ConfigCleanSyncProgress, reportedGroups reportedGroupsRecord, policies clientPolicies) error {
	syncState := syncstate.CreateNewSyncState(
		c.timeProvider,
		machineID,
		requestCleanSync,
		lastCleanSync,
		batchSize,
		feedSyncCursor,
	)
	// Bounded by PreflightRequest.validate; the next preflight compares it with the hash it receives
	syncState.PreflightRulesHash = rulesHash
	syncState.ContinueFrom(prevSyncState, machinerules.DeliveryDigest(nil), c.timeProvider.Now())
	syncState.ReportedGroupsHash = reportedGroups.hash
	syncState.ReportedGroupsWrittenAt = reportedGroups.writtenAt
	syncState.GroupsBasis = reportedGroups.groupsBasis
	syncState.SilentBlocklistOnly = policies.silentBlocklistOnly
	syncState.HoldsDowngradedPolicies = policies.holdsDowngradedPolicies
	if configCleanSync != nil && requestCleanSync {
		syncState.SetConfigCleanSync(*configCleanSync)
	}
	_, err := c.putter.PutItem(syncState)
	return err
}
