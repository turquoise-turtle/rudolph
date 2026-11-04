package machineconfiguration

import (
	"fmt"
	"log"
	"strings"

	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const remountUsbModeAttribute = "RemountUsbMode"

// MountFlags are the mount flags Santa remounts blocked USB mass storage devices with (santa.sync.v1
// remount_usb_mode, a repeated string).
type MountFlags []string

// UnmarshalDynamoDBAttributeValue reads a list or string set of flags, and also the comma separated string
// that hand-edited rows and older tooling store. An attribute of another type is an error; the config decoder then
// leaves the attribute out (with a log), so one odd row does not fail the preflight of every host using it, and
// remembers that the remount flags are unknown (see SanitizeForSanta).
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
		return fmt.Errorf("RemountUsbMode of unsupported type %T", av)
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
