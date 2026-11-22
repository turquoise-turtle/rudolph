package machineconfiguration

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// IntendedClientMode must name the mode preflight sends: preflight applies the group's mode, then the temporary one,
// then drops a mode Santa cannot use (handler.go). Whenever that sequence sends a mode, the two agree.
func TestIntendedClientMode_AgreesWithPreflight(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	n := func(v string) awstypes.AttributeValue { return &awstypes.AttributeValueMemberN{Value: v} }
	globals := map[string]*MachineConfiguration{
		"none":     nil,
		"monitor":  {ClientMode: types.Monitor},
		"lockdown": {ClientMode: types.Lockdown},
	}
	machines := map[string]map[string]awstypes.AttributeValue{
		"none":             nil,
		"own standalone":   {"ClientMode": n("3")},
		"other settings":   {"BatchSize": n("20")},
		"temporary":        {"TemporaryClientMode": n("2"), "TemporaryClientModeExpiresAt": n("1795352400")},
		"temporary ended":  {"ClientMode": n("1"), "TemporaryClientMode": n("2"), "TemporaryClientModeExpiresAt": n("1795000000")},
		"temporary on own": {"ClientMode": n("1"), "TemporaryClientMode": n("3"), "TemporaryClientModeExpiresAt": n("1795352400")},
	}
	groupModes := map[string]types.ClientMode{"none": 0, "monitor": types.Monitor, "lockdown": types.Lockdown}
	for gName, global := range globals {
		for mName, machineItem := range machines {
			for grName, groupMode := range groupModes {
				config, own, err := IntendedFromStored(global, machineItem)
				if err != nil {
					t.Fatal(err)
				}
				// Preflight's sequence
				sent := config
				if _, temporary := sent.TemporaryClientModeAt(now); !temporary && !own && groupMode != 0 {
					sent = ApplyGroupClientMode(sent, false, groupMode)
				}
				sent = SanitizeForSanta(ApplyTemporaryClientMode(sent, now))

				got, source := IntendedClientMode(config, own, global, groupMode, groupMode != 0, now)
				if got != sent.ClientMode {
					t.Errorf("global %s, machine %s, group %s: intended %d (from %s), preflight sends %d", gName, mName, grName, got, source, sent.ClientMode)
				}
			}
		}
	}
}

func TestIntendedClientMode_SkipsAStoredModeThatIsNotOne(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	global := &MachineConfiguration{ClientMode: types.Lockdown}
	config, own, err := IntendedFromStored(global, map[string]awstypes.AttributeValue{"ClientMode": &awstypes.AttributeValueMemberN{Value: "9"}})
	if err != nil {
		t.Fatal(err)
	}
	if mode, source := IntendedClientMode(config, own, global, 0, false, now); mode != types.Lockdown || source != ClientModeFromGlobal {
		t.Errorf("a mistyped own mode: intended %d from %s, want the global LOCKDOWN", mode, source)
	}
	if mode, source := IntendedClientMode(config, own, &MachineConfiguration{ClientMode: 7}, 0, false, now); mode != types.Monitor || source != ClientModeFromDefault {
		t.Errorf("a mistyped global mode too: intended %d from %s, want the default MONITOR", mode, source)
	}
}
