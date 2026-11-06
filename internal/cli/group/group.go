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
			return nil
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
			return nil
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
				return fmt.Errorf("group %q: %w", name, err)
			}
			fmt.Printf("Deleted group %q\n", name)
			return nil
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
			fmt.Printf("%-32s %-10s %-8s %s\n", "GROUP", "REPORTED", "VERSION", "DESCRIPTION")
			for _, name := range registry.Names() {
				entry := registry.Groups[name]
				reported := "counts"
				if !entry.AcceptReported {
					reported = "ignored"
				}
				fmt.Printf("%-32s %-10s %-8d %s\n", name, reported, entry.RulesVersion, entry.Description)
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

	GroupCmd.AddCommand(createCmd, updateCmd, deleteCmd, listCmd, getCmd, addCmd, removeCmd, showCmd)
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
		return nil
	}
	if err := groups.RemoveAssigned(timeProvider, client, id, name); err != nil {
		return fmt.Errorf("group %q: %w", name, err)
	}
	fmt.Printf("Removed group %q from %s (if it reports the group, it stays a member)\n", name, id)
	return nil
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
	flags.MachineIDLookupAPI
}

func writer(cmd *cobra.Command) (client, error) {
	region, _ := cmd.Flags().GetString("region")
	table, _ := cmd.Flags().GetString("dynamodb_table")
	c, ok := dynamodb.GetClient(table, region).(client)
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
