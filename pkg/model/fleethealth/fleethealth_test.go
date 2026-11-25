package fleethealth_test

// Editable minimum Santa version (owner decision, 2026-11-25). The fleet job lists a Mac under the health category
// Version when its Santa release is below a minimum. That minimum is part of the fleet health setting, the store-table
// item FleetHealth / Config, as the string attribute MinSantaVersion:
//
//   - a release, anything rudolphtypes.ParseSantaVersion accepts ("2026.9", "2026.10.1"), at most 32 bytes;
//   - or the word "off", which turns the Version category off;
//   - absent: DefaultMinSantaVersion, "2026.9", applies. This constant is the one default: internal/fleet's
//     DefaultMinSantaVersion is only an alias of it, and rudolph-web reads it from here.
//
// ValidateMinSantaVersion(version) (normalised string, error) is the one rule for what may be stored: surrounding
// spaces are trimmed, "off" in any case becomes "off", and a release is kept as typed (after trimming). Anything else
// is refused with an error that names "off" and an example release. SetMinSantaVersion stores only what it accepts,
// FromItem keeps only what it accepts (normalised the same way), and the CLI checks with it before asking to confirm.
//
// Config.MinSantaVersionInForce() (version, off, stored) is what the fleet job and every display use: version is the
// stored value, or DefaultMinSantaVersion when none is stored (or the stored one is unusable); off reports the value
// "off" (version is then "off" too); stored reports whether it came from the item rather than the default.

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/model/fleethealth"
)

func TestDefaultMinSantaVersion(t *testing.T) {
	if fleethealth.DefaultMinSantaVersion != "2026.9" {
		t.Errorf("DefaultMinSantaVersion %q, want 2026.9", fleethealth.DefaultMinSantaVersion)
	}
}

// What may be stored, and how it is normalised.
func TestValidateMinSantaVersion_Accepted(t *testing.T) {
	for in, want := range map[string]string{
		"2026.9":      "2026.9",
		"2026.10":     "2026.10",
		"2026.10.1":   "2026.10.1",
		"2025.12.0":   "2025.12.0",
		" 2026.10 ":   "2026.10",
		"\t2026.11\n": "2026.11",
		"off":         "off",
		"OFF":         "off",
		" Off ":       "off",
		// ParseSantaVersion reads build metadata and pre-release suffixes as the release they belong to.
		"2026.9.1-beta": "2026.9.1-beta",
		// 32 bytes, the most that may be stored
		"2026.10." + strings.Repeat("1", 24): "2026.10." + strings.Repeat("1", 24),
	} {
		got, err := fleethealth.ValidateMinSantaVersion(in)
		if err != nil {
			t.Errorf("ValidateMinSantaVersion(%q) refused: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateMinSantaVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateMinSantaVersion_Refused(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"2026",
		"v2026.9",
		"2026.x",
		".9",
		"0.9",
		"latest",
		"default",
		"none",
		"of",
		"offf",
		"o ff",
		"2026.9.x",
		// 33 bytes
		"2026.10." + strings.Repeat("1", 25),
	} {
		got, err := fleethealth.ValidateMinSantaVersion(in)
		if err == nil {
			t.Errorf("ValidateMinSantaVersion(%q) accepted as %q", in, got)
			continue
		}
		if got != "" {
			t.Errorf("ValidateMinSantaVersion(%q) refused but returned %q, want \"\"", in, got)
		}
		// The error tells the operator what would have been accepted.
		if msg := err.Error(); !strings.Contains(msg, "off") || !strings.Contains(msg, "2026.") {
			t.Errorf("ValidateMinSantaVersion(%q) error %q names neither \"off\" nor an example release such as 2026.10", in, msg)
		}
	}
}

// FromItem decodes MinSantaVersion with the same rule, and drops an unusable value to "" rather than refusing the
// item: the fleet job runs unattended, and a hand-edited bad value must fall back to the default, not stop every run.
// An unusable MinSantaVersion leaves the other attributes as they are.
func TestFromItem_MinSantaVersion(t *testing.T) {
	for name, c := range map[string]struct {
		value types.AttributeValue // nil: attribute absent
		want  string
	}{
		"absent":                 {nil, ""},
		"a release":              {&types.AttributeValueMemberS{Value: "2026.10"}, "2026.10"},
		"a patch release":        {&types.AttributeValueMemberS{Value: "2026.10.1"}, "2026.10.1"},
		"off":                    {&types.AttributeValueMemberS{Value: "off"}, "off"},
		"OFF, edited by hand":    {&types.AttributeValueMemberS{Value: "OFF"}, "off"},
		"spaces, edited by hand": {&types.AttributeValueMemberS{Value: " 2026.10 "}, "2026.10"},
		"empty":                  {&types.AttributeValueMemberS{Value: ""}, ""},
		"not a release":          {&types.AttributeValueMemberS{Value: "garbage"}, ""},
		"a year alone":           {&types.AttributeValueMemberS{Value: "2026"}, ""},
		"too long":               {&types.AttributeValueMemberS{Value: "2026.10." + strings.Repeat("1", 25)}, ""},
		"a number":               {&types.AttributeValueMemberN{Value: "2026.9"}, ""},
		"a BOOL":                 {&types.AttributeValueMemberBOOL{Value: false}, ""},
		"a NULL":                 {&types.AttributeValueMemberNULL{Value: true}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			item := map[string]types.AttributeValue{
				"PK":               &types.AttributeValueMemberS{Value: "FleetHealth"},
				"SK":               &types.AttributeValueMemberS{Value: "Config"},
				"StaleDays":        &types.AttributeValueMemberN{Value: "14"},
				"HolidayStaleDays": &types.AttributeValueMemberN{Value: "60"},
				"HolidayUntil":     &types.AttributeValueMemberS{Value: "2027-02-01"},
			}
			if c.value != nil {
				item["MinSantaVersion"] = c.value
			}
			got := fleethealth.FromItem(item)
			want := fleethealth.Config{StaleDays: 14, HolidayStaleDays: 60, HolidayUntil: "2027-02-01", MinSantaVersion: c.want}
			if got != want {
				t.Errorf("FromItem = %+v, want %+v", got, want)
			}
		})
	}
}

func TestMinSantaVersionInForce(t *testing.T) {
	for name, c := range map[string]struct {
		config      fleethealth.Config
		version     string
		off, stored bool
	}{
		"nothing stored: the default":        {fleethealth.Config{}, "2026.9", false, false},
		"a stale threshold alone":            {fleethealth.Config{StaleDays: 20}, "2026.9", false, false},
		"a stored release":                   {fleethealth.Config{MinSantaVersion: "2026.10"}, "2026.10", false, true},
		"a stored release below the default": {fleethealth.Config{MinSantaVersion: "2026.8"}, "2026.8", false, true},
		"stored off":                         {fleethealth.Config{MinSantaVersion: "off"}, "off", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			version, off, stored := c.config.MinSantaVersionInForce()
			if version != c.version || off != c.off || stored != c.stored {
				t.Errorf("MinSantaVersionInForce() = (%q, off %v, stored %v), want (%q, off %v, stored %v)", version, off, stored, c.version, c.off, c.stored)
			}
		})
	}
	// The default is the constant, not a copy of it.
	if version, _, _ := (fleethealth.Config{}).MinSantaVersionInForce(); version != fleethealth.DefaultMinSantaVersion {
		t.Errorf("the default in force %q is not DefaultMinSantaVersion %q", version, fleethealth.DefaultMinSantaVersion)
	}
}
