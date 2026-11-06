package machineconfiguration

import (
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Attributes of a machine-specific config row that are not settings, or settings never inherited from the global
// config
var notInheritedAttributes = []string{"PK", "SK", "DataType", "CleanSync", "CleanSyncRequest", temporaryClientModeAttribute, temporaryClientModeExpiresAtAttribute}

// unmarshalConfigItem decodes a stored config row. An attribute whose stored type does not fit its setting (e.g. a
// hand-edited "yes" for a boolean) is left out with a log, so one bad attribute does not make the whole config, and
// so the preflight of every machine using it, fail. Values of the right type are validated when they are sent
// (SanitizeForSanta).
func unmarshalConfigItem(item map[string]awstypes.AttributeValue, source string) MachineConfiguration {
	var config MachineConfiguration
	if err := attributevalue.UnmarshalMap(item, &config); err == nil {
		return config
	}
	usable := make(map[string]awstypes.AttributeValue, len(item))
	remountUsbModeDropped := false
	for name, value := range item {
		if err := decodesAsSetting(name, value); err != nil {
			log.Printf("ignoring attribute %s of config %s: %v", name, source, err)
			remountUsbModeDropped = remountUsbModeDropped || name == remountUsbModeAttribute
			continue
		}
		usable[name] = value
	}
	config = MachineConfiguration{}
	if err := attributevalue.UnmarshalMap(usable, &config); err != nil {
		// Unreachable: every remaining attribute decodes on its own
		log.Printf("ignoring config %s that cannot be decoded: %v", source, err)
		return MachineConfiguration{}
	}
	config.remountUsbModeDropped = remountUsbModeDropped
	return config
}

// decodesAsSetting reports whether a single stored attribute can be decoded into its MachineConfiguration field
func decodesAsSetting(name string, value awstypes.AttributeValue) error {
	var scratch MachineConfiguration
	return attributevalue.UnmarshalMap(map[string]awstypes.AttributeValue{name: value}, &scratch)
}

// mergeMachineConfig returns the global config with every setting stored on the machine-specific config row
// overriding it. Attributes of the row that are NULL or cannot be decoded do not override the global setting.
func mergeMachineConfig(global MachineConfiguration, machineItem map[string]awstypes.AttributeValue) (MachineConfiguration, error) {
	merged, err := attributevalue.MarshalMap(global)
	if err != nil {
		return MachineConfiguration{}, fmt.Errorf("failed to encode the global config: %w", err)
	}
	for _, name := range notInheritedAttributes {
		delete(merged, name)
	}
	// Remount flags the machine config sets but that cannot be decoded are not replaced by the global ones: the
	// machine's intended flags are unknown either way
	remountUsbModeDropped := global.remountUsbModeDropped
	for name, value := range machineItem {
		if name == "PK" || name == "SK" {
			continue
		}
		if _, isNull := value.(*awstypes.AttributeValueMemberNULL); isNull {
			continue
		}
		err := decodesAsSetting(name, value)
		if name == remountUsbModeAttribute {
			remountUsbModeDropped = err != nil
		}
		if err != nil {
			log.Printf("ignoring attribute %s of a machine config: %v", name, err)
			continue
		}
		merged[name] = value
	}
	config := unmarshalConfigItem(merged, "merged machine config")
	config.remountUsbModeDropped = remountUsbModeDropped
	return config, nil
}
