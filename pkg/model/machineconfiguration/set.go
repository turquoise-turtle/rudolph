package machineconfiguration

import (
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
)

// SetGlobalConfig writes a global configuration to the DynamoDB table via a PutItem API call
func SetGlobalConfig(client dynamodb.PutItemAPI, clientMode types.ClientMode, blockedPathRegex string, allowedPathRegex string, batchSize int, isEnableBundles bool, isEnabledTransitiveRules bool, fullSyncInterval int, uploadLogsURL string, opts ...GlobalConfigOption) (err error) {
	// This is a safety check to prevent the accidental setting of lockdown mode globally via a config set operation
	err = checkGlobalClientMode(clientMode, opts)
	if err != nil {
		return
	}

	// Construct a MachineConfigRow to represent a GlobalConfig
	globalConfigRow := buildConfig(
		globalConfigurationPK,
		clientMode,
		blockedPathRegex,
		allowedPathRegex,
		batchSize,
		isEnableBundles,
		isEnabledTransitiveRules,
		false,
		fullSyncInterval,
		uploadLogsURL,
	)
	err = ValidateConfig(globalConfigRow.MachineConfiguration, true)
	if err != nil {
		return
	}

	_, err = client.PutItem(globalConfigRow)
	if err != nil {
		err = fmt.Errorf("setting global config failed: %w", err)
	}

	return
}

// SetMachineConfig writes a machineID specific configuration to the DynamoDB table via a PutItem API call
func SetMachineConfig(client dynamodb.PutItemAPI, machineID string, clientMode types.ClientMode, blockedPathRegex string, allowedPathRegex string, batchSize int, isEnableBundles bool, isEnabledTransitiveRules bool, isCleanSync bool, fullSyncInterval int, uploadLogsURL string) (err error) {
	// Create the machineID specific PK
	machinePK := machineConfigurationPK(machineID)

	// Construct a MachineConfigRow to represent a MachineConfig
	machineConfigRow := buildConfig(
		machinePK,
		clientMode,
		blockedPathRegex,
		allowedPathRegex,
		batchSize,
		isEnableBundles,
		isEnabledTransitiveRules,
		isCleanSync,
		fullSyncInterval,
		uploadLogsURL,
	)
	err = ValidateConfig(machineConfigRow.MachineConfiguration, false)
	if err != nil {
		return
	}
	if isCleanSync {
		machineConfigRow.CleanSyncRequest = newCleanSyncRequest()
	}

	_, err = client.PutItem(machineConfigRow)
	if err != nil {
		err = fmt.Errorf("setting machine config failed: %w", err)
	}

	return
}

// buildConfig constructs a MachineConfigurationRow which will represent either a global or machineID specific configuration set to be used in a DynamoDB PutItem API call
func buildConfig(pk string, clientMode types.ClientMode, blockedPathRegex string, allowedPathRegex string, batchSize int, isEnableBundles bool, isEnabledTransitiveRules bool, isCleanSync bool, fullSyncInterval int, uploadLogsURL string) (configRow *MachineConfigurationRow) {
	return buildConfigRow(pk, MachineConfiguration{
		ClientMode:             clientMode,
		BlockedPathRegex:       blockedPathRegex,
		AllowedPathRegex:       allowedPathRegex,
		BatchSize:              batchSize,
		EnableBundles:          isEnableBundles,
		EnabledTransitiveRules: isEnabledTransitiveRules,
		CleanSync:              isCleanSync,
		FullSyncInterval:       fullSyncInterval,
		UploadLogsURL:          uploadLogsURL,
	})
}

// buildConfigRow constructs a MachineConfigurationRow containing all settings of the given config for the given pk
func buildConfigRow(pk string, config MachineConfiguration) (configRow *MachineConfigurationRow) {
	// A batch size that is not configured (0) is stored as the default
	if config.BatchSize <= 0 {
		config.BatchSize = DefaultBatchSize
	}

	if strings.Compare(pk, globalConfigurationPK) == 0 {
		config.DataType = types.DataTypeGlobalConfig
	} else {
		config.DataType = types.DataTypeMachineConfig
	}

	configRow = &MachineConfigurationRow{
		dynamodb.PrimaryKey{
			PartitionKey: pk,
			SortKey:      machineConfigurationSK(),
		},
		config,
	}
	return
}

// ConfigurationStore reads and writes configuration rows
type ConfigurationStore interface {
	dynamodb.GetItemAPI
	dynamodb.PutItemAPI
}

// GetConfigurationSetter returns a setter that does not cache what it writes. The timeProvider is unused.
func GetConfigurationSetter(client ConfigurationStore, timeProvider clock.TimeProvider) ConcreteConfigurationSetter {
	return newConfigurationSetter(client, nil)
}

