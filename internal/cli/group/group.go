// Package group is the `rudolph group` command: device groups (docs/groups.md), named sets of Macs that rules can
// target with --group.
package group

import (
	"errors"
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/spf13/cobra"
)

// GroupCmd is the `group` command; it only holds the subcommands
var GroupCmd = &cobra.Command{
	Use:   "group",
	Short: "Manage device groups: named sets of Macs that rules can target with --group",
	Long: `Device groups are named sets of Macs. A Mac is in the registered groups Santa reports for it (MachineOwnerGroups
in the Santa configuration profile, or MachineOwnerPlist with MachineOwnerGroupsKey) and in the groups assigned to
it here. Rules target a group with "rudolph rule ... --group <group>", and the Macs in no group with --ungrouped.`,
}

func init() {
	flags.AddYesFlag(GroupCmd)
	flags.AddDryRunFlag(GroupCmd)

	var description string
	var noReported, acceptReported, force bool
	var machineID string

	createCmd := &cobra.Command{
		Use:   "create <group> [--description <text>] [--no-reported]",
		Short: "Register a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := groups.Normalise(args[0])
			if err != nil {
				return err
			}
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			if approval := flags.ApprovalOf(cmd); approval.DryRun {
				registry, err := groups.GetRegistry(client, true)
				if err != nil {
					return err
				}
				if _, ok := registry.Groups[name]; ok {
					return fmt.Errorf("group %q: %w", name, groups.ErrGroupExists)
				}
				registered := len(registry.Groups)
				if _, ok := registry.Groups[groups.Ungrouped]; ok {
					registered--
				}
				if registered >= groups.MaxGroups {
					return fmt.Errorf("group %q: %w", name, groups.ErrTooManyGroups)
				}
				fmt.Printf("Creating group %q, description %q, Macs that report it %s\n", name, description, reportedText(!noReported))
				approval.Write()
				return nil
			}
			if err := groups.CreateGroup(clock.ConcreteTimeProvider{}, client, name, description, !noReported); err != nil {
				return fmt.Errorf("group %q: %w", name, err)
			}
			fmt.Printf("Created group %q", name)
			if noReported {
				fmt.Print(" (only Macs assigned with rudolph group add are members)")
			}
			fmt.Println()
			return flags.Record(client, "group.create", flags.GroupTarget(name), fmt.Sprintf("created group %s, description %q, Macs that report it %s", name, description, reportedText(!noReported)))
		},
	}
	createCmd.Flags().StringVar(&description, "description", "", "What the group is for")
	createCmd.Flags().BoolVar(&noReported, "no-reported", false, "Ignore the group when Santa reports it: only Macs assigned with rudolph group add are members. Use it for groups whose rules grant privilege.")

	updateCmd := &cobra.Command{
		Use:   "update <group> [--description <text>] [--no-reported | --accept-reported]",
		Short: "Change a group's description, or whether Macs that report it are members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := groups.NormaliseTarget(args[0])
			if err != nil {
				return err
			}
			if noReported && acceptReported {
				return errors.New("pass either --no-reported or --accept-reported, not both")
			}
			var desc *string
			if cmd.Flags().Changed("description") {
				desc = &description
			}
			var accept *bool
			if noReported || acceptReported {
				accept = &acceptReported
			}
			if desc == nil && accept == nil {
				return errors.New("nothing to change: pass --description, --no-reported or --accept-reported")
			}
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			if approval := flags.ApprovalOf(cmd); approval.DryRun {
				if err := requireGroup(client, name); err != nil {
					return err
				}
				fmt.Printf("Updating group %q:", name)
				if desc != nil {
					fmt.Printf(" description %q", *desc)
				}
				if accept != nil {
					fmt.Printf(" Macs that report it %s", reportedText(*accept))
				}
				fmt.Println()
				approval.Write()
				return nil
			}
			if err := groups.UpdateGroup(client, name, desc, accept); err != nil {
				return fmt.Errorf("group %q: %w", name, err)
			}
			fmt.Printf("Updated group %q\n", name)
			var changes []string
			if desc != nil {
				changes = append(changes, fmt.Sprintf("description %q", *desc))
			}
			if accept != nil {
				changes = append(changes, "Macs that report it "+reportedText(*accept))
			}
			return flags.Record(client, "group.update", flags.GroupTarget(name), fmt.Sprintf("updated group %s: %s", name, strings.Join(changes, ", ")))
		},
	}
	updateCmd.Flags().StringVar(&description, "description", "", "What the group is for")
	updateCmd.Flags().BoolVar(&noReported, "no-reported", false, "Ignore the group when Santa reports it")
	updateCmd.Flags().BoolVar(&acceptReported, "accept-reported", false, "Count Macs that report the group as members")

	deleteCmd := &cobra.Command{
		Use:   "delete <group> [--force]",
		Short: "Delete a group; refused while it has rules, unless --force, which deletes them too",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := groups.NormaliseTarget(args[0])
			if err != nil {
				return err
			}
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			fmt.Printf("Deleting group %q", name)
			if force {
				fmt.Print(" and all of its rules")
			}
			fmt.Println(". Its member Macs drop its rules, or get the global rules back, at their next sync.")
			approval := flags.ApprovalOf(cmd)
			if approval.DryRun {
				if err := requireGroup(client, name); err != nil {
					return err
				}
				rows, err := groups.GetRules(client, name)
				if err != nil {
					return err
				}
				if len(rows) > 0 && !force {
					return fmt.Errorf("group %q: the group has %d rules; remove them first, or pass --force to delete them with the group", name, len(rows))
				}
				for _, row := range rows {
					fmt.Println("  Rule:", row.SortKey)
				}
			}
			if apply, err := approval.Confirm(); !apply {
				return err
			}
			if err := groups.DeleteGroup(client, name, force); err != nil {
				err = fmt.Errorf("group %q: %w", name, err)
				if errors.Is(err, groups.ErrDeletedPartly) {
					return flags.RecordPartial(client, "group.delete", flags.GroupTarget(name), "deleting group "+name+" and its rules", err)
				}
				return err
			}
			fmt.Printf("Deleted group %q\n", name)
			summary := "deleted group " + name
			if force {
				summary += " and its rules"
			}
			return flags.Record(client, "group.delete", flags.GroupTarget(name), summary)
		},
	}
	deleteCmd.Flags().BoolVar(&force, "force", false, "Also delete the group's rules")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the registered groups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			registry, err := groups.GetRegistry(client, true)
			if err != nil {
				return err
			}
			if len(registry.Groups) == 0 {
				fmt.Println("No groups. Create one with rudolph group create <group>.")
				return nil
			}
			fmt.Printf("%-32s %-10s %-11s %-8s %s\n", "GROUP", "REPORTED", "CLIENTMODE", "VERSION", "DESCRIPTION")
			for _, name := range registry.Names() {
				entry := registry.Groups[name]
				reported := "counts"
				if !entry.AcceptReported {
					reported = "ignored"
				}
				fmt.Printf("%-32s %-10s %-11s %-8d %s\n", name, reported, clientModeText(entry), entry.RulesVersion, entry.Description)
			}
			return nil
		},
	}

	getCmd := &cobra.Command{
		Use:   "get <group>",
		Short: "Show a group and its rules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := groups.NormaliseTarget(args[0])
			if err != nil {
				return err
			}
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			registry, err := groups.GetRegistry(client, true)
			if err != nil {
				return err
			}
			entry, ok := registry.Groups[name]
			if !ok {
				return fmt.Errorf("group %q: %w", name, groups.ErrGroupNotFound)
			}
			rows, err := groups.GetRules(client, name)
			if err != nil {
				return err
			}
			fmt.Println("Group:          ", name)
			fmt.Println("Description:    ", entry.Description)
			fmt.Println("AcceptReported: ", entry.AcceptReported)
			fmt.Println("ClientMode:     ", clientModeText(entry))
			fmt.Println("RulesVersion:   ", entry.RulesVersion)
			fmt.Println("CreatedAt:      ", entry.CreatedAt)
			fmt.Printf("Rules (%d):\n", len(rows))
			for _, row := range rows {
				policy, _ := row.Policy.MarshalText()
				fmt.Printf("  %-12s %s\n", policy, row.SortKey)
			}
			return nil
		},
	}

	addCmd := &cobra.Command{
		Use:   "add -m <machine-id> <group>",
		Short: "Assign a group to a Mac",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return changeMembership(cmd, machineID, args[0], true)
		},
	}
	addCmd.Flags().StringVarP(&machineID, "machine", "m", "", "The Mac's machine UUID")
	_ = addCmd.MarkFlagRequired("machine")

	removeCmd := &cobra.Command{
		Use:   "remove -m <machine-id> <group>",
		Short: "Remove a group assigned to a Mac (groups the Mac reports are not affected)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return changeMembership(cmd, machineID, args[0], false)
		},
	}
	removeCmd.Flags().StringVarP(&machineID, "machine", "m", "", "The Mac's machine UUID")
	_ = removeCmd.MarkFlagRequired("machine")

	showCmd := &cobra.Command{
		Use:   "show -m <machine-id>",
		Short: "Show a Mac's assigned, reported and effective groups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			id, err := flags.ResolveMachineID(client, machineID)
			if err != nil {
				return err
			}
			registry, err := groups.GetRegistry(client, true)
			if err != nil {
				return err
			}
			membership, err := groups.GetMachineGroups(client, id, true)
			if err != nil {
				return err
			}
			fmt.Println("Machine:   ", id)
			fmt.Println("Assigned:  ", list(membership.Assigned))
			fmt.Println("Reported:  ", list(membership.Reported), " (registered groups that count, as of the Mac's last preflight)")
			effective := groups.Effective(registry, membership)
			if len(effective) == 1 && effective[0] == groups.Ungrouped {
				fmt.Println("Effective:  ungrouped (in no group: gets the --ungrouped rules)")
			} else {
				fmt.Println("Effective: ", list(effective))
			}
			return nil
		},
	}
	showCmd.Flags().StringVarP(&machineID, "machine", "m", "", "The Mac's machine UUID")
	_ = showCmd.MarkFlagRequired("machine")

	var rebuild bool
	membersCmd := &cobra.Command{
		Use:   "members <group> | --rebuild [--yes|--dry-run]",
		Short: "List the Macs in a group, or rebuild the group member index from every Mac's groups",
		Long: `Lists the Macs in a registered group, by machine ID, each marked "assigned" (rudolph group add), "reported" (Santa
reports the group for it) or both. Macs that only report a group whose reported membership is ignored (--no-reported)
are left out, as they are not members. The Macs in no group (rudolph:ungrouped) cannot be listed.

With --rebuild, rebuilds the member index of every group from the groups of every Mac (one Scan of the table): writes
the rows that are missing or wrong and deletes the rows of Macs no longer in the group. Run it once after upgrading to
a version that keeps the index, so that Macs whose groups have not changed since are listed too.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case rebuild && len(args) > 0:
				return errors.New("--rebuild rebuilds every group: pass no group with it")
			case !rebuild && len(args) == 0:
				return errors.New("pass a group, or --rebuild")
			}
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			if rebuild {
				return rebuildMembers(cmd, client)
			}
			return listMembers(client, args[0])
		},
	}
	membersCmd.Flags().BoolVar(&rebuild, "rebuild", false, "Rebuild the member index of every group from every Mac's groups")

	GroupCmd.AddCommand(createCmd, updateCmd, deleteCmd, listCmd, getCmd, addCmd, removeCmd, showCmd, membersCmd)
}

func listMembers(client client, group string) error {
	name, err := groups.NormaliseTarget(group)
	if err != nil {
		return err
	}
	if name == groups.Ungrouped {
		return errors.New("the Macs in no group are not indexed: list the Macs in each group instead")
	}
	registry, err := groups.GetRegistry(client, true)
	if err != nil {
		return err
	}
	entry, ok := registry.Groups[name]
	if !ok {
		return fmt.Errorf("group %q: %w", name, groups.ErrGroupNotFound)
	}
	members, err := groups.ListMembers(client, name)
	if err != nil {
		return err
	}
	var lines []string
	for _, m := range members {
		var how []string
		if m.Assigned {
			how = append(how, "assigned")
		}
		if m.Reported && entry.AcceptReported {
			how = append(how, "reported")
		}
		if len(how) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%-38s %s", m.SK, strings.Join(how, ", ")))
	}
	if len(lines) == 0 {
		fmt.Printf("No Macs in group %q.\n", name)
		return nil
	}
	fmt.Printf("%-38s %s\n", "MACHINE", "MEMBERSHIP")
	for _, line := range lines {
		fmt.Println(line)
	}
	fmt.Printf("%d Macs in group %q.\n", len(lines), name)
	return nil
}

// rebuildShown bounds the rows rebuildMembers lists; a first rebuild after upgrading writes a row per membership
const rebuildShown = 20

func rebuildMembers(cmd *cobra.Command, client client) error {
	plan, err := groups.PlanMembersRebuild(clock.ConcreteTimeProvider{}, client)
	if err != nil {
		return err
	}
	fmt.Printf("Read the groups of %d Macs and %d member rows.\n", plan.Macs, plan.Rows)
	if plan.Empty() {
		fmt.Println("Every member row is right; nothing to write.")
		return nil
	}
	fmt.Printf("Writing %d missing or wrong member rows and deleting %d that no Mac's groups back:\n", len(plan.Writes), len(plan.Deletes))
	shown := 0
	for _, row := range plan.Writes {
		if shown++; shown > rebuildShown {
			break
		}
		fmt.Printf("  write  %s / %s (assigned %t, reported %t)\n", row.PK, row.SK, row.Assigned, row.Reported)
	}
	for _, key := range plan.Deletes {
		if shown++; shown > rebuildShown {
			break
		}
		fmt.Printf("  delete %s / %s\n", key.PartitionKey, key.SortKey)
	}
	if total := len(plan.Writes) + len(plan.Deletes); total > rebuildShown {
		fmt.Printf("  and %d more\n", total-rebuildShown)
	}
	if apply, err := flags.ApprovalOf(cmd).Confirm(); !apply {
		return err
	}
	summary := fmt.Sprintf("rebuilt the group member rows from %d Macs' groups: wrote %d, deleted %d", plan.Macs, len(plan.Writes), len(plan.Deletes))
	if err := plan.Apply(client); err != nil {
		// The rows are sent 25 at a time, so only a failure after the first batch can leave some written
		if len(plan.Writes)+len(plan.Deletes) <= 25 {
			return err
		}
		return flags.RecordPartial(client, "group.members.rebuild", "groups", "rebuilding the group member rows", err)
	}
	fmt.Printf("Wrote %d member rows and deleted %d.\n", len(plan.Writes), len(plan.Deletes))
	return flags.Record(client, "group.members.rebuild", "groups", summary)
}

func changeMembership(cmd *cobra.Command, machineID, group string, add bool) error {
	name, err := groups.Normalise(group)
	if err != nil {
		return err
	}
	client, err := writer(cmd)
	if err != nil {
		return err
	}
	id, err := flags.ResolveMachineID(client, machineID)
	if err != nil {
		return err
	}
	if approval := flags.ApprovalOf(cmd); approval.DryRun {
		if add {
			// As AddAssigned checks
			if err := requireGroup(client, name); err != nil {
				return err
			}
			fmt.Printf("Assigning group %q to %s\n", name, id)
		} else {
			fmt.Printf("Removing assigned group %q from %s\n", name, id)
		}
		approval.Write()
		return nil
	}
	timeProvider := clock.ConcreteTimeProvider{}
	if add {
		if err := groups.AddAssigned(timeProvider, client, id, name); err != nil {
			return fmt.Errorf("group %q: %w", name, err)
		}
		fmt.Printf("Assigned group %q to %s; it applies from the Mac's next sync\n", name, id)
		return flags.Record(client, "group.add", flags.MachineTarget(id), "assigned group "+name)
	}
	if err := groups.RemoveAssigned(timeProvider, client, id, name); err != nil {
		return fmt.Errorf("group %q: %w", name, err)
	}
	fmt.Printf("Removed group %q from %s (if it reports the group, it stays a member)\n", name, id)
	return flags.Record(client, "group.remove", flags.MachineTarget(id), "removed assigned group "+name)
}

// requireGroup returns an error unless the group is registered
func requireGroup(client groups.RegistryWriter, name string) error {
	registry, err := groups.GetRegistry(client, true)
	if err != nil {
		return err
	}
	if _, ok := registry.Groups[name]; !ok {
		return fmt.Errorf("group %q: %w", name, groups.ErrGroupNotFound)
	}
	return nil
}

// reportedText says whether Macs that report a group are members
func reportedText(accept bool) string {
	if accept {
		return "count as members"
	}
	return "are ignored (only assigned Macs are members)"
}

// client is what the group commands need from DynamoDB
type client interface {
	groups.RegistryWriter
	dynamodb.ScanAPI
	flags.MachineIDLookupAPI
	audit.Writer
}

func writer(cmd *cobra.Command) (client, error) {
	region, _ := cmd.Flags().GetString("region")
	table, _ := cmd.Flags().GetString("dynamodb_table")
	ddb, err := dynamodb.GetClient(table, region)
	if err != nil {
		return nil, err
	}
	c, ok := ddb.(client)
	if !ok {
		return nil, errors.New("this DynamoDB client cannot write groups")
	}
	return c, nil
}

func list(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// clientModeText shows a group's client mode (set with rudolph config update --group)
func clientModeText(entry groups.Entry) string {
	mode, ok := entry.ClientModeOf()
	switch {
	case ok:
		text, _ := mode.MarshalText()
		return string(text)
	case entry.ClientMode.Stored():
		return "unreadable"
	}
	return "-"
}
