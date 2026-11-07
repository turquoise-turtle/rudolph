package config

import (
	"fmt"
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
		var text string
		switch v := field.Elem().Interface().(type) {
		case types.ClientMode:
			text = clientModeName(v)
		case []string:
			text = strings.Join(v, ",")
		case string:
			text = fmt.Sprintf("%q", v)
		default:
			text = fmt.Sprint(v)
		}
		changes = append(changes, value.Type().Field(i).Name+"="+text)
	}
	for _, setting := range request.Inherit {
		changes = append(changes, setting+" inherited")
	}
	return strings.Join(changes, ", ")
}
