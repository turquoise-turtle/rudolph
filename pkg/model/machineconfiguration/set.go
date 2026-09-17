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
	// Check batchsize just to make sure at least a valid value is provided if not a positive int value
	if config.BatchSize <= 0 {
		config.BatchSize = 50
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

// validateConfig rejects settings that a Santa client would fail to parse
func validateConfig(config MachineConfiguration) error {
	if config.OverrideFileAccessAction != "" && !config.OverrideFileAccessAction.IsValid() {
		return fmt.Errorf("invalid override file access action %q", config.OverrideFileAccessAction)
	}
	return nil
}

func GetConfigurationSetter(client dynamodb.PutItemAPI, timeProvider clock.TimeProvider) ConcreteConfigurationSetter {
	return ConcreteConfigurationSetter{
		global: ConcreteGlobalConfigurationSetter{
			setter: client,
			cache:  GetCache(timeProvider),
		},
		machine: ConcreteMachineConfigurationSetter{
			setter: client,
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
	err := checkGlobalClientMode(config.ClientMode, opts)
	if err != nil {
		return err
	}

	err = validateConfig(config)
	if err != nil {
		return err
	}

	// Construct a MachineConfigRow to represent a GlobalConfig
	globalConfigRow := buildConfigRow(globalConfigurationPK, config)

	_, err = c.setter.PutItem(globalConfigRow)
	if err != nil {
		return fmt.Errorf("setting global config failed: %w", err)
	}

	// Attempt to set the new global configuration into cache
	_ = c.cache.Set(CacheKeyGlobal, &globalConfigRow.MachineConfiguration)

	return nil
}

// MachineConfigurationSetter
type MachineConfigurationSetter interface {
	setMachineConfig(machineID string, config MachineConfiguration) error
}

type ConcreteMachineConfigurationSetter struct {
	setter dynamodb.PutItemAPI
}

func (c ConcreteMachineConfigurationSetter) setMachineConfig(machineID string, config MachineConfiguration) error {
	err := validateConfig(config)
	if err != nil {
		return err
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
