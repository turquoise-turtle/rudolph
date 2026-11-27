package machineconfiguration_test

// The attributes the fleet job writes with an automatic clean sync request (AutoCleanSyncFor, AutoCleanSyncAt,
// AutoCleanSyncRequest) describe one Mac's request, like CleanSync and CleanSyncRequest: they are never inherited from
// the global config, and a Mac's own are kept in its intended config, where `rudolph machine show` reads them.
//
// CleanSyncRequestedByFleet reports whether the clean sync requested now is the fleet job's: CleanSync is set and
// AutoCleanSyncRequest is the CleanSyncRequest in force. A later manual request replaces CleanSyncRequest, so it is
// then false.

import (
	"testing"

	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
)

func sv(v string) awstypes.AttributeValue { return &awstypes.AttributeValueMemberS{Value: v} }

func TestAutoCleanSyncAttributesAreNotInherited(t *testing.T) {
	global := machineconfiguration.GetUniversalDefaultConfig()
	global.AutoCleanSyncFor = "2026-11-21T16:00:00Z"
	global.AutoCleanSyncAt = "2026-11-22T12:00:00Z"
	global.AutoCleanSyncRequest = "0123456789abcdef"

	for name, item := range map[string]map[string]awstypes.AttributeValue{
		"no machine config": nil,
		"a machine config without them": {
			"PK": sv("Machine#AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"), "SK": sv("Config"), "DataType": sv("MachineConfig"),
			"BatchSize": &awstypes.AttributeValueMemberN{Value: "100"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, _, err := machineconfiguration.IntendedFromStored(&global, item)
			if err != nil {
				t.Fatal(err)
			}
			if config.AutoCleanSyncFor != "" || config.AutoCleanSyncAt != "" || config.AutoCleanSyncRequest != "" {
				t.Errorf("inherited from the global config: AutoCleanSyncFor %q AutoCleanSyncAt %q AutoCleanSyncRequest %q",
					config.AutoCleanSyncFor, config.AutoCleanSyncAt, config.AutoCleanSyncRequest)
			}
		})
	}
}

func TestAutoCleanSyncAttributesOfTheMachineAreKept(t *testing.T) {
	global := machineconfiguration.GetUniversalDefaultConfig()
	global.AutoCleanSyncFor = "global"
	item := map[string]awstypes.AttributeValue{
		"PK": sv("Machine#AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"), "SK": sv("Config"), "DataType": sv("MachineConfig"),
		"CleanSync": &awstypes.AttributeValueMemberBOOL{Value: true}, "CleanSyncRequest": sv("0123456789abcdef"),
		"AutoCleanSyncFor": sv("none"), "AutoCleanSyncAt": sv("2026-11-22T12:00:00Z"), "AutoCleanSyncRequest": sv("0123456789abcdef"),
	}
	config, _, err := machineconfiguration.IntendedFromStored(&global, item)
	if err != nil {
		t.Fatal(err)
	}
	if config.AutoCleanSyncFor != "none" || config.AutoCleanSyncAt != "2026-11-22T12:00:00Z" || config.AutoCleanSyncRequest != "0123456789abcdef" {
		t.Errorf("the machine's own attributes were not kept: %q %q %q", config.AutoCleanSyncFor, config.AutoCleanSyncAt, config.AutoCleanSyncRequest)
	}
	if !config.CleanSyncRequestedByFleet() {
		t.Error("CleanSyncRequestedByFleet false for a pending request the fleet job wrote")
	}
}

func TestCleanSyncRequestedByFleet(t *testing.T) {
	for name, c := range map[string]struct {
		config machineconfiguration.MachineConfiguration
		want   bool
	}{
		"the fleet job's request":   {machineconfiguration.MachineConfiguration{CleanSync: true, CleanSyncRequest: "aaaa", AutoCleanSyncFor: "none", AutoCleanSyncRequest: "aaaa"}, true},
		"a manual request after it": {machineconfiguration.MachineConfiguration{CleanSync: true, CleanSyncRequest: "bbbb", AutoCleanSyncFor: "none", AutoCleanSyncRequest: "aaaa"}, false},
		"a manual request alone":    {machineconfiguration.MachineConfiguration{CleanSync: true, CleanSyncRequest: "bbbb"}, false},
		"performed":                 {machineconfiguration.MachineConfiguration{AutoCleanSyncFor: "none", AutoCleanSyncRequest: "aaaa"}, false},
		// A flag stored without an identifier (by hand, or an older version) is not the fleet job's.
		"no identifiers": {machineconfiguration.MachineConfiguration{CleanSync: true}, false},
		"nothing":        {machineconfiguration.MachineConfiguration{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.config.CleanSyncRequestedByFleet(); got != c.want {
				t.Errorf("CleanSyncRequestedByFleet() = %v, want %v", got, c.want)
			}
		})
	}
}
