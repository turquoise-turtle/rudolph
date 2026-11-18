//go:build integration

package machine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// What a Mac reports (host name, serial, user, versions, model) reaches the admin's terminal. Escape sequences in
// it could clear the screen, rewrite earlier lines, set the window title or plant links, and bidi or zero-width
// characters could make it read as something else (stored-XSS review, 2026-11-18). The CLI shows such characters as
// visible Go-style escapes (\x1b, \u202e, \n) instead of writing them.

// hostile is one value per field, each with characters a terminal would act on.
const (
	hostileHost   = "evil\x1b]0;pwned\x07\u202emac\nFAKE: line\r"
	hostileSerial = "C02\u009b31mX\x7f"
	hostileUser   = "j\u200bsmith\u2066"
	hostileSanta  = "2026.9\x1b[2J"
)

// dangerous reports whether r is a rune a terminal acts on or that hides or reorders text. Zero-width (non-)joiners
// are left alone: emoji sequences and several scripts need them.
func dangerous(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false // the CLI's own layout; checked per line below
	case r < 0x20 || (r >= 0x7f && r <= 0x9f):
		return true
	case r == 0x200c || r == 0x200d:
		return false
	case (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2060 && r <= 0x2069) || r == 0xfeff || r == 0x061c || r == 0x00ad:
		return true
	}
	return false
}

func assertNoTerminalControl(t *testing.T, out string) {
	t.Helper()
	for i, r := range out {
		if dangerous(r) {
			t.Errorf("output has %U at byte %d:\n%q", r, i, out)
			return
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAKE:") {
			t.Errorf("a newline in a reported value started a line of its own: %q", line)
		}
	}
}

func seedHostileMac(t *testing.T) {
	t.Helper()
	seedMac(t, macA, hostileHost, hostileSerial, time.Now())
	setSensorField(t, macA, "PrimaryUser", hostileUser)
	setSensorField(t, macA, "SantaVersion", hostileSanta)
}

func TestMachineShow_EscapesTerminalControlInReportedValues(t *testing.T) {
	ddblocal.Reset(t)
	seedHostileMac(t)
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	assertNoTerminalControl(t, out)
	mustContain(t, out, `evil\x1b]0;pwned\x07\u202emac\nFAKE: line\r`, `C02\u009b31mX\x7f`, `j\u200bsmith\u2066`, `2026.9\x1b[2J`)
}

func TestMachines_EscapesTerminalControlInReportedValues(t *testing.T) {
	ddblocal.Reset(t)
	seedHostileMac(t)
	out, err := clitest.Run(t, env(), MachinesCmd, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	assertNoTerminalControl(t, out)
	mustContain(t, out, `\x1b]0;pwned`, `C02\u009b31mX\x7f`, `2026.9\x1b[2J`)
}

// Ordinary non-ASCII text, emoji sequences joined with U+200D included, is shown as it is.
func TestMachineShow_LeavesOrdinaryUnicodeAlone(t *testing.T) {
	ddblocal.Reset(t)
	host := "Jürgen’s Mac 👨\u200d👩\u200d👧 工程"
	seedMac(t, macA, host, "C02ABC123", time.Now())
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out, host)
}

func setSensorField(t *testing.T, id, field, value string) {
	t.Helper()
	pk, sk := sensordata.MachineIDSensorDataPKSK(id)
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName:                 aws.String(ddblocal.TableName),
		Key:                       map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: pk}, "SK": &types.AttributeValueMemberS{Value: sk}},
		UpdateExpression:          aws.String("SET #f = :v"),
		ExpressionAttributeNames:  map[string]string{"#f": field},
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": &types.AttributeValueMemberS{Value: value}},
	})
	if err != nil {
		t.Fatal(err)
	}
}
