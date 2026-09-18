package machineconfiguration

import (
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
)

// UpdateMachineConfigClientMode updates a machineID specific ClientMode configuration to the DynamoDB table via an UpdateItem API call
func UpdateMachineConfigClientMode(client dynamodb.UpdateItemAPI, machineID string, newClientMode types.ClientMode) error {
	pk := dynamodb.PrimaryKey{
		PartitionKey: machineConfigurationPK(machineID),
		SortKey:      machineConfigurationSK(),
	}

	clientMode := updateClientMode{
		ClientMode: newClientMode,
	}

	//UpdateItem takes a key (pk/sk), item name --> match this to the column/struct tag, and interface to set
	_, err := client.UpdateItem(pk, clientMode)
	if err != nil {
		return fmt.Errorf("setting machine config failed: %w", err)
	}

	return nil
}

// UpdateGlobalConfigClientMode updates the global ClientMode configuration to the DynamoDB table via an UpdateItem API call
func UpdateGlobalConfigClientMode(client dynamodb.UpdateItemAPI, newClientMode types.ClientMode, opts ...GlobalConfigOption) error {
	err := checkGlobalClientMode(newClientMode, opts)
	if err != nil {
		return err
	}

	pk := dynamodb.PrimaryKey{
		PartitionKey: globalConfigurationPK,
		SortKey:      machineConfigurationSK(),
	}

	clientMode := updateClientMode{
		ClientMode: newClientMode,
	}

	//UpdateItem takes a key (pk/sk), item name --> match this to the column/struct tag, and interface to set
	_, err = client.UpdateItem(pk, clientMode)
	if err != nil {
		return fmt.Errorf("setting global config failed: %w", err)
	}

	return nil
}

// ConfigurationWriter updates existing configuration rows and creates the ones that do not exist yet.
// Creating requires a conditional put, so a client that cannot do one atomically is rejected at compile time.
type ConfigurationWriter interface {
	dynamodb.UpdateItemAPI
	dynamodb.PutItemAPI
	dynamodb.PutItemIfNotExistsAPI
}

// maxConfigWriteAttempts bounds retries when a concurrent write creates or deletes the row being updated
const maxConfigWriteAttempts = 3

// GetConfigurationUpdater returns an updater that does not cache what it writes. The timeProvider is unused.
func GetConfigurationUpdater(client ConfigurationWriter, fetcher ConcreteConfigurationFetcher, timeProvider clock.TimeProvider) ConcreteConfigurationUpdater {
	return newConfigurationUpdater(client, fetcher, nil)
}

// newConfigurationUpdater returns an updater that stores an updated global config in cache, if not nil.
// The fetcher should be uncached, so updates are applied to the current config.
func newConfigurationUpdater(client ConfigurationWriter, fetcher ConcreteConfigurationFetcher, cache Cache) ConcreteConfigurationUpdater {
	return ConcreteConfigurationUpdater{
		global: ConcreteGlobalConfigurationUpdater{
			updater: client,
			cache:   cache,
			fetcher: fetcher,
		},
		machine: ConcreteMachineConfigurationUpdater{
			updater: client,
			fetcher: fetcher,
		},
	}
}

// ConfigurationUpdater is an interface to accept a new configuration and update either a global or machine configuration
type ConfigurationUpdater interface {
	UpdateGlobalConfig(configRequest MachineConfigurationUpdateRequest, opts ...GlobalConfigOption) (MachineConfiguration, error)
	UpdateMachineConfig(machineID string, configRequest MachineConfigurationUpdateRequest) (MachineConfiguration, error)
}

type ConcreteConfigurationUpdater struct {
	global  GlobalConfigurationUpdater
	machine MachineConfigurationUpdater
}

func (f ConcreteConfigurationUpdater) UpdateGlobalConfig(configRequest MachineConfigurationUpdateRequest, opts ...GlobalConfigOption) (*MachineConfiguration, error) {
	return f.global.updateConfig(configRequest, opts...)
}

// MachineConfigs //

func (f ConcreteConfigurationUpdater) UpdateMachineConfig(machineID string, configRequest MachineConfigurationUpdateRequest) (*MachineConfiguration, error) {
	return f.machine.updateConfig(machineID, configRequest)
}

