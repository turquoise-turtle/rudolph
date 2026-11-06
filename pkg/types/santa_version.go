package types

import (
	"strconv"
	"strings"
)

// SantaVersion is the release of Santa a Mac runs, from the santa_version of its preflight (e.g. "2025.12.0" or
// "2026.9"). Santa numbers its releases year.month, optionally followed by a patch number and build metadata.
//
// The zero value stands for the current release: a version that is empty or cannot be parsed is taken to be current,
// so a Mac reporting something unexpected keeps the behaviour of the release Rudolph targets rather than a legacy one.
type SantaVersion struct {
	Year  int
	Month int
}

// Releases whose behaviour Rudolph adapts to.
var (
	// SantaVersionSilentGUITTYPolicies is the first release that knows SILENT_GUI_BLOCKLIST and SILENT_TTY_BLOCKLIST;
	// older ones skip a rule with an unknown policy.
	SantaVersionSilentGUITTYPolicies = SantaVersion{Year: 2026, Month: 6}
	// SantaVersionEmptyRegexClears is the first release that clears a path regex when it receives an empty string;
	// older ones compile it as received, so "" matches every path.
	SantaVersionEmptyRegexClears = SantaVersion{Year: 2026, Month: 8}
)

// ParseSantaVersion parses the version a Santa client reports. An empty or unparseable version is current (the zero
// SantaVersion).
func ParseSantaVersion(s string) SantaVersion {
	s = strings.TrimSpace(s)
	// Build metadata or a pre-release suffix, e.g. "2026.9.1-beta (1234)", does not change the release
	if i := strings.IndexAny(s, " -+("); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return SantaVersion{}
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 1 {
		return SantaVersion{}
	}
	month, err := strconv.Atoi(parts[1])
	if err != nil || month < 0 {
		return SantaVersion{}
	}
	for _, part := range parts[2:] {
		if _, err := strconv.Atoi(part); err != nil {
			return SantaVersion{}
		}
	}
	return SantaVersion{Year: year, Month: month}
}

// IsCurrent reports whether this stands for the current release (an empty or unparseable version).
func (v SantaVersion) IsCurrent() bool {
	return v == SantaVersion{}
}

// Before reports whether v is a release older than other. The current release is older than none.
func (v SantaVersion) Before(other SantaVersion) bool {
	if v.IsCurrent() {
		return false
	}
	if other.IsCurrent() {
		return true
	}
	if v.Year != other.Year {
		return v.Year < other.Year
	}
	return v.Month < other.Month
}

// AtLeast reports whether v is other or a newer release.
func (v SantaVersion) AtLeast(other SantaVersion) bool {
	return !v.Before(other)
}

// KnowsSilentGUITTYPolicies reports whether the client applies SILENT_GUI_BLOCKLIST and SILENT_TTY_BLOCKLIST rules.
// Releases older than 2025.12 are not supported, but skip unknown policies like it, so they are treated the same.
func (v SantaVersion) KnowsSilentGUITTYPolicies() bool {
	return v.AtLeast(SantaVersionSilentGUITTYPolicies)
}

// ClearsRegexWhenEmpty reports whether the client clears a path regex it receives as an empty string. Releases older
// than 2025.12 are not supported, but compile a regex as received like it, so they are treated the same.
func (v SantaVersion) ClearsRegexWhenEmpty() bool {
	return v.AtLeast(SantaVersionEmptyRegexClears)
}

// WithoutSilentGUITTY returns the policy to send a client that predates SILENT_GUI_BLOCKLIST and
// SILENT_TTY_BLOCKLIST: such a client skips a rule with a policy it does not know, so the binary would not be blocked
// at all. SILENT_BLOCKLIST blocks it, without the notification the policy would have kept.
func (p Policy) WithoutSilentGUITTY() Policy {
	switch p {
	case RulePolicySilentGUIBlocklist, RulePolicySilentTTYBlocklist:
		return RulePolicySilentBlocklist
	default:
		return p
	}
}
