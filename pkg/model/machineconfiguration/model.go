package machineconfiguration

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
)

const (
	machineConfigurationPKPrefix        = "Machine#"
	globalConfigurationPK               = "GlobalConfig"
	currentSK                           = "Config"
	DefaultFullSyncInterval             = 600
	DefaultSyncTypeNormal        string = SyncTypeNormal
	SyncTypeNormal               string = "normal"
	SyncTypeClean                string = "clean"
	SyncTypeCleanAll             string = "clean_all"
)

// MachineConfigurationRow is an encapsulation of a DynamoDB row containing machine configuration data
type MachineConfigurationRow struct {
	dynamodb.PrimaryKey
	MachineConfiguration
}

// MachineConfiguration is the abstract notion, sans DynamoDB magic (e.g. PK/SK)
// Use Santa defined constants
// https://github.com/google/santa/blob/main/Source/santactl/Commands/sync/SNTCommandSyncConstants.m#L32-L35
type MachineConfiguration struct {
	ClientMode             types.ClientMode `dynamodbav:"ClientMode"`
	BlockedPathRegex       string           `dynamodbav:"BlockedPathRegex"`
	AllowedPathRegex       string           `dynamodbav:"AllowedPathRegex"`
	BatchSize              int              `dynamodbav:"BatchSize"`
	EnableBundles          bool             `dynamodbav:"EnableBundles"`
	EnabledTransitiveRules bool             `dynamodbav:"EnableTransitiveRules"`
	// CleanSync requests one clean sync of a machine; it is cleared once that clean sync is confirmed (see
	// syncstate.ConfigCleanSyncOutcome). Never taken from the global config.
	CleanSync bool `dynamodbav:"CleanSync,omitempty"`
	// CleanSyncRequest identifies the request, written with every CleanSync set to true, so that clearing the flag
	// for a clean sync that started earlier does not cancel a request made in the meantime.
	CleanSyncRequest string `dynamodbav:"CleanSyncRequest,omitempty"`
	FullSyncInterval int    `dynamodbav:"FullSyncInterval,omitempty"`
	UploadLogsURL    string `dynamodbav:"UploadLogsUrl,omitempty"`
	// SyncType                 types.SyncType   `dynamodbav:"SyncType,omitempty"`
	DataType types.DataType `dynamodbav:"DataType,omitempty"`

	// Optional Santa settings. A nil/"" value means "not configured": if neither the machine-specific nor the
	// global config configures it, it is not sent to Santa, which keeps its current value.
	// A machine-specific config row overrides only the attributes stored on it, base and optional settings alike;
	// the others are inherited from the global config (see getIntendedConfig).
	// A non-nil empty RemountUsbMode is configured (no remount flags).
	EnableAllEventUpload      *bool                  `dynamodbav:"EnableAllEventUpload,omitempty"`
	DisableUnknownEventUpload *bool                  `dynamodbav:"DisableUnknownEventUpload,omitempty"`
	OverrideFileAccessAction  types.FileAccessAction `dynamodbav:"OverrideFileAccessAction,omitempty"`
	BlockUsbMount             *bool                  `dynamodbav:"BlockUsbMount,omitempty"`
	RemountUsbMode            MountFlags             `dynamodbav:"RemountUsbMode,omitempty"`
	EventDetailURL            *string                `dynamodbav:"EventDetailURL,omitempty"`
	EventDetailText           *string                `dynamodbav:"EventDetailText,omitempty"`

	// remountUsbModeDropped is set when the stored remount flags of the config could not be decoded, so the
	// intended remount flags are unknown (see SanitizeForSanta).
	remountUsbModeDropped bool
}

type MachineConfigurationUpdateRequest struct {
	ClientMode            *types.ClientMode
	BlockedPathRegex      *string
	AllowedPathRegex      *string
	BatchSize             *int
	EnableBundles         *bool
	EnableTransitiveRules *bool
	CleanSync             *bool
	FullSyncInterval      *int
	UploadLogsURL         *string
	// SyncType                 *types.SyncType
	EnableAllEventUpload      *bool
	DisableUnknownEventUpload *bool
	OverrideFileAccessAction  *types.FileAccessAction
	BlockUsbMount             *bool
	RemountUsbMode            *[]string
	EventDetailURL            *string
	EventDetailText           *string
	// Inherit lists settings (attribute names, see InheritableSettings) to remove from a machine-specific config, so
	// that the machine follows the global config for them again. Refused for the global config.
	Inherit []string
}

// IsEmpty reports whether the update requests no change at all
func (r MachineConfigurationUpdateRequest) IsEmpty() bool {
	return r.ClientMode == nil && r.BlockedPathRegex == nil && r.AllowedPathRegex == nil && r.BatchSize == nil &&
		r.EnableBundles == nil && r.EnableTransitiveRules == nil && r.CleanSync == nil && r.FullSyncInterval == nil &&
		r.UploadLogsURL == nil && r.EnableAllEventUpload == nil && r.DisableUnknownEventUpload == nil &&
		r.OverrideFileAccessAction == nil && r.BlockUsbMount == nil && r.RemountUsbMode == nil &&
		r.EventDetailURL == nil && r.EventDetailText == nil && len(r.Inherit) == 0
}

// Fragments for updates
type updateClientMode struct {
	ClientMode types.ClientMode `dynamodbav:"ClientMode"`
}

func machineConfigurationPK(machineID string) string {
	return fmt.Sprintf("%s%s", machineConfigurationPKPrefix, machineID)
}

func machineConfigurationSK() string {
	return currentSK
}

// Default Universal Config
func GetUniversalDefaultConfig() MachineConfiguration {
	return MachineConfiguration{
		ClientMode:             types.Monitor,
		BlockedPathRegex:       "",
		AllowedPathRegex:       "",
		BatchSize:              50,
		EnableBundles:          false,
		EnabledTransitiveRules: false,
		CleanSync:              false,
		FullSyncInterval:       DefaultFullSyncInterval,
		UploadLogsURL:          "",
		// SyncType:                 types.SyncTypeNormal,
		DataType: types.DataTypeGlobalConfig,
	}
}
