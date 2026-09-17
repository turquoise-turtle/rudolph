package preflight

import (
	"log"

	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
)

// PreflightResponse represents sync response returned to a Santa client by the sync server.
//
// WARNING: The PreflightResponse copies its format directly from the database; make sure this struct's fields
//
//	are consistent with the fields of the store.MachineConfiguration type
//
// Field names and types follow santa.sync.v1.PreflightResponse (testdata/santa-2026.8/sync_v1.proto).
// Santa leaves a setting unchanged when its optional field is absent, so settings that are not configured
// are omitted. The path regexes are always sent: on Santa 2026.8 "" clears a regex, an absent field keeps it.
type PreflightResponse struct {
	ClientMode                types.ClientMode       `json:"client_mode"`
	BlockedPathRegex          string                 `json:"blocked_path_regex"`
	AllowedPathRegex          string                 `json:"allowed_path_regex"`
	BatchSize                 int                    `json:"batch_size"`
	EnableBundles             bool                   `json:"enable_bundles"`
	EnabledTransitiveRules    bool                   `json:"enable_transitive_rules"`
	FullSyncInterval          int                    `json:"full_sync_interval,omitempty"`
	UploadLogsURL             string                 `json:"upload_logs_url,omitempty"`
	BlockUsbMount             *bool                  `json:"block_usb_mount,omitempty"`
	RemountUsbMode            []string               `json:"remount_usb_mode,omitempty"`
	SyncType                  types.SyncType         `json:"sync_type,omitempty"`
	OverrideFileAccessAction  types.FileAccessAction `json:"override_file_access_action,omitempty"`
	EnableAllEventUpload      *bool                  `json:"enable_all_event_upload,omitempty"`
	DisableUnknownEventUpload *bool                  `json:"disable_unknown_event_upload,omitempty"`
	EventDetailURL            *string                `json:"event_detail_url,omitempty"`
	EventDetailText           *string                `json:"event_detail_text,omitempty"`
}

// ConstructPreflightResponse converts a MachineConfiguration pulled from the database into the corresponding
// response to be return as an API response.
func ConstructPreflightResponse(machineConfiguration machineconfiguration.MachineConfiguration, cleanSync bool) *PreflightResponse {
	var syncType types.SyncType
	switch cleanSync {
	case true:
		syncType = types.SyncTypeClean
	default:
		syncType = types.SyncTypeNormal
	}

	// An enum value Santa does not know would make it reject the whole preflight response
	overrideFileAccessAction := machineConfiguration.OverrideFileAccessAction
	if overrideFileAccessAction != "" && !overrideFileAccessAction.IsValid() {
		log.Printf("ignoring invalid OverrideFileAccessAction %q in machine configuration", overrideFileAccessAction)
		overrideFileAccessAction = ""
	}

	return &PreflightResponse{
		ClientMode:                machineConfiguration.ClientMode,
		BlockedPathRegex:          machineConfiguration.BlockedPathRegex,
		AllowedPathRegex:          machineConfiguration.AllowedPathRegex,
		BatchSize:                 machineConfiguration.BatchSize,
		EnableBundles:             machineConfiguration.EnableBundles,
		EnabledTransitiveRules:    machineConfiguration.EnabledTransitiveRules,
		UploadLogsURL:             machineConfiguration.UploadLogsURL,
		FullSyncInterval:          machineConfiguration.FullSyncInterval,
		SyncType:                  syncType,
		BlockUsbMount:             machineConfiguration.BlockUsbMount,
		RemountUsbMode:            machineConfiguration.RemountUsbMode,
		OverrideFileAccessAction:  overrideFileAccessAction,
		EnableAllEventUpload:      machineConfiguration.EnableAllEventUpload,
		DisableUnknownEventUpload: machineConfiguration.DisableUnknownEventUpload,
		EventDetailURL:            machineConfiguration.EventDetailURL,
		EventDetailText:           machineConfiguration.EventDetailText,
		// Notably, we do not grab the clean sync from the config
	}
}
