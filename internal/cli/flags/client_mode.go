package flags

import (
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/pkg/types"
)

const (
	monitorMode       = "monitor"
	lockdownMode      = "lockdown"
	standaloneMode    = "standalone"
	defaultClientMode = "monitor"
)

// configMode is a custom type for use as a CLI flag representing the type of config mode being applied
type ClientMode types.ClientMode

func (i *ClientMode) AsClientMode() types.ClientMode {
	return types.ClientMode(*i)
}

func (i *ClientMode) Set(c string) error {
	switch strings.ToLower(c) {
	case monitorMode:
		*i = ClientMode(types.Monitor)
	case lockdownMode:
		*i = ClientMode(types.Lockdown)
	case standaloneMode:
		*i = ClientMode(types.Standalone)
	default:
		return fmt.Errorf(`invalid client mode; must be "monitor", "lockdown" or "standalone"`)
	}
	return nil
}

func (i *ClientMode) Type() string {
	return "string"
}

func (i *ClientMode) String() string {
	c := (types.ClientMode)(*i)
	// select `monitor`, `lockdown` or `standalone` and if invalid, always default to `defaultClientMode` ClientMode
	switch c {
	case types.Monitor:
		return monitorMode
	case types.Lockdown:
		return lockdownMode
	case types.Standalone:
		return standaloneMode
	default:
		return defaultClientMode
	}
}