// newConfigurationSetter returns a setter that stores a written global config in cache, if not nil
func newConfigurationSetter(client ConfigurationStore, cache Cache) ConcreteConfigurationSetter {
	return ConcreteConfigurationSetter{
		global: ConcreteGlobalConfigurationSetter{
			setter: client,
			cache:  cache,
		},
		machine: ConcreteMachineConfigurationSetter{
			setter: client,
			global: UncachedConcreteGlobalConfigurationFetcher{getter: client},
		},
	}
}

//
// ConfigurationSetter is an interface to set and override the all contents of a Global or MachineConfiguration.
// This can be used to initially create a configuration for either a global or machine configuration.
//
// type ConfigurationSetter interface {
// 	SetGlobalConfig(config MachineConfiguration) error
// 	SetMachineConfig(machineID string, config MachineConfiguration) error
// }

type ConcreteConfigurationSetter struct {
	global  GlobalConfigurationSetter
	machine MachineConfigurationSetter
}

// func (f ConcreteConfigurationSetter) SetGlobalConfig(config MachineConfiguration) error {
// 	err := f.global.setGlobalConfig(config)
// 	return err
// }

// func (f ConcreteConfigurationSetter) SetMachineConfig(machineID string, config MachineConfiguration) error {
// 	err := f.machine.setMachineConfig(machineID, config)
// 	return err
// }

// GlobalConfigurationSetter
type GlobalConfigurationSetter interface {
	setGlobalConfig(config MachineConfiguration, opts ...GlobalConfigOption) error
}

type ConcreteGlobalConfigurationSetter struct {
	setter dynamodb.PutItemAPI
	cache  Cache
}

func (c ConcreteGlobalConfigurationSetter) setGlobalConfig(config MachineConfiguration, opts ...GlobalConfigOption) error {
	err := ValidateConfig(config, true)
	if err != nil {
		return err
	}

	err = checkGlobalClientMode(config.ClientMode, opts)
	if err != nil {
		return err
	}

	config.OverrideFileAccessAction, err = normalizeOverrideFileAccessAction(config.OverrideFileAccessAction)
	if err != nil {
		return err
	}

	err = validateRemountUsbMode(config.RemountUsbMode, config.BlockUsbMount)
	if err != nil {
		return err
	}

	// Construct a MachineConfigRow to represent a GlobalConfig
	config.CleanSyncRequest = ""
	globalConfigRow := buildConfigRow(globalConfigurationPK, config)

	_, err = c.setter.PutItem(globalConfigRow)
	if err != nil {
		return fmt.Errorf("setting global config failed: %w", err)
	}

	setCachedGlobalConfig(c.cache, &globalConfigRow.MachineConfiguration)

	return nil
}

// MachineConfigurationSetter
type MachineConfigurationSetter interface {
	setMachineConfig(machineID string, config MachineConfiguration) error
}

type ConcreteMachineConfigurationSetter struct {
	setter dynamodb.PutItemAPI
	// global provides the settings a machine config inherits, for validation
	global GlobalConfigurationFetcher
}

func (c ConcreteMachineConfigurationSetter) setMachineConfig(machineID string, config MachineConfiguration) error {
	err := ValidateConfig(config, false)
	if err != nil {
		return err
	}

	config.OverrideFileAccessAction, err = normalizeOverrideFileAccessAction(config.OverrideFileAccessAction)
	if err != nil {
		return err
	}

	blockUsbMount := config.BlockUsbMount
	if blockUsbMount == nil && len(config.RemountUsbMode) > 0 && c.global != nil {
		globalConfig, eerr := c.global.GetGlobalConfig()
		if eerr != nil {
			return eerr
		}
		if globalConfig != nil {
			blockUsbMount = globalConfig.BlockUsbMount
		}
	}
	err = validateRemountUsbMode(config.RemountUsbMode, blockUsbMount)
	if err != nil {
		return err
	}

	// Every clean sync requested gets its own identifier; the row is replaced as a whole, so a request still pending
	// is replaced by this one, or cancelled when this config requests none
	config.CleanSyncRequest = ""
	if config.CleanSync {
		config.CleanSyncRequest = newCleanSyncRequest()
	}

	// Create the machineID specific PK
	machinePK := machineConfigurationPK(machineID)

	// Construct a MachineConfigRow to represent a MachineConfig
	machineConfigRow := buildConfigRow(machinePK, config)

	_, err = c.setter.PutItem(machineConfigRow)
	if err != nil {
		return fmt.Errorf("setting machine config failed: %w", err)
	}
	return nil
}

// setCachedGlobalConfig stores a global config that was just written into cache, if there is one
func setCachedGlobalConfig(cache Cache, config *MachineConfiguration) {
	if cache != nil {
		_ = cache.Set(CacheKeyGlobal, config)
	}
}
