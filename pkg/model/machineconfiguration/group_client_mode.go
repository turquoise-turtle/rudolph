package machineconfiguration

import (
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The client mode a Mac gets is the first of: its temporary client mode while it lasts (ApplyTemporaryClientMode);
// the ClientMode stored on its own config row; the strictest client mode of its device groups (groups.MachineClientMode,
// applied with ApplyGroupClientMode); the global config's, or the universal default's.

// OwnClientMode reports whether a machine's stored config row sets a client mode Santa can use, which then beats any
// group's. A row without ClientMode (one holding only other settings, or only a temporary mode) inherits it, and so
// does one whose stored ClientMode is NULL or not a known mode, which preflight would not send.
func OwnClientMode(machineItem map[string]awstypes.AttributeValue) bool {
	value, ok := machineItem["ClientMode"]
	if !ok {
		return false
	}
	if _, isNull := value.(*awstypes.AttributeValueMemberNULL); isNull {
		return false
	}
	var decoded MachineConfiguration
	if err := attributevalue.UnmarshalMap(map[string]awstypes.AttributeValue{"ClientMode": value}, &decoded); err != nil {
		return false
	}
	return validateClientMode(decoded.ClientMode) == nil
}

// ApplyGroupClientMode returns the config with the client mode of the machine's groups, unless its own config row sets
// one (ownClientMode, see OwnClientMode). Apply the temporary client mode after it, as that beats both.
func ApplyGroupClientMode(config MachineConfiguration, ownClientMode bool, groupMode types.ClientMode) MachineConfiguration {
	if ownClientMode || validateClientMode(groupMode) != nil {
		return config
	}
	config.ClientMode = groupMode
	return config
}
