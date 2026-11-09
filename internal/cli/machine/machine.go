// Package machine holds `rudolph machine show` and `rudolph machines`: what a Mac last reported and how Rudolph is
// set up for it, from the command line; and `rudolph machine forget`, which deletes it all for a retired Mac.
package machine

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/airbnb/rudolph/internal/cli/clitime"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

// MachineCmd is the `machine` command; it only holds the subcommands
var MachineCmd = &cobra.Command{
	Use:   "machine",
	Short: "Show what a Mac reported when it last synced and how Rudolph is set up for it, or forget a retired Mac",
}

func init() {
	var machineID string
	showCmd := &cobra.Command{
		Use:   "show -m <machine-id>",
		Short: "Show what a Mac reported at its last sync, the configuration Rudolph sends it and its groups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := Client(cmd)
			if err != nil {
				return err
			}
			id, err := flags.ResolveMachineID(client, machineID)
			if err != nil {
				return err
			}
			summary, err := Load(client, id)
			if err != nil {
				return err
			}
			if summary.Sensor == nil {
				return fmt.Errorf("no machine with ID %s has synced (or its data expired, 90 days after its last sync)", id)
			}
			summary.Print(os.Stdout, time.Now())
			return nil
		},
	}
	showCmd.Flags().StringVarP(&machineID, "machine", "m", "", "The Mac's machine UUID")
	_ = showCmd.MarkFlagRequired("machine")
	MachineCmd.AddCommand(showCmd)
}

// Client returns a DynamoDB client of the table and region the command's flags name
func Client(cmd *cobra.Command) (dynamodb.DynamoDBClient, error) {
	region, _ := cmd.Flags().GetString("region")
	table, _ := cmd.Flags().GetString("dynamodb_table")
	return dynamodb.GetClient(table, region)
}

// Summary is what Rudolph knows about a Mac
type Summary struct {
	MachineID string
	// Sensor is what the Mac reported at its last preflight; nil when it has not synced (or its row expired)
	Sensor *sensordata.SensorData
	// Sync is the Mac's sync state; nil when there is none
	Sync *syncstate.SyncStateRow
	// Config is the configuration Rudolph sends the Mac, before its temporary client mode is applied
	Config     machineconfiguration.MachineConfiguration
	Registry   *groups.Registry
	Membership *groups.MachineGroups
}

// Load reads what Rudolph knows about a Mac, under the machine ID exactly as given (see flags.ResolveMachineID)
func Load(client dynamodb.DynamoDBClient, machineID string) (*Summary, error) {
	s := &Summary{MachineID: machineID}
	var err error
	if s.Sensor, err = sensordata.GetSensorData(client, machineID); err != nil {
		return nil, fmt.Errorf("failed to read the machine's sensor data: %w", err)
	}
	if s.Sync, err = syncstate.GetByMachineID(client, machineID); err != nil {
		return nil, fmt.Errorf("failed to read the machine's sync state: %w", err)
	}
	service := machineconfiguration.GetMachineConfigurationService(client, clock.ConcreteTimeProvider{})
	if s.Config, err = service.GetIntendedConfig(machineID); err != nil {
		return nil, fmt.Errorf("failed to read the machine's configuration: %w", err)
	}
	if s.Registry, err = groups.GetRegistry(client, false); err != nil {
		return nil, err
	}
	if s.Membership, err = groups.GetMachineGroups(client, machineID, false); err != nil {
		return nil, err
	}
	return s, nil
}

