//go:build integration

package config

// The MonitorEventsWithoutRules setting (internal/handlers/zero_rules_integration_test.go explains it): set globally
// or per machine with --monitor-events-without-rules, and returned to the global value with --inherit.

import (
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func boolAttr(row map[string]awstypes.AttributeValue, attr string) (value, present bool) {
	v, ok := row[attr].(*awstypes.AttributeValueMemberBOOL)
	if !ok {
		return false, false
	}
	return v.Value, true
}

func TestZeroRules_ConfigFlag(t *testing.T) {
	ddblocal.Reset(t)
	run := func(args ...string) {
		t.Helper()
		if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	run("set", "--global", "-c", "monitor", "--monitor-events-without-rules=false")
	if v, ok := boolAttr(configRow(t, "GlobalConfig"), "MonitorEventsWithoutRules"); !ok || v {
		t.Errorf("global MonitorEventsWithoutRules = %v (present %v), want false", v, ok)
	}
	run("update", "--global", "--monitor-events-without-rules=true")
	if v, ok := boolAttr(configRow(t, "GlobalConfig"), "MonitorEventsWithoutRules"); !ok || !v {
		t.Errorf("after update: global MonitorEventsWithoutRules = %v (present %v), want true", v, ok)
	}

	run("update", "--machine", machine, "--monitor-events-without-rules=false")
	if v, ok := boolAttr(configRow(t, "Machine#"+machine), "MonitorEventsWithoutRules"); !ok || v {
		t.Errorf("machine MonitorEventsWithoutRules = %v (present %v), want false", v, ok)
	}
	run("update", "--machine", machine, "--inherit", "monitor-events-without-rules")
	if _, ok := configRow(t, "Machine#"+machine)["MonitorEventsWithoutRules"]; ok {
		t.Error("--inherit monitor-events-without-rules left the machine override in place")
	}

	// A config set without the flag leaves the setting unset: it then means on.
	ddblocal.Reset(t)
	run("set", "--global", "-c", "monitor")
	if _, ok := configRow(t, "GlobalConfig")["MonitorEventsWithoutRules"]; ok {
		t.Error("config set without the flag stored MonitorEventsWithoutRules")
	}
}
