// Package groups implements device groups (docs/groups-design.md): named sets of Macs that rules can target besides
// the whole fleet and a single machine.
//
// A Mac's groups are the registered groups among those Santa reports in preflight (primary_user_groups, for groups
// that accept reported membership) together with the groups assigned to it in Rudolph. A Mac in none of them is in
// the reserved group Ungrouped. Ruledownload sends, for each rule type and identifier, the strongest rule of the
// Mac's groups with the Mac's machine rules (see Resolve).
package groups

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	// ReservedPrefix starts the names Rudolph keeps for itself; nobody can create or report such a group
	ReservedPrefix = "rudolph:"
	// Ungrouped is the group of every Mac that is in no registered group
	Ungrouped = ReservedPrefix + "ungrouped"

	// MaxNameBytes bounds a group name after normalisation
	MaxNameBytes = 128
	// MaxGroups is how many groups can be registered, Ungrouped not included
	MaxGroups = 64
	// MaxRulesPerGroup bounds a group's rules: every member Mac receives all of them on every sync
	MaxRulesPerGroup = 5000
)

var ErrReservedName = fmt.Errorf("group names starting with %q are reserved", ReservedPrefix)

// Normalise returns the canonical form of a group name: Unicode NFC, trimmed, lower case. It refuses names that are
// empty, contain control characters, are longer than MaxNameBytes or use the reserved prefix.
func Normalise(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(norm.NFC.String(name)))
	switch {
	case n == "":
		return "", errors.New("a group name cannot be empty")
	case len(n) > MaxNameBytes:
		return "", fmt.Errorf("a group name is at most %d bytes", MaxNameBytes)
	case strings.IndexFunc(n, unicode.IsControl) >= 0:
		return "", errors.New("a group name cannot contain control characters")
	case strings.HasPrefix(n, ReservedPrefix):
		return "", ErrReservedName
	}
	return n, nil
}

// NormaliseTarget is Normalise, except that it also accepts Ungrouped, which rules (but not members) can target.
func NormaliseTarget(name string) (string, error) {
	if strings.ToLower(strings.TrimSpace(name)) == Ungrouped {
		return Ungrouped, nil
	}
	return Normalise(name)
}

// NormaliseList returns the valid names of a list in canonical form, sorted and without duplicates. Invalid names (see
// Normalise) are dropped, and their number returned so the caller can log it without logging the names.
func NormaliseList(names []string) (normalised []string, dropped int) {
	seen := map[string]bool{}
	for _, name := range names {
		n, err := Normalise(name)
		if err != nil {
			dropped++
			continue
		}
		if !seen[n] {
			seen[n] = true
			normalised = append(normalised, n)
		}
	}
	sort.Strings(normalised)
	return normalised, dropped
}