// Print writes the summary as a readable block
func (s *Summary) Print(w io.Writer, now time.Time) {
	fmt.Fprintf(w, "Machine ID:        %s\n", s.MachineID)
	if sd := s.Sensor; sd != nil {
		fmt.Fprintf(w, "Hostname:          %s\n", orNone(sd.Hostname))
		fmt.Fprintf(w, "Serial number:     %s\n", orNone(sd.SerialNum))
		fmt.Fprintf(w, "Primary user:      %s\n", orNone(sd.PrimaryUser))
		fmt.Fprintf(w, "Model:             %s\n", orNone(sd.ModelIdentifier))
		fmt.Fprintf(w, "macOS:             %s (%s)\n", orNone(sd.OSVersion), orNone(sd.OSBuild))
		fmt.Fprintf(w, "Santa:             %s\n", orNone(sd.SantaVersion))
		fmt.Fprintf(w, "Reported mode:     %s (the client mode the Mac was in at its last sync)\n", modeName(sd.ClientMode))
		fmt.Fprintf(w, "Last sync:         %s\n", timeText(sd.Time))
		if st := s.Sync; st != nil {
			if st.PostflightAt != "" {
				fmt.Fprintf(w, "Last completed:    %s (postflight)\n", timeText(st.PostflightAt))
			}
			if st.LastCleanSync != "" {
				fmt.Fprintf(w, "Last clean sync:   %s\n", timeText(st.LastCleanSync))
			}
		}
		fmt.Fprintf(w, "Rules on the Mac:  %d (binary %d, certificate %d, Team ID %d, signing ID %d, CDHash %d; compiler %d, transitive %d)\n",
			sd.RuleCount, sd.BinaryRuleCount, sd.CertificateRuleCount, sd.TeamIDRuleCount, sd.SigningIDRuleCount, sd.CDHashRuleCount,
			sd.CompilerRuleCount, sd.TransitiveRuleCount)
		if v := types.ParseSantaVersion(sd.SantaVersion); v.Before(types.SantaVersionEmptyRegexClears) {
			fmt.Fprintf(w, "Legacy client:     Santa %s is older than 2026.8, the oldest release Rudolph fully supports; see \"Supported Santa releases\" in docs/sync.md\n", sd.SantaVersion)
		}
	} else {
		fmt.Fprintln(w, "Last sync:         never (no machine with this ID has synced, or its data expired)")
	}

	fmt.Fprintln(w, "Configuration Rudolph sends:")
	effective := machineconfiguration.ApplyTemporaryClientMode(s.Config, now)
	fmt.Fprintf(w, "  Client mode:     %s\n", modeName(effective.ClientMode))
	if s.Config.TemporaryClientMode != 0 || s.Config.TemporaryClientModeExpiresAt != 0 {
		mode := modeName(s.Config.TemporaryClientMode)
		end := s.Config.TemporaryClientModeEnd()
		switch _, active := s.Config.TemporaryClientModeAt(now); {
		case active:
			fmt.Fprintf(w, "  Temporary mode:  %s until %s; then %s\n", mode, clitime.Expiry(end, now), modeName(s.Config.ClientMode))
		case s.Config.TemporaryClientModeExpiresAt <= 0:
			fmt.Fprintf(w, "  Temporary mode:  %s without an end (ignored)\n", mode)
		case end.After(now):
			// Rudolph caps temporary modes at 30 days, also when reading them, so a later end (other tooling, or
			// milliseconds written as seconds) is ignored rather than in force.
			fmt.Fprintf(w, "  Temporary mode:  %s until %s (ignored: more than 30 days away)\n", mode, clitime.Expiry(end, now))
		default:
			fmt.Fprintf(w, "  Temporary mode:  %s, ended %s (ignored)\n", mode, clitime.Since(end, now))
		}
	} else {
		fmt.Fprintln(w, "  Temporary mode:  none")
	}
	if s.Config.CleanSync {
		fmt.Fprintln(w, "  Clean sync:      requested")
	}

	if s.Registry != nil && s.Membership != nil {
		fmt.Fprintln(w, "Groups:")
		fmt.Fprintf(w, "  Assigned:        %s\n", list(s.Membership.Assigned))
		fmt.Fprintf(w, "  Reported:        %s\n", list(s.Membership.Reported))
		effective := groups.Effective(s.Registry, s.Membership)
		if len(effective) == 1 && effective[0] == groups.Ungrouped {
			fmt.Fprintln(w, "  Effective:       ungrouped (gets the --ungrouped rules)")
		} else {
			fmt.Fprintf(w, "  Effective:       %s\n", list(effective))
		}
	}
}

func modeName(mode types.ClientMode) string {
	text, err := mode.MarshalText()
	if err != nil {
		return fmt.Sprintf("%d (unknown)", mode)
	}
	return string(text)
}

// timeText shows a stored RFC 3339 time in UTC and in local time, or as stored when it cannot be parsed
func timeText(stored string) string {
	t, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		return orNone(stored)
	}
	return clitime.Since(t, time.Now())
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none reported)"
	}
	return s
}

func list(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}
