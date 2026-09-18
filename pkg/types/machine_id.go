package types

import (
	"fmt"
	"regexp"
)

// machineRegexp matches the canonical 36 character UUID form (8-4-4-4-12), in either case.
//
// The spellings a UUID parser would normally also accept - braces, a `urn:uuid:` prefix, or 32 hex digits
// without dashes - are deliberately not matched: they name the same machine with a different key, so one Mac
// could end up with several sets of rules, configuration and sync state. The case of an ID is never
// rewritten either, so hosts keep the identity they already sync under.
var machineRegexp = regexp.MustCompile(`^(?i:[A-F0-9]{8}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{12})$`)

// ValidateMachineID returns an error if a machineID is not a properly formatted UUID string
func ValidateMachineID(machineID string) error {
	if !machineRegexp.MatchString(machineID) {
		return fmt.Errorf("invalid machineID: %s", machineID)
	}
	return nil
}
