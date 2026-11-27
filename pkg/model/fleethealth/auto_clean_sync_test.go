package fleethealth_test

// Automatic clean syncs for rules hash outliers (internal/fleet) are on unless turned off. The setting is the string
// attribute AutoCleanSync of FleetHealth / Config: only "off" is stored (AutoCleanSyncOff); absent means on. FromItem
// reads "off" in any case, with surrounding spaces, as off, and anything else (another word, a number, a BOOL) as on,
// the default: the fleet job runs unattended, and a hand-edited value must not stop it. Config.AutoCleanSyncOn()
// reports whether they are on.

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/model/fleethealth"
)

func TestAutoCleanSyncOff(t *testing.T) {
	if fleethealth.AutoCleanSyncOff != "off" {
		t.Errorf("AutoCleanSyncOff %q, want off", fleethealth.AutoCleanSyncOff)
	}
}

func TestFromItem_AutoCleanSync(t *testing.T) {
	for name, c := range map[string]struct {
		value  types.AttributeValue // nil: attribute absent
		stored string
		on     bool
	}{
		"absent":                 {nil, "", true},
		"off":                    {&types.AttributeValueMemberS{Value: "off"}, "off", false},
		"OFF, edited by hand":    {&types.AttributeValueMemberS{Value: "OFF"}, "off", false},
		"spaces, edited by hand": {&types.AttributeValueMemberS{Value: " off "}, "off", false},
		"on, edited by hand":     {&types.AttributeValueMemberS{Value: "on"}, "", true},
		"another word":           {&types.AttributeValueMemberS{Value: "no"}, "", true},
		"empty":                  {&types.AttributeValueMemberS{Value: ""}, "", true},
		"a BOOL":                 {&types.AttributeValueMemberBOOL{Value: false}, "", true},
		"a number":               {&types.AttributeValueMemberN{Value: "0"}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			item := map[string]types.AttributeValue{
				"PK":              &types.AttributeValueMemberS{Value: "FleetHealth"},
				"SK":              &types.AttributeValueMemberS{Value: "Config"},
				"StaleDays":       &types.AttributeValueMemberN{Value: "14"},
				"MinSantaVersion": &types.AttributeValueMemberS{Value: "2026.10"},
			}
			if c.value != nil {
				item["AutoCleanSync"] = c.value
			}
			got := fleethealth.FromItem(item)
			want := fleethealth.Config{StaleDays: 14, MinSantaVersion: "2026.10", AutoCleanSync: c.stored}
			if got != want {
				t.Errorf("FromItem = %+v, want %+v", got, want)
			}
			if got.AutoCleanSyncOn() != c.on {
				t.Errorf("AutoCleanSyncOn() = %v, want %v", got.AutoCleanSyncOn(), c.on)
			}
		})
	}
	if !(fleethealth.Config{}).AutoCleanSyncOn() {
		t.Error("automatic clean syncs are off with nothing stored; they are on by default")
	}
}
