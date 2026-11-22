package config

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/internal/cli/termsafe"
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

// groupUpdateFlags are the flags `config update --group` accepts: a group has a client mode and a file access override,
// and no other setting
var groupUpdateFlags = map[string]bool{
	"group": true, "ungrouped": true, "client-mode": true, flagConfirmGroupLockdown: true, flagOverrideFileAccessAction: true,
	flagInherit: true, "yes": true, "dry-run": true, "region": true, "dynamodb_table": true, "help": true,
}

const groupSettingsText = "a device group has only a client mode (-c) and a file access override (--" + flagOverrideFileAccessAction + ")"

// groupConfigRequest is a validated `config update --group|--ungrouped`
type groupConfigRequest struct {
	group  string
	change groups.SettingsChange
}

// removesOnly reports whether the request only removes settings
func (r groupConfigRequest) removesOnly() bool {
	return r.change.ClientMode == nil && r.change.FileAccessAction == nil
}

// parseGroupConfigRequest checks a config update for a group before connecting to the sync server
func parseGroupConfigRequest(cmd *cobra.Command, tf *flags.TargetFlags, clientMode optionalClientMode, inherit []string, confirmGroupLockdown bool) (groupConfigRequest, error) {
	if err := tf.RequireExplicitTarget(); err != nil {
		return groupConfigRequest{}, err
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
		return groupConfigRequest{}, fmt.Errorf("%s; %v cannot be set for a group", groupSettingsText, refused)
	}
	request := groupConfigRequest{group: tf.Group}
	settings, err := inheritedSettings(inherit)
	if err != nil {
		return request, err
	}
	for _, setting := range settings {
		switch setting {
		case "ClientMode":
			request.change.RemoveClientMode = true
		case "OverrideFileAccessAction":
			request.change.RemoveFileAccessAction = true
		default:
			return request, fmt.Errorf("%s; --%s takes only client-mode or %s for a group", groupSettingsText, flagInherit, flagOverrideFileAccessAction)
		}
	}
	if clientMode.set {
		if request.change.RemoveClientMode {
			return request, errors.New("the client mode cannot be both set and inherited")
		}
		mode := clientMode.mode.AsClientMode()
		request.change.ClientMode = &mode
		if machineconfiguration.RequiresGlobalLockdownConfirmation(mode) && !confirmGroupLockdown {
			return request, fmt.Errorf("%s blocks unknown executions on every Mac of the group; pass --%s to confirm", clientModeName(mode), flagConfirmGroupLockdown)
		}
	}
	if cmd.Flags().Changed(flagOverrideFileAccessAction) {
		if request.change.RemoveFileAccessAction {
			return request, errors.New("the file access override cannot be both set and inherited")
		}
		value, _ := cmd.Flags().GetString(flagOverrideFileAccessAction)
		action, err := types.ParseFileAccessAction(value)
		if err != nil {
			return request, fmt.Errorf("--%s: %w", flagOverrideFileAccessAction, err)
		}
		request.change.FileAccessAction = &action
	}
	if request.removesOnly() && !request.change.RemoveClientMode && !request.change.RemoveFileAccessAction {
		return request, fmt.Errorf("no group setting to update was given: pass -c <mode> or --%s <action>, or --%s client-mode or --%s %s to remove the group's", flagOverrideFileAccessAction, flagInherit, flagInherit, flagOverrideFileAccessAction)
	}
	return request, nil
}

// groupConfigWriter is what changing a group's settings needs
type groupConfigWriter interface {
	groups.RegistryWriter
	audit.Writer
}

