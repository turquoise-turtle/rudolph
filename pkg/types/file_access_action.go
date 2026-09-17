package types

import (
	"fmt"
	"strings"
)

// FileAccessAction overrides the action Santa takes for File Access Authorization (FAA) policies.
// Values are the enum names of santa.sync.v1.FileAccessAction, which is also how they are stored.
type FileAccessAction string

const (
	// FileAccessActionNone applies the FAA policy as written
	FileAccessActionNone FileAccessAction = "NONE"
	// FileAccessActionAuditOnly logs actions that would be denied, but allows them
	FileAccessActionAuditOnly FileAccessAction = "AUDIT_ONLY"
	// FileAccessActionDisable takes no action
	FileAccessActionDisable FileAccessAction = "DISABLE"
)

// ParseFileAccessAction accepts the protocol enum names and the spellings used in Santa's documentation
// (e.g. "auditonly"), case-insensitively.
func ParseFileAccessAction(value string) (FileAccessAction, error) {
	switch strings.ToUpper(value) {
	case "NONE":
		return FileAccessActionNone, nil
	case "AUDIT_ONLY", "AUDITONLY":
		return FileAccessActionAuditOnly, nil
	case "DISABLE":
		return FileAccessActionDisable, nil
	default:
		return "", fmt.Errorf(`unknown file access action %q; must be one of "none", "audit_only" or "disable"`, value)
	}
}

// IsValid reports whether a is one of the enum names a Santa client accepts.
func (a FileAccessAction) IsValid() bool {
	switch a {
	case FileAccessActionNone, FileAccessActionAuditOnly, FileAccessActionDisable:
		return true
	default:
		return false
	}
}
