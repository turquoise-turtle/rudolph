package machineconfiguration

import (
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func GetConfigurationFetcher(client dynamodb.GetItemAPI, timeProvider clock.TimeProvider) ConcreteConfigurationFetcher {
	return newCachedConfigurationFetcher(client, GetCache(timeProvider))
}

// newCachedConfigurationFetcher returns a fetcher that caches the global config in cache
func newCachedConfigurationFetcher(client dynamodb.GetItemAPI, cache Cache) ConcreteConfigurationFetcher {
	return ConcreteConfigurationFetcher{
		global: CachedConcreteGlobalConfigurationFetcher{
			getter: client,
			cache:  cache,
		},
		machine: ConcreteMachineConfigurationFetcher{
			getter: client,
		},
		universal: ConcreteUniversalConfigurationProvider{},
	}
}

func GetUncachedConfigurationFetcher(client dynamodb.GetItemAPI, timeProvider clock.TimeProvider) ConcreteConfigurationFetcher {
	return ConcreteConfigurationFetcher{
		global: UncachedConcreteGlobalConfigurationFetcher{
			getter: client,
			cache:  GetCache(timeProvider),
		},
		machine: ConcreteMachineConfigurationFetcher{
			getter: client,
		},
		universal: ConcreteUniversalConfigurationProvider{},
	}
}

//
// ConfigurationFetcher is intended to jump through all of the hoops to get the configuration that is
// intended to be deployed to a particular machine, resolving through defaults, overrides, and fallbacks
// and other logic.
//
// type ConfigurationFetcher interface {
// 	GetIntendedConfig(machineID string) (intendedConfig MachineConfiguration, err error)
// 	GetIntendedGlobalConfig() (intendedConfig MachineConfiguration, err error)
// }

type ConcreteConfigurationFetcher struct {
	global    GlobalConfigurationFetcher
	machine   MachineConfigurationFetcher
	universal UniversalConfigurationProvider
}

// GetIntendedConfig returns the machineconfiguration intended for the given machineID:
// - the global config from DynamoDB, or the hardcoded "universal config" if there is none
// - overridden by each setting that the machine-specific config in DynamoDB sets, if there is one
//
// A machine-specific config overrides only the settings stored on it; every other setting follows the current
// global config. Rows written by `config set --machine` (and by earlier versions) store every base setting, so
// they keep overriding all of them. The clean sync flag is never taken from the global config (see
// withoutCleanSyncRequest), nor is a temporary client mode. A temporary client mode of the machine is returned as
// stored, whatever the time: preflight applies it while it lasts (ApplyTemporaryClientMode).
func (f ConcreteConfigurationFetcher) getIntendedConfig(machineID string) (intendedConfig MachineConfiguration, err error) {
	machineItem, err := f.machine.getMachineSpecificItem(machineID)
	if err != nil {
		err = fmt.Errorf("failed to get machine config: %w", err)
		return
	}

	globalConfig, err := f.global.GetGlobalConfig()
	if err != nil {
		if len(machineItem) == 0 {
			err = fmt.Errorf("failed to get fallback global config: %w", err)
		} else {
			err = fmt.Errorf("failed to get global config for inherited settings: %w", err)
		}
		return
	}
	if globalConfig == nil {
		universal := f.universal.GetUniversalDefaultConfig()
		globalConfig = &universal
	}

	global := withoutTemporaryClientMode(withoutCleanSyncRequest(*globalConfig))
	if len(machineItem) == 0 {
		intendedConfig = global
		return
	}

	return mergeMachineConfig(global, machineItem)
}

// withoutCleanSyncRequest returns a global config without its clean sync flag. The flag is refused for the global
// config, but earlier versions stored it there; honouring it would make every machine that does not override it
// clean sync on every sync, as nothing ever clears it. Clear it with `rudolph config update --global
// --clean-sync=false`.
func withoutCleanSyncRequest(global MachineConfiguration) MachineConfiguration {
	global.CleanSync = false
	global.CleanSyncRequest = ""
	return global
}

// GetIntendedGlobalConfig returns the global configuration. It searches in the following order:
// - First, looks for a global config for all machines in DynamoDB
// - Defaults to returning a hardcoded "universal config"
func (f ConcreteConfigurationFetcher) getIntendedGlobalConfig() (intendedConfig MachineConfiguration, isDefaultConfig bool, err error) {
	config, err := f.global.GetGlobalConfig()
	if err != nil {
		err = fmt.Errorf("failed to get fallback global config: %w", err)
		return
	}

	// Check if the result.Item is not nil. If this is nil, no valid global config exists within the DynamoDB and return a generic hardcoded value
	if config == nil {
		isDefaultConfig = true
		intendedConfig = f.universal.GetUniversalDefaultConfig()
	} else {
		intendedConfig = *config
	}

	return
}

//
// GlobalConfigurationFetcher is intended to fetch the global configuration that all machines fallback onto when
// they are missing their machine-specific overrides.
//
// Because the global config is not intended to change frequently, we cache this item to save on dynamodb getitem calls.
//

func GetGlobalConfigurationFetcher(client dynamodb.GetItemAPI, timeProvider clock.TimeProvider) GlobalConfigurationFetcher {
	return CachedConcreteGlobalConfigurationFetcher{
		getter: client,
		cache:  GetCache(timeProvider),
	}
}

func GetUncachedGlobalConfigurationFetcher(client dynamodb.GetItemAPI, timeProvider clock.TimeProvider) GlobalConfigurationFetcher {
	return UncachedConcreteGlobalConfigurationFetcher{
		getter: client,
		cache:  GetCache(timeProvider),
	}
}

type GlobalConfigurationFetcher interface {
	GetGlobalConfig() (*MachineConfiguration, error)
}

type CachedConcreteGlobalConfigurationFetcher struct {
	getter dynamodb.GetItemAPI
	cache  Cache
}

type UncachedConcreteGlobalConfigurationFetcher struct {
	getter dynamodb.GetItemAPI
	cache  Cache
}

func (c CachedConcreteGlobalConfigurationFetcher) GetGlobalConfig() (config *MachineConfiguration, err error) {
	if c.cache.Has(CacheKeyGlobal) {
		config = c.cache.Get(CacheKeyGlobal)
		return
	}

	config, err = getItemAsMachineConfiguration(
		c.getter,
		globalConfigurationPK,
		machineConfigurationSK(),
	)
	if err != nil {
		err = fmt.Errorf("failed to get global config: %w", err)
		return
	}

	// Here, it is perfectly possible to have nil config. In this case, we *should* set nil into the cache
	// as it simply means there are no configurations set.
	c.cache.Set(CacheKeyGlobal, config)

	return config, nil
}

func (c UncachedConcreteGlobalConfigurationFetcher) GetGlobalConfig() (config *MachineConfiguration, err error) {
	config, err = getItemAsMachineConfiguration(
		c.getter,
		globalConfigurationPK,
		machineConfigurationSK(),
	)
	if err != nil {
		err = fmt.Errorf("failed to get global config: %w", err)
		return
	}
	return
}

//
// MachineConfigurationFetcher is intended to retrieve any machine-specific configuration override, if prevent, for
// the given machineID. This override takes precedence over the global and universal defaults.
//

func GetMachineConfigurationFetcher(client dynamodb.GetItemAPI) MachineConfigurationFetcher {
	return ConcreteMachineConfigurationFetcher{
		getter: client,
	}
}

type MachineConfigurationFetcher interface {
	GetMachineSpecificConfig(machineID string) (*MachineConfiguration, error)
	// getMachineSpecificItem returns the stored attributes of the machine-specific config, or nil if there is none
	getMachineSpecificItem(machineID string) (map[string]awstypes.AttributeValue, error)
}

type ConcreteMachineConfigurationFetcher struct {
	getter dynamodb.GetItemAPI
}

func (f ConcreteMachineConfigurationFetcher) GetMachineSpecificConfig(machineID string) (config *MachineConfiguration, err error) {
	return getItemAsMachineConfiguration(
		f.getter,
		machineConfigurationPK(machineID),
		machineConfigurationSK(),
	)
}

func (f ConcreteMachineConfigurationFetcher) getMachineSpecificItem(machineID string) (map[string]awstypes.AttributeValue, error) {
	output, err := f.getter.GetItem(
		dynamodb.PrimaryKey{
			PartitionKey: machineConfigurationPK(machineID),
			SortKey:      machineConfigurationSK(),
		},
		false,
	)
	if err != nil {
		return nil, err
	}
	return output.Item, nil
}

//
// UniversalConfigurationProvider provides a universal config that all machines can fall back onto, when they neither
// have machine-specific nor global overrides.
//

func GetUniversalConfigurationProvider() UniversalConfigurationProvider {
	return ConcreteUniversalConfigurationProvider{}
}

type UniversalConfigurationProvider interface {
	GetUniversalDefaultConfig() MachineConfiguration
}

type ConcreteUniversalConfigurationProvider struct{}

func (p ConcreteUniversalConfigurationProvider) GetUniversalDefaultConfig() MachineConfiguration {
	return GetUniversalDefaultConfig()
}

// ============================== //
// PRIVATE AND DEPRECATED METHODS //
// ============================== //

// @deprecated
func GetIntendedConfig(client dynamodb.GetItemAPI, machineID string) (intendedConfig MachineConfiguration, err error) {
	config, err := GetMachineSpecificConfig(client, machineID)
	if err != nil {
		err = fmt.Errorf("failed to get machine config: %w", err)
		return
	}

	if config == nil {
		log.Printf("no config items found for %s", machineID)
		log.Printf("%s", "Retrieving global config")

		config, err = GetGlobalConfig(client)
		if err != nil {
			err = fmt.Errorf("failed to get global config: %w", err)
			return
		}
	}

	// Check if the result.Item is not nil. If this is nil, no valid global config exists within the DynamoDB and return a generic hardcoded value
	if config == nil {
		log.Printf("WARNING: GLOBAL CONFIG ITEM NOT FOUND; DEFAULTING TO HARDCODED DEFAULT CONFIG.")

		intendedConfig = GetUniversalDefaultConfig()
	} else {
		intendedConfig = *config
	}

	return
}

// @deprecated
func GetMachineSpecificConfig(client dynamodb.GetItemAPI, machineID string) (config *MachineConfiguration, err error) {
	return getItemAsMachineConfiguration(
		client,
		machineConfigurationPK(machineID),
		machineConfigurationSK(),
	)
}

// @deprecated
func GetGlobalConfig(client dynamodb.GetItemAPI) (config *MachineConfiguration, err error) {
	return getItemAsMachineConfiguration(
		client,
		globalConfigurationPK,
		machineConfigurationSK(),
	)
}

func getItemAsMachineConfiguration(client dynamodb.GetItemAPI, partitionKey string, sortKey string) (config *MachineConfiguration, err error) {
	output, err := client.GetItem(
		dynamodb.PrimaryKey{
			PartitionKey: partitionKey,
			SortKey:      sortKey,
		},
		false,
	)

	if err != nil {
		return
	}

	if len(output.Item) == 0 {
		return
	}

	decoded := unmarshalConfigItem(output.Item, partitionKey)
	config = &decoded
	return
}
