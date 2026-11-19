//go:build integration

package config

// From the stored-XSS review (2026-11-19): `config get` and `config update --inherit` print the path regexes, the
// log upload URL and the event detail URL and text with Go's %q, which bypasses termsafe. %q only escapes what
// strconv.IsPrint rejects, and it accepts variation selectors, the combining grapheme joiner, Khmer inherent vowels,
// Hangul fillers and the blank braille pattern, so a value written by another tool (or an earlier version) can hide a
// payload that the admin never sees. Every such character must reach the terminal as a visible escape.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// configHiddenRunes are characters %q leaves raw. They are built from their numbers because written literally they
// would be invisible here too.
var configHiddenRunes = []rune{0xfe00, 0xfe0e, 0xe0100, 0xe01ef, 0x034f, 0x17b4, 0x115f, 0x3164, 0x2800}

// configHidden returns prefix followed by every hidden rune, and the escapes the CLI should show for them.
func configHidden(prefix string) (raw, escapes string) {
	var r, e strings.Builder
	r.WriteString(prefix)
	for _, c := range configHiddenRunes {
		r.WriteRune(c)
		if c > 0xffff {
			fmt.Fprintf(&e, `\U%08x`, c)
		} else {
			fmt.Fprintf(&e, `\u%04x`, c)
		}
	}
	return r.String(), e.String()
}

// configRawHidden reports the first default-ignorable or blank-looking character in s that the CLI should have
// escaped; ZWNJ, ZWJ and the emoji presentation selector are kept by design.
func configRawHidden(s string) (rune, bool) {
	for _, r := range s {
		switch r {
		case 0x200c, 0x200d, 0xfe0f:
			continue
		case 0x115f, 0x1160, 0x3164, 0xffa0, 0x2800:
			return r, true
		}
		if unicode.In(r, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector) {
			return r, true
		}
	}
	return 0, false
}

func configLine(out, label string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), label) {
			return l
		}
	}
	return ""
}

// seedHiddenGlobalConfig writes a valid global configuration through the CLI, then stores the text settings with
// hidden characters directly, as another tool could. It returns the escapes expected for each setting.
func seedHiddenGlobalConfig(t *testing.T) map[string]string {
	t.Helper()
	mustConfigCLI(t, "set", "--global", "-c", "monitor")
	values := map[string]awstypes.AttributeValue{}
	want := map[string]string{}
	for attr, prefix := range map[string]string{
		"BlockedPathRegex": "^/opt/blocked/",
		"AllowedPathRegex": "^/opt/allowed/",
		"UploadLogsUrl":    "https://logs.example.com/",
		"EventDetailURL":   "https://detail.example.com/",
		"EventDetailText":  "Details",
	} {
		raw, escapes := configHidden(prefix)
		values[":"+attr] = &awstypes.AttributeValueMemberS{Value: raw}
		want[attr] = prefix + escapes
	}
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: "GlobalConfig"},
			"SK": &awstypes.AttributeValueMemberS{Value: "Config"},
		},
		UpdateExpression: aws.String("SET BlockedPathRegex = :BlockedPathRegex, AllowedPathRegex = :AllowedPathRegex, " +
			"UploadLogsUrl = :UploadLogsUrl, EventDetailURL = :EventDetailURL, EventDetailText = :EventDetailText"),
		ExpressionAttributeValues: values,
	})
	if err != nil {
		t.Fatal(err)
	}
	return want
}

func TestConfigGet_EscapesHiddenCharactersInStoredTextSettings(t *testing.T) {
	ddblocal.Reset(t)
	want := seedHiddenGlobalConfig(t)
	out := mustConfigCLI(t, "get", "--global")
	if r, ok := configRawHidden(out); ok {
		t.Errorf("config get --global writes U+%04X raw:\n%+q", r, out)
	}
	for label, attr := range map[string]string{
		"BlockedPathRegex:": "BlockedPathRegex",
		"AllowedPathRegex:": "AllowedPathRegex",
		"UploadLogUrl:":     "UploadLogsUrl",
		"EventDetailURL:":   "EventDetailURL",
		"EventDetailText:":  "EventDetailText",
	} {
		if line := configLine(out, label); !strings.Contains(line, want[attr]) {
			t.Errorf("config get --global: %s line %+q, want it to show %+q (the escapes as text)", label, line, want[attr])
		}
	}
}

// A machine that inherits a setting is shown the global value, formatted by inheritedSettingText.
func TestConfigUpdateInherit_EscapesHiddenCharactersInTheGlobalValue(t *testing.T) {
	ddblocal.Reset(t)
	want := seedHiddenGlobalConfig(t)
	mustConfigCLI(t, "update", "--machine", machine, "--blocked-paths", "^/opt/own/", "--allowed-paths", "^/opt/own/", "--event-detail-text", "own")
	out := mustConfigCLI(t, "update", "--machine", machine, "--inherit", "blocked-paths", "--inherit", "allowed-paths",
		"--inherit", "upload-logs", "--inherit", "event-detail-text")
	if r, ok := configRawHidden(out); ok {
		t.Errorf("config update --inherit writes U+%04X raw:\n%+q", r, out)
	}
	for _, attr := range []string{"BlockedPathRegex", "AllowedPathRegex", "UploadLogsUrl", "EventDetailText"} {
		if line := configLine(out, attr+":"); !strings.Contains(line, want[attr]) {
			t.Errorf("config update --inherit: %s line %+q, want it to show %+q (the escapes as text)", attr, line, want[attr])
		}
	}
}