// GlobalConfigurationUpdater //
type GlobalConfigurationUpdater interface {
	updateConfig(configRequest MachineConfigurationUpdateRequest, opts ...GlobalConfigOption) (updatedConfig *MachineConfiguration, err error)
}

type ConcreteGlobalConfigurationUpdater struct {
	updater ConfigurationWriter
	cache   Cache
	fetcher ConcreteConfigurationFetcher
}

func (c ConcreteGlobalConfigurationUpdater) updateConfig(configRequest MachineConfigurationUpdateRequest, opts ...GlobalConfigOption) (updatedConfig *MachineConfiguration, err error) {
	// Requesting a blocking mode needs confirmation, even if the stored global config already has it
	if configRequest.ClientMode != nil {
		err = checkGlobalClientMode(*configRequest.ClientMode, opts)
		if err != nil {
			return
		}
	}

	configRequest, err = normalizeUpdateRequest(configRequest)
	if err != nil {
		return
	}

	for attempt := 1; ; attempt++ {
		currentGlobalConfig, isDefaultConfig, eerr := c.fetcher.getIntendedGlobalConfig()
		if eerr != nil {
			err = eerr
			return
		}

		newGlobalConfig, changes := applyUpdateRequest(currentGlobalConfig, configRequest)
		if touchesUsbMountSettings(configRequest) {
			err = validateRemountUsbMode(newGlobalConfig.RemountUsbMode, newGlobalConfig.BlockUsbMount)
			if err != nil {
				return
			}
		}

		// Without a stored global config there is no row to update yet
		updatedConfig, err = writeConfigUpdate(c.updater, globalConfigurationPK, newGlobalConfig, changes, !isDefaultConfig)
		if dynamodb.IsConditionalCheckFailed(err) && attempt < maxConfigWriteAttempts {
			// The row was created or deleted concurrently; apply the update to its current state
			continue
		}
		if err != nil {
			err = fmt.Errorf("failed to update global configuration: %w", err)
			return
		}
		break
	}

	setCachedGlobalConfig(c.cache, updatedConfig)

	return
}

// MachineConfig //
type MachineConfigurationUpdater interface {
	updateConfig(machineID string, configRequest MachineConfigurationUpdateRequest) (updatedConfig *MachineConfiguration, err error)
}

type ConcreteMachineConfigurationUpdater struct {
	updater ConfigurationWriter
	fetcher ConcreteConfigurationFetcher
}

func (c ConcreteMachineConfigurationUpdater) updateConfig(machineID string, configRequest MachineConfigurationUpdateRequest) (updatedConfig *MachineConfiguration, err error) {
	configRequest, err = normalizeUpdateRequest(configRequest)
	if err != nil {
		return
	}

	for attempt := 1; ; attempt++ {
		currentMachineConfig, eerr := c.fetcher.machine.GetMachineSpecificConfig(machineID)
		if eerr != nil {
			err = fmt.Errorf("failed to get machine config: %w", eerr)
			return
		}

		// The global config is needed for a new machine config and to validate inherited settings; read it once
		var globalConfig *MachineConfiguration
		getGlobalConfig := func() (*MachineConfiguration, error) {
			if globalConfig == nil {
				config, _, eerr := c.fetcher.getIntendedGlobalConfig()
				if eerr != nil {
					return nil, eerr
				}
				globalConfig = &config
			}
			return globalConfig, nil
		}

		exists := currentMachineConfig != nil
		if !exists {
			// The machine uses the global config so far. Its machine-specific config starts out as a copy of the
			// global base settings; optional settings stay unset, so they keep being inherited.
			global, eerr := getGlobalConfig()
			if eerr != nil {
				err = eerr
				return
			}
			base := withoutOptionalSettings(*global)
			currentMachineConfig = &base
		}

		newMachineConfig, changes := applyUpdateRequest(*currentMachineConfig, configRequest)
		if touchesUsbMountSettings(configRequest) {
			blockUsbMount := newMachineConfig.BlockUsbMount
			if blockUsbMount == nil && len(newMachineConfig.RemountUsbMode) > 0 {
				global, eerr := getGlobalConfig()
				if eerr != nil {
					err = eerr
					return
				}
				blockUsbMount = global.BlockUsbMount
			}
			err = validateRemountUsbMode(newMachineConfig.RemountUsbMode, blockUsbMount)
			if err != nil {
				return
			}
		}

		updatedConfig, err = writeConfigUpdate(c.updater, machineConfigurationPK(machineID), newMachineConfig, changes, exists)
		if dynamodb.IsConditionalCheckFailed(err) && attempt < maxConfigWriteAttempts {
			// The row was created or deleted concurrently; apply the update to its current state
			continue
		}
		if err != nil {
			err = fmt.Errorf("updating machine configuration failed: %w", err)
		}
		return
	}
}

