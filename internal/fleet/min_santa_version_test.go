package fleet_test

// One source for the minimum Santa version (owner decision, 2026-11-25): the stored setting, FleetHealth / Config's
// MinSantaVersion, else fleethealth.DefaultMinSantaVersion. The job has no option for it: fleet.Options.MinSantaVersion
// is removed rather than kept as a test override, so no caller (cmd/fleet, a test, a future tool) can make a run
// disagree with what the CLI and rudolph-web show as in force. Tests set it the way an operator does, in the store.
// fleet.DefaultMinSantaVersion remains only as an alias of fleethealth.DefaultMinSantaVersion.

import (
	"reflect"
	"testing"

	"github.com/airbnb/rudolph/internal/fleet"
	"github.com/airbnb/rudolph/pkg/model/fleethealth"
)

func TestFleet_OptionsHaveNoMinSantaVersion(t *testing.T) {
	if _, ok := reflect.TypeOf(fleet.Options{}).FieldByName("MinSantaVersion"); ok {
		t.Error("fleet.Options still has MinSantaVersion; the minimum comes only from the store (FleetHealth / Config) or fleethealth.DefaultMinSantaVersion")
	}
}

func TestFleet_DefaultMinSantaVersionIsFleethealths(t *testing.T) {
	if fleet.DefaultMinSantaVersion != fleethealth.DefaultMinSantaVersion || fleethealth.DefaultMinSantaVersion != "2026.9" {
		t.Errorf("fleet.DefaultMinSantaVersion %q, fleethealth.DefaultMinSantaVersion %q: want both 2026.9, the first an alias of the second",
			fleet.DefaultMinSantaVersion, fleethealth.DefaultMinSantaVersion)
	}
}
