package types

import "fmt"

// ClientMode specifies which mode the Santa client will evaluate rules in.
type ClientMode int

const (
	Monitor ClientMode = iota + 1
	Lockdown
	// Standalone is lockdown mode in which the user may approve unknown binaries locally (e.g. with Touch ID).
	Standalone
)

// UnmarshalText yes
func (c *ClientMode) UnmarshalText(text []byte) error {
	switch mode := string(text); mode {
	case "MONITOR":
		*c = Monitor
	case "LOCKDOWN":
		*c = Lockdown
	case "STANDALONE":
		*c = Standalone
	default:
		return fmt.Errorf("unknown client_mode value %q", mode)
	}
	return nil
}

// MarshalText yes
func (c ClientMode) MarshalText() ([]byte, error) {
	switch c {
	case Monitor:
		return []byte("MONITOR"), nil
	case Lockdown:
		return []byte("LOCKDOWN"), nil
	case Standalone:
		return []byte("STANDALONE"), nil
	default:
		return nil, fmt.Errorf("unknown client_mode %d", c)
	}
}
