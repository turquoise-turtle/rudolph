package machineconfiguration

import (
	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The override_file_access_action a Mac gets is the first of: the OverrideFileAccessAction stored on its own config
// row; the most relaxed override of its device groups (groups.MachineFileAccessOverride, applied with
// ApplyGroupFileAccessOverride); the global config's; none. It is resolved independently of the client mode.

// OwnFileAccessOverride reports whether a machine's stored config row sets a file access override Santa can use, which
// then beats any group's. A row without one inherits it, and so does one whose stored value is NULL or not a file
// access action, which preflight would not send.
func OwnFileAccessOverride(machineItem map[string]awstypes.AttributeValue) bool {
	value, ok := machineItem["OverrideFileAccessAction"].(*awstypes.AttributeValueMemberS)
	if !ok {
		return false
	}
	_, err := types.ParseFileAccessAction(value.Value)
	return err == nil
}

// ApplyGroupFileAccessOverride returns the config with the file access override of the machine's groups, unless its
// own config row sets one (ownOverride, see OwnFileAccessOverride).
func ApplyGroupFileAccessOverride(config MachineConfiguration, ownOverride bool, groupAction types.FileAccessAction) MachineConfiguration {
	if ownOverride || !groupAction.IsValid() {
		return config
	}
	config.OverrideFileAccessAction = groupAction
	return config
}

// Origin says which of the settings a device group can decide are set by the machine's own config row, which then
// beats its groups'
type Origin struct {
	ClientMode       bool
	FileAccessAction bool
}

func originOf(machineItem map[string]awstypes.AttributeValue) Origin {
	return Origin{ClientMode: OwnClientMode(machineItem), FileAccessAction: OwnFileAccessOverride(machineItem)}
}
