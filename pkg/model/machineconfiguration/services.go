package machineconfiguration

import (
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
)

//
// This service exposes all machineconfiguration access methods
//
type MachineConfigurationService interface {
	GetIntendedConfig(machineID string) (intendedConfig MachineConfiguration, err error)
	GetIntendedGlobalConfig() (intendedConfig MachineConfiguration, isDefaultConfig bool, err error)
	SetGlobalConfig(config MachineConfiguration, opts ...GlobalConfigOption) error
	SetMachineConfig(machineID string, config MachineConfiguration) error
	UpdateGlobalConfig(configRequest MachineConfigurationUpdateRequest, opts ...GlobalConfigOption) (*MachineConfiguration, error)
	UpdateMachineConfig(machineID string, configRequest MachineConfigurationUpdateRequest) (*MachineConfiguration, error)
	DeleteGlobalConfig() error
	DeleteMachineConfig(machineID string) error
	// ClearMachineCleanSync clears the clean sync flag of the machine-specific config once the clean sync it
	// requested is confirmed, if the flag still holds that request (see the ClearMachineCleanSync function)
	ClearMachineCleanSync(machineID string, request string) error
}

type ConcreteMachineConfigurationService struct {
	dynamodb dynamodb.DynamoDBClient
	fetcher  ConcreteConfigurationFetcher
	setter   ConcreteConfigurationSetter
	updater  ConcreteConfigurationUpdater
	deleter  ConcreteConfigurationDeleter
}

// GetMachineConfigurationService caches the global config for reads. Writes through the service refresh that
// cache. Updates always read the current config uncached, so they never write back a stale copy.
func GetMachineConfigurationService(dynamodb dynamodb.DynamoDBClient, timeProvider clock.TimeProvider) MachineConfigurationService {
	cache := GetCache(timeProvider)
	return ConcreteMachineConfigurationService{
		dynamodb: dynamodb,
		fetcher:  newCachedConfigurationFetcher(dynamodb, cache),
		setter:   newConfigurationSetter(dynamodb, cache),
		updater:  newConfigurationUpdater(dynamodb, GetUncachedConfigurationFetcher(dynamodb, timeProvider), cache),
		deleter:  GetConfigurationDeleter(dynamodb),
	}
}

func GetUncachedMachineConfigurationService(dynamodb dynamodb.DynamoDBClient, timeProvider clock.TimeProvider) MachineConfigurationService {
	return ConcreteMachineConfigurationService{
		dynamodb: dynamodb,
		fetcher:  GetUncachedConfigurationFetcher(dynamodb, timeProvider),
		setter:   newConfigurationSetter(dynamodb, nil),
		updater:  newConfigurationUpdater(dynamodb, GetUncachedConfigurationFetcher(dynamodb, timeProvider), nil),
		deleter:  GetConfigurationDeleter(dynamodb),
	}
}

// Getter //

// Machine
func (s ConcreteMachineConfigurationService) GetIntendedConfig(machineID string) (intendedConfig MachineConfiguration, err error) {
	return s.fetcher.getIntendedConfig(machineID)
}

// Global
func (s ConcreteMachineConfigurationService) GetIntendedGlobalConfig() (intendedConfig MachineConfiguration, isDefaultConfig bool, err error) {
	return s.fetcher.getIntendedGlobalConfig()
}

// Setter //

// Global
// Setting a global client mode other than MONITOR requires the ConfirmGlobalLockdown() option.
func (s ConcreteMachineConfigurationService) SetGlobalConfig(config MachineConfiguration, opts ...GlobalConfigOption) error {
	return s.setter.global.setGlobalConfig(config, opts...)
}

// Machine
func (s ConcreteMachineConfigurationService) SetMachineConfig(machineID string, config MachineConfiguration) error {
	return s.setter.machine.setMachineConfig(machineID, config)
}

// Updater //

// Global
// Setting a global client mode other than MONITOR requires the ConfirmGlobalLockdown() option.
func (s ConcreteMachineConfigurationService) UpdateGlobalConfig(configRequest MachineConfigurationUpdateRequest, opts ...GlobalConfigOption) (*MachineConfiguration, error) {
	return s.updater.global.updateConfig(configRequest, opts...)
}

// Machine

func (s ConcreteMachineConfigurationService) UpdateMachineConfig(machineID string, configRequest MachineConfigurationUpdateRequest) (*MachineConfiguration, error) {
	return s.updater.machine.updateConfig(machineID, configRequest)
}

// Deleter //

// Global
func (s ConcreteMachineConfigurationService) DeleteGlobalConfig() error {
	return s.deleter.global.deleteGlobalConfig()
}

// Machine
func (s ConcreteMachineConfigurationService) DeleteMachineConfig(machineID string) error {
	return s.deleter.machine.deleteMachineConfig(machineID)
}

// ClearMachineCleanSync clears the clean sync flag of the machine-specific config, if it still holds request
func (s ConcreteMachineConfigurationService) ClearMachineCleanSync(machineID string, request string) error {
	return ClearMachineCleanSync(s.dynamodb, machineID, request)
}