// normalizeUpdateRequest validates the requested values and converts them to their stored form. Values already
// stored are not validated, so an unrelated update never fails because of them.
func normalizeUpdateRequest(configRequest MachineConfigurationUpdateRequest) (MachineConfigurationUpdateRequest, error) {
	if configRequest.OverrideFileAccessAction != nil {
		action, err := normalizeOverrideFileAccessAction(*configRequest.OverrideFileAccessAction)
		if err != nil {
			return configRequest, err
		}
		configRequest.OverrideFileAccessAction = &action
	}
	return configRequest, nil
}

func touchesUsbMountSettings(configRequest MachineConfigurationUpdateRequest) bool {
	return configRequest.BlockUsbMount != nil || configRequest.RemountUsbMode != nil
}

// writeConfigUpdate writes newConfig to the config row with the given pk. An existing row gets only the changed
// attributes, and only if it still exists. A row that does not exist yet is created from the complete new config,
// and only if it still does not exist, so concurrent first updates cannot overwrite each other. If the row changed
// existence in the meantime, the error satisfies dynamodb.IsConditionalCheckFailed.
func writeConfigUpdate(client ConfigurationWriter, pk string, newConfig MachineConfiguration, changes map[string]interface{}, exists bool) (updatedConfig *MachineConfiguration, err error) {
	// if no items of an existing row have been changed, return the same configuration. A missing row is always
	// created, even if it equals the copied settings: it pins them, e.g. to exempt a machine from a later global change.
	if exists && len(changes) == 0 {
		updatedConfig = &newConfig
		return
	}

	if !exists {
		row := buildConfigRow(pk, newConfig)
		_, err = client.PutItemIfNotExists(row)
		if err != nil {
			return
		}
		updatedConfig = &row.MachineConfiguration
		return
	}

	// Keep the DataType consistent with the row being written
	changes["DataType"] = buildConfigRow(pk, newConfig).DataType

	output, err := client.UpdateItem(
		dynamodb.PrimaryKey{
			PartitionKey: pk,
			SortKey:      machineConfigurationSK(),
		},
		changes,
	)
	if err != nil {
		return
	}

	err = attributevalue.UnmarshalMap(output.Attributes, &updatedConfig)
	if err != nil {
		err = fmt.Errorf("succeeded UpdateItem but failed to unmarshalMap into MachineConfiguration: %w", err)
		return
	}

	return
}

