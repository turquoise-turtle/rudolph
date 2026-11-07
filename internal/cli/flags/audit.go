package flags

import (
	"fmt"
	"os"
	"os/user"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/airbnb/rudolph/pkg/model/groups"
)

// Actor is who the CLI records as having made a change: "cli:<OS user>@<host name>"
func Actor() string {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" {
		name = "unknown"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("cli:%s@%s", name, host)
}

// Record adds a change the command has just written to the audit trail. Call it only after the write succeeded, so
// that a dry run, a declined prompt or a failed write records nothing. When the entry cannot be recorded the change
// stays (it is not rolled back): Record says so on stderr and returns an error, so that the command exits non-zero.
func Record(client audit.Writer, action, target, summary string) error {
	err := audit.Record(clock.ConcreteTimeProvider{}, client, audit.Entry{Actor: Actor(), Action: action, Target: target, Summary: summary})
	if err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "[WARNING] the change was made but not audited (%s on %s: %s): %v\n", action, target, summary, err)
	return fmt.Errorf("the change was made but not recorded in the audit trail: %w", err)
}

// MachineTarget is the audit target of a machine's rules or configuration
func MachineTarget(machineID string) string {
	return "machine:" + machineID
}

// GroupTarget is the audit target of a device group's rules or settings; the reserved group of Macs in no group is
// "ungrouped"
func GroupTarget(group string) string {
	if group == groups.Ungrouped {
		return "ungrouped"
	}
	return "group:" + group
}

// GlobalTarget is the audit target of global rules and configuration
const GlobalTarget = "global"
