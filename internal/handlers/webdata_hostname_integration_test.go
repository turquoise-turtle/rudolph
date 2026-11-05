//go:build integration

package handlers

// R1 and R2 of rudolph-web's docs/data-layer-design.md: the web view finds Macs by hostname prefix through a
// GSI keyed on HostnameKey. DynamoDB rejects a PutItem whose GSI key attribute is an empty string, so a Mac that
// reports no hostname must store neither attribute, or every preflight from it would fail once the index exists.

import (
	"testing"

	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func sensorString(t *testing.T, item map[string]awstypes.AttributeValue, attr string) (string, bool) {
	t.Helper()
	v, ok := item[attr]
	if !ok {
		return "", false
	}
	s, isString := v.(*awstypes.AttributeValueMemberS)
	if !isString {
		t.Fatalf("%s is stored as %T, want a string", attr, v)
	}
	return s.Value, true
}

func TestR1_EmptyHostnameIsNotStored(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.Hostname = ""
	host.MustSync(t)

	item := getItem(t, "Machine#"+machineA, "Current")
	if len(item) == 0 {
		t.Fatal("no sensor data stored")
	}
	for _, attr := range []string{"Hostname", "HostnameKey"} {
		if v, ok := item[attr]; ok {
			t.Errorf("%s is stored for a Mac that reported no hostname: %#v", attr, v)
		}
	}
}

func TestR2_HostnameKeyIsTheTrimmedLowerCaseHostname(t *testing.T) {
	for reported, want := range map[string]string{
		"mac-1.example.com":         "mac-1.example.com",
		"Jane's-MacBook-Pro.local":  "jane's-macbook-pro.local",
		"  BUILD-07.Corp.Example  ": "build-07.corp.example",
		"MÜNCHEN-Studio":            "münchen-studio",
		"\tmac-tab.example.com\n":   "mac-tab.example.com",
	} {
		t.Run(reported, func(t *testing.T) {
			transport := freshServer(t)
			host := newHost(t, machineA, transport)
			host.Hostname = reported
			host.MustSync(t)

			item := getItem(t, "Machine#"+machineA, "Current")
			got, ok := sensorString(t, item, "HostnameKey")
			if !ok || got != want {
				t.Errorf("HostnameKey = %q (present %v), want %q", got, ok, want)
			}
			// The web view shows the hostname as the Mac reported it.
			if got, _ := sensorString(t, item, "Hostname"); got != reported {
				t.Errorf("Hostname = %q, want it stored as reported, %q", got, reported)
			}
		})
	}
}

func TestR2_BlankHostnameHasNoHostnameKey(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.Hostname = "   "
	host.MustSync(t)

	item := getItem(t, "Machine#"+machineA, "Current")
	if len(item) == 0 {
		t.Fatal("no sensor data stored")
	}
	if v, ok := item["HostnameKey"]; ok {
		t.Errorf("HostnameKey is stored for a blank hostname: %#v", v)
	}
}

// A Mac whose hostname later goes missing must not keep its old key, or a search would still find it by a name
// it no longer reports.
func TestR2_HostnameKeyFollowsTheLatestPreflight(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.Hostname = "Old-Name.example.com"
	host.MustSync(t)

	host.Hostname = "New-Name.example.com"
	host.MustSync(t)
	item := getItem(t, "Machine#"+machineA, "Current")
	if got, _ := sensorString(t, item, "HostnameKey"); got != "new-name.example.com" {
		t.Fatalf("HostnameKey = %q after a rename, want %q", got, "new-name.example.com")
	}

	host.Hostname = ""
	host.MustSync(t)
	item = getItem(t, "Machine#"+machineA, "Current")
	for _, attr := range []string{"Hostname", "HostnameKey"} {
		if v, ok := item[attr]; ok {
			t.Errorf("%s is still stored after the Mac stopped reporting a hostname: %#v", attr, v)
		}
	}
}
