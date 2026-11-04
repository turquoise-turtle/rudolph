package machineconfiguration

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrGlobalInherit is returned when settings are to be inherited by the global config, which has nothing to inherit
// from.
var ErrGlobalInherit = errors.New("only a machine-specific config can inherit settings from the global config (--machine)")

// inheritableSettings maps the attribute name of every setting a machine config can override, and so inherit again,
// to a function that resets it in a config.
var inheritableSettings = map[string]func(*MachineConfiguration){
	"ClientMode":                func(c *MachineConfiguration) { c.ClientMode = 0 },
	"BlockedPathRegex":          func(c *MachineConfiguration) { c.BlockedPathRegex = "" },
	"AllowedPathRegex":          func(c *MachineConfiguration) { c.AllowedPathRegex = "" },
	"BatchSize":                 func(c *MachineConfiguration) { c.BatchSize = 0 },
	"EnableBundles":             func(c *MachineConfiguration) { c.EnableBundles = false },
	"EnableTransitiveRules":     func(c *MachineConfiguration) { c.EnabledTransitiveRules = false },
	"FullSyncInterval":          func(c *MachineConfiguration) { c.FullSyncInterval = 0 },
	"UploadLogsUrl":             func(c *MachineConfiguration) { c.UploadLogsURL = "" },
	"EnableAllEventUpload":      func(c *MachineConfiguration) { c.EnableAllEventUpload = nil },
	"DisableUnknownEventUpload": func(c *MachineConfiguration) { c.DisableUnknownEventUpload = nil },
	"OverrideFileAccessAction":  func(c *MachineConfiguration) { c.OverrideFileAccessAction = "" },
	"BlockUsbMount":             func(c *MachineConfiguration) { c.BlockUsbMount = nil },
	remountUsbModeAttribute:     func(c *MachineConfiguration) { c.RemountUsbMode = nil },
	"EventDetailURL":            func(c *MachineConfiguration) { c.EventDetailURL = nil },
	"EventDetailText":           func(c *MachineConfiguration) { c.EventDetailText = nil },
}

// InheritableSettings returns the attribute names of the settings a machine config can inherit, sorted
func InheritableSettings() []string {
	names := make([]string, 0, len(inheritableSettings))
	for name := range inheritableSettings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateInherit checks the settings an update is to inherit: known settings, only for a machine config, and none
// that the same update also sets.
func validateInherit(request MachineConfigurationUpdateRequest, global bool) error {
	if len(request.Inherit) == 0 {
		return nil
	}
	if global {
		return ErrGlobalInherit
	}
	_, set := applyUpdateRequest(MachineConfiguration{}, request, true)
	for _, name := range request.Inherit {
		if _, ok := inheritableSettings[name]; !ok {
			return fmt.Errorf("unknown setting %q to inherit; must be one of %s", name, strings.Join(InheritableSettings(), ", "))
		}
		if _, ok := set[name]; ok {
			return fmt.Errorf("setting %s cannot be both set and inherited", name)
		}
	}
	return nil
}

// withoutSettings returns the config with the named settings reset, as a config row without them decodes
func withoutSettings(config MachineConfiguration, names []string) MachineConfiguration {
	for _, name := range names {
		if reset, ok := inheritableSettings[name]; ok {
			reset(&config)
		}
	}
	return config
}
