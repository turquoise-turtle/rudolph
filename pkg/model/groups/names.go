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

// ErrFormatCharacter is Normalise's error for a name with an invisible format character. Groups registered before
// 2026-11-18 may have such names; NormaliseExisting accepts them so they can still be managed and deleted.
var ErrFormatCharacter = errors.New("a group name cannot contain invisible format characters (such as bidi overrides or zero-width spaces)")

// Normalise returns the canonical form of a group name: Unicode NFC, trimmed, lower case. It refuses names that are
// empty, are "." or "..", contain control characters or invisible format characters (Unicode Cf other than the
// zero-width joiner and non-joiner), are longer than MaxNameBytes or use the reserved prefix.
func Normalise(name string) (string, error) {
	n, err := NormaliseExisting(name)
	if err == nil && strings.IndexFunc(n, isRefusedFormat) >= 0 {
		return "", ErrFormatCharacter
	}
	return n, err
}

// NormaliseExisting is Normalise without the check for invisible format characters, for names that must already be
// registered (every operation but creating a group). The caller must still check that the group exists, which every
// model function that changes a group does (ErrGroupNotFound); that keeps new names to Normalise's rules.
func NormaliseExisting(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(norm.NFC.String(name)))
	switch {
	case n == "":
		return "", errors.New("a group name cannot be empty")
	case n == "." || n == "..":
		// rudolph-web serves a group at /groups/<name>, and browsers and routers resolve these path segments away.
		return "", errors.New(`a group name cannot be "." or ".."`)
	case len(n) > MaxNameBytes:
		return "", fmt.Errorf("a group name is at most %d bytes", MaxNameBytes)
	case strings.IndexFunc(n, unicode.IsControl) >= 0:
		return "", errors.New("a group name cannot contain control characters")
	case strings.HasPrefix(n, ReservedPrefix):
		return "", ErrReservedName
	}
	return n, nil
}

// isRefusedFormat reports invisible format characters (Unicode Cf): bidi overrides and isolates, zero-width spaces,
// soft hyphens and the like, which would make a name display as something it is not. The zero-width non-joiner and
// joiner are allowed: Persian and several Indic scripts need them in ordinary spelling.
func isRefusedFormat(r rune) bool {
	return r != 0x200c && r != 0x200d && unicode.Is(unicode.Cf, r)
}

// NormaliseTarget is Normalise, except that it also accepts Ungrouped, which rules (but not members) can target.
func NormaliseTarget(name string) (string, error) {
	if isUngrouped(name) {
		return Ungrouped, nil
	}
	return Normalise(name)
}

// NormaliseExistingTarget is NormaliseExisting, except that it also accepts Ungrouped.
func NormaliseExistingTarget(name string) (string, error) {
	if isUngrouped(name) {
		return Ungrouped, nil
	}
	return NormaliseExisting(name)
}

func isUngrouped(name string) bool {
	return strings.ToLower(strings.TrimSpace(name)) == Ungrouped
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
