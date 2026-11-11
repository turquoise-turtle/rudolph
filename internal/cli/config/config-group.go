package config

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const flagConfirmGroupLockdown = "confirm-group-lockdown"

// groupUpdateFlags are the flags `config update --group` accepts: a group has a client mode and no other setting
var groupUpdateFlags = map[string]bool{
	"group": true, "ungrouped": true, "client-mode": true, flagConfirmGroupLockdown: true, flagInherit: true,
	"yes": true, "dry-run": true, "region": true, "dynamodb_table": true, "help": true,
}

// groupClientModeRequest is a validated `config update --group|--ungrouped`: mode is nil to remove the group's mode
type groupClientModeRequest struct {
	group string
	mode  *types.ClientMode
}

// parseGroupClientModeRequest checks a config update for a group before connecting to the sync server
func parseGroupClientModeRequest(cmd *cobra.Command, tf *flags.TargetFlags, clientMode optionalClientMode, inherit []string, confirmGroupLockdown bool) (groupClientModeRequest, error) {
	if err := tf.RequireExplicitTarget(); err != nil {
		return groupClientModeRequest{}, err
	}
	var refused []string
	// VisitAll and Changed rather than Visit: Visit also lists flags given in an earlier execution of the same
	// command (tests run the commands repeatedly)
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed && !groupUpdateFlags[f.Name] {
			refused = append(refused, "--"+f.Name)
		}
	})
	if len(refused) > 0 {
		return groupClientModeRequest{}, fmt.Errorf("a device group has only a client mode (-c, or --%s client-mode); %v cannot be set for a group", flagInherit, refused)
	}
	request := groupClientModeRequest{group: tf.Group}
	settings, err := inheritedSettings(inherit)
	if err != nil {
		return request, err
	}
	for _, setting := range settings {
		if setting != "ClientMode" {
			return request, fmt.Errorf("a device group has only a client mode; --%s takes only client-mode for a group", flagInherit)
		}
	}
	switch {
	case clientMode.set && len(settings) > 0:
		return request, errors.New("the client mode cannot be both set and inherited")
	case clientMode.set:
		mode := clientMode.mode.AsClientMode()
		request.mode = &mode
		if machineconfiguration.RequiresGlobalLockdownConfirmation(mode) && !confirmGroupLockdown {
			return request, fmt.Errorf("%s blocks unknown executions on every Mac of the group; pass --%s to confirm", clientModeName(mode), flagConfirmGroupLockdown)
		}
	case len(settings) == 0:
		return request, fmt.Errorf("no client mode to update was given: pass -c <mode>, or --%s client-mode to remove the group's", flagInherit)
	}
	return request, nil
}

// groupConfigWriter is what changing a group's client mode needs
type groupConfigWriter interface {
	groups.RegistryWriter
	audit.Writer
}

// updateGroupClientMode sets or removes a group's client mode. The reserved group of Macs in no group is registered
// when its mode is first set, as when it gets its first rule.
func updateGroupClientMode(client groupConfigWriter, request groupClientModeRequest, approval flags.Approval) error {
	registry, err := groups.GetRegistry(client, true)
	if err != nil {
		return err
	}
	entry, registered := registry.Groups[request.group]
	if !registered && request.group != groups.Ungrouped {
		return fmt.Errorf("group %q: %w; create it first with rudolph group create", request.group, groups.ErrGroupNotFound)
	}
	if !registered && request.mode == nil {
		return errors.New("the Macs in no group have no client mode of their own to remove")
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 8, 0, '\t', tabwriter.AlignRight)
	fmt.Println("Setting the following configuration")
	fmt.Println()
	fmt.Fprintln(writer, "Config\t Setting")
	fmt.Fprintln(writer, "Group:\t", groupText(request.group))
	if current, ok := entry.ClientModeOf(); ok {
		fmt.Fprintln(writer, "Current ClientMode:\t", clientModeName(current))
	}
	if request.mode != nil {
		fmt.Fprintln(writer, "ClientMode:\t", *request.mode, "-->(", clientModeName(*request.mode), ")")
	} else {
		fmt.Fprintln(writer, "ClientMode:\t", "(none: the Macs follow their own or the global client mode)")
	}
	writer.Flush()
	fmt.Println("\nA Mac's own or temporary client mode beats its groups'; between its groups, the strictest mode wins.")
	fmt.Println()
	if apply, err := approval.Confirm(); !apply {
		return err
	}

	if !registered {
		err := groups.CreateGroup(clock.ConcreteTimeProvider{}, client, groups.Ungrouped, "Macs in no group", false)
		if err != nil && !errors.Is(err, groups.ErrGroupExists) {
			return err
		}
	}
	if err := groups.SetClientMode(client, request.group, request.mode); err != nil {
		return fmt.Errorf("group %q: %w", request.group, err)
	}
	fmt.Printf("Updated the client mode of %s; it applies from each Mac's next sync (servers may take up to %s to see it)\n", groupText(request.group), groups.CacheDuration)

	summary := machineconfiguration.MachineConfigurationUpdateRequest{ClientMode: request.mode}
	if request.mode == nil {
		summary.Inherit = []string{"ClientMode"}
	}
	return flags.Record(client, "config.update", flags.GroupTarget(request.group), updateSummary(summary))
}

// getGroupConfig shows a group's client mode
func getGroupConfig(client dynamodb.GetItemAPI, group string) error {
	registry, err := groups.GetRegistry(client, true)
	if err != nil {
		return err
	}
	entry, registered := registry.Groups[group]
	if !registered && group != groups.Ungrouped {
		return fmt.Errorf("group %q: %w", group, groups.ErrGroupNotFound)
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 8, 0, '\t', tabwriter.AlignRight)
	fmt.Fprintln(writer, "Config\t Setting")
	fmt.Fprintln(writer, "Target:\t", groupText(group))
	switch mode, ok := entry.ClientModeOf(); {
	case ok:
		fmt.Fprintf(writer, "ClientMode:\t %d --> ( %s )\n", mode, clientModeName(mode))
	case entry.ClientMode.Stored():
		fmt.Fprintln(writer, "ClientMode:\t", "(stored value cannot be read, so the group has none)")
	default:
		fmt.Fprintln(writer, "ClientMode:\t", "(none: the Macs follow their own or the global client mode)")
	}
	return writer.Flush()
}

func groupText(group string) string {
	if group == groups.Ungrouped {
		return "the Macs in no group (ungrouped)"
	}
	return "group " + group
}

// groupClientModeOrigin returns the client mode a machine gets from its groups, and the group, using the membership
// stored on its groups item (as of its last preflight)
func groupClientModeOrigin(client dynamodb.GetItemAPI, machineID string) (types.ClientMode, string, bool, error) {
	registry, err := groups.GetRegistry(client, true)
	if err != nil {
		return 0, "", false, err
	}
	if !registry.HasClientModes() {
		return 0, "", false, nil
	}
	membership, err := groups.GetMachineGroups(client, machineID, true)
	if err != nil {
		return 0, "", false, err
	}
	mode, group, ok := groups.MachineClientMode(registry, membership)
	return mode, group, ok, nil
}

func groupWriter(cmd *cobra.Command) (groupConfigWriter, error) {
	region, _ := cmd.Flags().GetString("region")
	table, _ := cmd.Flags().GetString("dynamodb_table")
	ddb, err := dynamodb.GetClient(table, region)
	if err != nil {
		return nil, err
	}
	client, ok := ddb.(groupConfigWriter)
	if !ok {
		return nil, errors.New("this DynamoDB client cannot write device groups")
	}
	return client, nil
}
