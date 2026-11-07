package config

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
)

// auditTarget is the audit trail's name of the configuration a command changes
func auditTarget(tf flags.TargetFlags, machineID string) string {
	if tf.IsGlobal {
		return flags.GlobalTarget
	}
	return flags.MachineTarget(machineID)
}

// updateSummary lists the settings a config update changes, e.g. "ClientMode=LOCKDOWN, BatchSize=50"
func updateSummary(request machineconfiguration.MachineConfigurationUpdateRequest) string {
	var changes []string
	value := reflect.ValueOf(request)
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() {
			continue
		}
		name := value.Type().Field(i).Name
		var text string
		switch v := field.Elem().Interface().(type) {
		case types.ClientMode:
			text = clientModeName(v)
		case types.FileAccessAction:
			text = string(v)
		case []string:
			text = strings.Join(v, ",")
		case string:
			if urlSettings[name] {
				v = redactURL(v)
			}
			text = fmt.Sprintf("%q", v)
		default:
			text = fmt.Sprint(v)
		}
		changes = append(changes, name+"="+text)
	}
	for _, setting := range request.Inherit {
		changes = append(changes, setting+" inherited")
	}
	return strings.Join(changes, ", ")
}

// urlSettings are the settings holding URLs, which redactURL shortens in audit entries
var urlSettings = map[string]bool{"UploadLogsURL": true, "EventDetailURL": true}

// redactURL keeps the scheme, host and path of a URL. Audit entries never expire, so a token in a query string,
// fragment or user information must not end up in one. EventDetailURL may hold Santa's %placeholders%, which are not
// valid URL escapes, so the query and fragment are cut off before parsing.
func redactURL(raw string) string {
	cut := raw
	if i := strings.IndexAny(cut, "?#"); i >= 0 {
		cut = cut[:i]
	}
	if u, err := url.Parse(cut); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	if scheme, rest, ok := strings.Cut(cut, "://"); ok {
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			return scheme + "://" + rest[at+1:]
		}
	}
	return cut
}