// updateGroupConfig sets or removes a group's client mode and file access override, in one update. The reserved group
// of Macs in no group is registered when one of its settings is first set, as when it gets its first rule.
func updateGroupConfig(client groupConfigWriter, request groupConfigRequest, approval flags.Approval) error {
	registry, err := groups.GetRegistry(client, true)
	if err != nil {
		return err
	}
	entry, registered := registry.Groups[request.group]
	if !registered && request.group != groups.Ungrouped {
		return fmt.Errorf("group %q: %w; create it first with rudolph group create", request.group, groups.ErrGroupNotFound)
	}
	if !registered && request.removesOnly() {
		return errors.New("the Macs in no group have no settings of their own to remove")
	}
	change := request.change

	writer := tabwriter.NewWriter(os.Stdout, 0, 8, 0, '\t', tabwriter.AlignRight)
	fmt.Println("Setting the following configuration")
	fmt.Println()
	fmt.Fprintln(writer, "Config\t Setting")
	fmt.Fprintln(writer, "Group:\t", groupText(request.group))
	if change.ClientMode != nil || change.RemoveClientMode {
		if current, ok := entry.ClientModeOf(); ok {
			fmt.Fprintln(writer, "Current ClientMode:\t", clientModeName(current))
		}
		if change.ClientMode != nil {
			fmt.Fprintln(writer, "ClientMode:\t", *change.ClientMode, "-->(", clientModeName(*change.ClientMode), ")")
		} else {
			fmt.Fprintln(writer, "ClientMode:\t", "(none: the Macs follow their own or the global client mode)")
		}
	}
	if change.FileAccessAction != nil || change.RemoveFileAccessAction {
		if current, ok := entry.FileAccessOverrideOf(); ok {
			fmt.Fprintln(writer, "Current OverrideFileAccessAction:\t", current)
		}
		if change.FileAccessAction != nil {
			fmt.Fprintln(writer, "OverrideFileAccessAction:\t", *change.FileAccessAction)
		} else {
			fmt.Fprintln(writer, "OverrideFileAccessAction:\t", "(none: the Macs follow their own or the global override)")
		}
	}
	writer.Flush()
	fmt.Println()
	if change.ClientMode != nil || change.RemoveClientMode {
		fmt.Println("A Mac's own or temporary client mode beats its groups'; between its groups, the strictest mode wins.")
	}
	if change.FileAccessAction != nil || change.RemoveFileAccessAction {
		fmt.Println("A Mac's own file access override beats its groups'; between its groups, the most relaxed override wins.")
	}
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
	if err := groups.ChangeSettings(client, request.group, change); err != nil {
		return fmt.Errorf("group %q: %w", request.group, err)
	}
	fmt.Printf("Updated the settings of %s; they apply from each Mac's next sync (servers may take up to %s to see them)\n", groupText(request.group), groups.CacheDuration)

	summary := machineconfiguration.MachineConfigurationUpdateRequest{ClientMode: change.ClientMode, OverrideFileAccessAction: change.FileAccessAction}
	if change.RemoveClientMode {
		summary.Inherit = append(summary.Inherit, "ClientMode")
	}
	if change.RemoveFileAccessAction {
		summary.Inherit = append(summary.Inherit, "OverrideFileAccessAction")
	}
	return flags.Record(client, "config.update", flags.GroupTarget(request.group), updateSummary(summary))
}

// getGroupConfig shows a group's client mode and file access override
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
	switch action, ok := entry.FileAccessOverrideOf(); {
	case ok:
		fmt.Fprintln(writer, "OverrideFileAccessAction:\t", action)
	case entry.OverrideFileAccessAction.Stored():
		fmt.Fprintln(writer, "OverrideFileAccessAction:\t", "(stored value cannot be read, so the group has none)")
	default:
		fmt.Fprintln(writer, "OverrideFileAccessAction:\t", "(none: the Macs follow their own or the global override)")
	}
	return writer.Flush()
}

func groupText(group string) string {
	if group == groups.Ungrouped {
		return "the Macs in no group (ungrouped)"
	}
	return "group " + termsafe.String(group)
}

// machineGroupSettings returns the client mode and file access override a machine gets from its groups, and the group
// each comes from, using the membership stored on its groups item (as of its last preflight). It reads the groups item
// only when some group sets either.
func machineGroupSettings(client dynamodb.GetItemAPI, machineID string) (settings groupSettingsOrigin, err error) {
	registry, err := groups.GetRegistry(client, true)
	if err != nil {
		return settings, err
	}
	if !registry.HasClientModes() && !registry.HasFileAccessOverrides() {
		return settings, nil
	}
	membership, err := groups.GetMachineGroups(client, machineID, true)
	if err != nil {
		return settings, err
	}
	settings.mode, settings.modeGroup, settings.hasMode = groups.MachineClientMode(registry, membership)
	settings.fileAccess, settings.fileAccessGroup, settings.hasFileAccess = groups.MachineFileAccessOverride(registry, membership)
	return settings, nil
}

// groupSettingsOrigin is what machineGroupSettings returns
type groupSettingsOrigin struct {
	mode            types.ClientMode
	modeGroup       string
	hasMode         bool
	fileAccess      types.FileAccessAction
	fileAccessGroup string
	hasFileAccess   bool
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
