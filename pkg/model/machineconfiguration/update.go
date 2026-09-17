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

// ConfigurationWriter updates existing configuration rows and creates the ones that do not exist yet
type ConfigurationWriter interface {
	dynamodb.UpdateItemAPI
	dynamodb.PutItemAPI
}

func GetConfigurationUpdater(client ConfigurationWriter, fetcher ConcreteConfigurationFetcher, timeProvider clock.TimeProvider) ConcreteConfigurationUpdater {
	return ConcreteConfigurationUpdater{
		global: ConcreteGlobalConfigurationUpdater{
			updater: client,
			cache:   GetCache(timeProvider),
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
	// Requesting LOCKDOWN needs confirmation, even if the stored global config is already LOCKDOWN
	if configRequest.ClientMode != nil {
		err = checkGlobalClientMode(*configRequest.ClientMode, opts)
		if err != nil {
			return
		}
	}

	currentGlobalConfig, isDefaultConfig, err := c.fetcher.getIntendedGlobalConfig()
	if err != nil {
		return
	}

	// Without a stored global config there is no row to update yet
	updatedConfig, err = writeConfigUpdate(c.updater, globalConfigurationPK, currentGlobalConfig, !isDefaultConfig, configRequest)
	if err != nil {
		err = fmt.Errorf("failed to update global configuration: %w", err)
		return
	}

	// Attempt to set the new global configuration into cache
	_ = c.cache.Set(CacheKeyGlobal, updatedConfig)

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
	currentMachineConfig, err := c.fetcher.machine.GetMachineSpecificConfig(machineID)
	if err != nil {
		err = fmt.Errorf("failed to get machine config: %w", err)
		return
	}

	exists := currentMachineConfig != nil
	if !exists {
		// The machine uses the global config so far; its machine-specific config starts out as a copy of it
		globalConfig, _, eerr := c.fetcher.getIntendedGlobalConfig()
		if eerr != nil {
			err = eerr
			return
		}
		currentMachineConfig = &globalConfig
	}

	updatedConfig, err = writeConfigUpdate(c.updater, machineConfigurationPK(machineID), *currentMachineConfig, exists, configRequest)
	if err != nil {
		err = fmt.Errorf("updating machine configuration failed: %w", err)
	}

	return
}

// writeConfigUpdate applies configRequest to the current config and writes the result to the config row with the given pk.
// An existing row gets only the changed attributes; a row that does not exist yet is created from the complete new config.
func writeConfigUpdate(client ConfigurationWriter, pk string, current MachineConfiguration, exists bool, configRequest MachineConfigurationUpdateRequest) (updatedConfig *MachineConfiguration, err error) {
	newConfig, changes := applyUpdateRequest(current, configRequest)

	err = validateConfig(newConfig)
	if err != nil {
		return
	}

	// if no items have been changed, return the same configuration
	if len(changes) == 0 {
		updatedConfig = &newConfig
		return
	}

	if !exists {
		row := buildConfigRow(pk, newConfig)
		_, err = client.PutItem(row)
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

	if configRequest.RemountUsbMode != nil && !stringSlicesEqual(*configRequest.RemountUsbMode, current.RemountUsbMode) {
		newConfig.RemountUsbMode = append([]string{}, *configRequest.RemountUsbMode...)
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