// applyUpdateRequest returns a copy of current with the requested changes applied, and the changed DynamoDB
// attributes (named as in the MachineConfiguration dynamodbav tags) with their new values.
// Only changed attributes are written, so zero values (e.g. CleanSync=false) are not lost to omitempty.
func applyUpdateRequest(current MachineConfiguration, configRequest MachineConfigurationUpdateRequest) (newConfig MachineConfiguration, changes map[string]interface{}) {
	// the new config is a copy of the current config
	newConfig = current
	changes = make(map[string]interface{})

	if configRequest.ClientMode != nil && *configRequest.ClientMode != current.ClientMode {
		newConfig.ClientMode = *configRequest.ClientMode
		changes["ClientMode"] = newConfig.ClientMode
	}

	if configRequest.AllowedPathRegex != nil && strings.Compare(*configRequest.AllowedPathRegex, current.AllowedPathRegex) != 0 {
		newConfig.AllowedPathRegex = *configRequest.AllowedPathRegex
		changes["AllowedPathRegex"] = newConfig.AllowedPathRegex
	}

	if configRequest.BlockedPathRegex != nil && strings.Compare(*configRequest.BlockedPathRegex, current.BlockedPathRegex) != 0 {
		newConfig.BlockedPathRegex = *configRequest.BlockedPathRegex
		changes["BlockedPathRegex"] = newConfig.BlockedPathRegex
	}

	if configRequest.BatchSize != nil && *configRequest.BatchSize != current.BatchSize {
		if *configRequest.BatchSize > 0 {
			newConfig.BatchSize = *configRequest.BatchSize
			changes["BatchSize"] = newConfig.BatchSize
		}
	}

	if configRequest.EnableBundles != nil && *configRequest.EnableBundles != current.EnableBundles {
		newConfig.EnableBundles = *configRequest.EnableBundles
		changes["EnableBundles"] = newConfig.EnableBundles
	}

	if configRequest.EnableTransitiveRules != nil && *configRequest.EnableTransitiveRules != current.EnabledTransitiveRules {
		newConfig.EnabledTransitiveRules = *configRequest.EnableTransitiveRules
		changes["EnableTransitiveRules"] = newConfig.EnabledTransitiveRules
	}

	if configRequest.FullSyncInterval != nil && *configRequest.FullSyncInterval != current.FullSyncInterval {
		if *configRequest.FullSyncInterval >= 60 {
			newConfig.FullSyncInterval = *configRequest.FullSyncInterval
			changes["FullSyncInterval"] = newConfig.FullSyncInterval
		}
	}

	if configRequest.CleanSync != nil {
		newConfig.CleanSync = *configRequest.CleanSync
		changes["CleanSync"] = newConfig.CleanSync
	}

	if configRequest.UploadLogsURL != nil && *configRequest.UploadLogsURL != current.UploadLogsURL {
		newConfig.UploadLogsURL = *configRequest.UploadLogsURL
		changes["UploadLogsUrl"] = newConfig.UploadLogsURL
	}

	if configRequest.EnableAllEventUpload != nil && !boolPtrEqual(configRequest.EnableAllEventUpload, current.EnableAllEventUpload) {
		newConfig.EnableAllEventUpload = boolPtr(*configRequest.EnableAllEventUpload)
		changes["EnableAllEventUpload"] = *newConfig.EnableAllEventUpload
	}

	if configRequest.DisableUnknownEventUpload != nil && !boolPtrEqual(configRequest.DisableUnknownEventUpload, current.DisableUnknownEventUpload) {
		newConfig.DisableUnknownEventUpload = boolPtr(*configRequest.DisableUnknownEventUpload)
		changes["DisableUnknownEventUpload"] = *newConfig.DisableUnknownEventUpload
	}

	if configRequest.OverrideFileAccessAction != nil && *configRequest.OverrideFileAccessAction != current.OverrideFileAccessAction {
		newConfig.OverrideFileAccessAction = *configRequest.OverrideFileAccessAction
		changes["OverrideFileAccessAction"] = newConfig.OverrideFileAccessAction
	}

	if configRequest.BlockUsbMount != nil && !boolPtrEqual(configRequest.BlockUsbMount, current.BlockUsbMount) {
		newConfig.BlockUsbMount = boolPtr(*configRequest.BlockUsbMount)
		changes["BlockUsbMount"] = *newConfig.BlockUsbMount
	}

	// nil (not configured) and empty (configured without flags) differ
	if configRequest.RemountUsbMode != nil && (current.RemountUsbMode == nil || !stringSlicesEqual(*configRequest.RemountUsbMode, current.RemountUsbMode)) {
		newConfig.RemountUsbMode = append(MountFlags{}, *configRequest.RemountUsbMode...)
		changes["RemountUsbMode"] = newConfig.RemountUsbMode
	}

	if configRequest.EventDetailURL != nil && !stringPtrEqual(configRequest.EventDetailURL, current.EventDetailURL) {
		newConfig.EventDetailURL = stringPtr(*configRequest.EventDetailURL)
		changes["EventDetailURL"] = *newConfig.EventDetailURL
	}

	if configRequest.EventDetailText != nil && !stringPtrEqual(configRequest.EventDetailText, current.EventDetailText) {
		newConfig.EventDetailText = stringPtr(*configRequest.EventDetailText)
		changes["EventDetailText"] = *newConfig.EventDetailText
	}

	return
}

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}

func boolPtrEqual(a, b *bool) bool {
	return a != nil && b != nil && *a == *b
}

func stringPtrEqual(a, b *string) bool {
	return a != nil && b != nil && *a == *b
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
