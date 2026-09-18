package machineconfiguration

import (
	"fmt"
	"log"
	"strings"

	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// MountFlags are the mount flags Santa remounts blocked USB mass storage devices with (santa.sync.v1
// remount_usb_mode, a repeated string).
type MountFlags []string

// UnmarshalDynamoDBAttributeValue reads a list or string set of flags, and also the comma separated string
// that hand-edited rows and older tooling store. An attribute of another type is ignored (with a log), so one
// odd row does not fail the preflight of every host using it.
func (m *MountFlags) UnmarshalDynamoDBAttributeValue(av awstypes.AttributeValue) error {
	switch value := av.(type) {
	case *awstypes.AttributeValueMemberL:
		flags := MountFlags{}
		for _, element := range value.Value {
			flag, ok := element.(*awstypes.AttributeValueMemberS)
			if !ok {
				log.Printf("ignoring non-string element %T in RemountUsbMode", element)
				continue
			}
			flags = append(flags, flag.Value)
		}
		*m = flags
	case *awstypes.AttributeValueMemberSS:
		*m = append(MountFlags{}, value.Value...)
	case *awstypes.AttributeValueMemberS:
		*m = parseMountFlags(value.Value)
	case *awstypes.AttributeValueMemberNULL:
		*m = nil
	default:
		log.Printf("ignoring RemountUsbMode of unsupported type %T", av)
		*m = nil
	}
	return nil
}

// parseMountFlags splits a comma separated list of mount flags
func parseMountFlags(value string) MountFlags {
	flags := MountFlags{}
	for _, flag := range strings.Split(value, ",") {
		if flag = strings.TrimSpace(flag); flag != "" {
			flags = append(flags, flag)
		}
	}
	return flags
}

// inheritOptionalSettings fills every optional Santa setting that the machine-specific config does not configure
// from the global config. Base settings (client mode, path regexes, ...) are never inherited.
func inheritOptionalSettings(machine MachineConfiguration, global MachineConfiguration) MachineConfiguration {
	if machine.EnableAllEventUpload == nil {
		machine.EnableAllEventUpload = global.EnableAllEventUpload
	}
	if machine.DisableUnknownEventUpload == nil {
		machine.DisableUnknownEventUpload = global.DisableUnknownEventUpload
	}
	if machine.OverrideFileAccessAction == "" {
		machine.OverrideFileAccessAction = global.OverrideFileAccessAction
	}
	if machine.BlockUsbMount == nil {
		machine.BlockUsbMount = global.BlockUsbMount
	}
	if machine.RemountUsbMode == nil {
		machine.RemountUsbMode = global.RemountUsbMode
	}
	if machine.EventDetailURL == nil {
		machine.EventDetailURL = global.EventDetailURL
	}
	if machine.EventDetailText == nil {
		machine.EventDetailText = global.EventDetailText
	}
	return machine
}

// withoutOptionalSettings returns the config with all optional Santa settings unset
func withoutOptionalSettings(config MachineConfiguration) MachineConfiguration {
	config.EnableAllEventUpload = nil
	config.DisableUnknownEventUpload = nil
	config.OverrideFileAccessAction = ""
	config.BlockUsbMount = nil
	config.RemountUsbMode = nil
	config.EventDetailURL = nil
	config.EventDetailText = nil
	return config
}

// normalizeOverrideFileAccessAction returns the enum name for a (possibly legacy) spelling of a file access
// action, and rejects values Santa does not know. "" (not configured) stays "".
func normalizeOverrideFileAccessAction(action types.FileAccessAction) (types.FileAccessAction, error) {
	if action == "" {
		return "", nil
	}
	normalized, err := action.Normalize()
	if err != nil {
		return "", fmt.Errorf("invalid override file access action: %w", err)
	}
	return normalized, nil
}

// validateRemountUsbMode rejects remount flags when the effective config does not block USB mounts: Santa only
// applies remount_usb_mode together with block_usb_mount, so the flags would silently do nothing.
func validateRemountUsbMode(remountUsbMode MountFlags, effectiveBlockUsbMount *bool) error {
	if len(remountUsbMode) > 0 && (effectiveBlockUsbMount == nil || !*effectiveBlockUsbMount) {
		return fmt.Errorf("remount USB mode %q requires blocking USB mounts (BlockUsbMount), which Santa needs to apply it", strings.Join(remountUsbMode, ","))
	}
	return nil
}
