// Package fleet is the `rudolph fleet` command: settings of the fleet health lists (docs/fleet-health.md).
package fleet

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/internal/cli/termsafe"
	fleetjob "github.com/airbnb/rudolph/internal/fleet"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/airbnb/rudolph/pkg/model/fleethealth"
	"github.com/spf13/cobra"
)

const (
	auditAction           = "fleet.holiday"
	minVersionAuditAction = "fleet.min-santa-version"
	autoCleanSyncAction   = "fleet.auto-clean-sync"
	auditTarget           = "fleet"
)

// FleetCmd is the `fleet` command; it only holds the subcommands
var FleetCmd = &cobra.Command{
	Use:   "fleet",
	Short: "Settings of the fleet health lists the daily fleet job builds",
	Long: `The fleet job lists, once a day, the Macs that need attention (docs/fleet-health.md). A Mac is stale when its last
completed sync is older than the stale threshold, 10 days unless changed, and listed under Version when its Santa
release is below the minimum Santa version, ` + fleethealth.DefaultMinSantaVersion + ` unless changed. A Mac whose rules hash differs from that of the Macs
sent the same rules gets a clean sync requested, once per rule change, before it is listed, unless that is turned off.`,
}

func init() {
	flags.AddYesFlag(FleetCmd)
	flags.AddDryRunFlag(FleetCmd)

	var staleDays int
	var until string
	var end bool

	holidayCmd := &cobra.Command{
		Use:   "holiday (--stale-days <days> --until <YYYY-MM-DD> | --end)",
		Short: "Raise the stale threshold until a date (school holidays), or end that early",
		Long: `Over a holiday Macs are simply off. Holiday mode raises the stale threshold to --stale-days until the end of
--until (a UTC date, inclusive); the day after, the normal threshold applies again by itself. A new holiday replaces the
one set. --end removes the holiday now. The fleet job uses the setting from its next run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			daysGiven, untilGiven := cmd.Flags().Changed("stale-days"), cmd.Flags().Changed("until")
			switch {
			case end && (daysGiven || untilGiven):
				return errors.New("pass either --end, or --stale-days with --until, not both")
			case !end && !daysGiven && !untilGiven:
				return errors.New("nothing to change: pass --stale-days with --until to set a holiday, or --end to end it")
			case !end && daysGiven != untilGiven:
				return errors.New("a holiday needs both --stale-days and --until")
			}
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			timeProvider := clock.ConcreteTimeProvider{}
			current, err := fleethealth.Get(client, true)
			if err != nil {
				return err
			}
			if end {
				if current.HolidayStaleDays == 0 {
					// The item may still hold a holiday that cannot be read (edited by hand); ending removes that too
					fmt.Printf("Ending any holiday stored; the stale threshold stays at %d days\n", current.NormalStaleDays())
				} else {
					fmt.Printf("Ending the holiday (%d stale days until %s); the stale threshold returns to %d days\n", current.HolidayStaleDays, current.HolidayUntil, current.NormalStaleDays())
				}
				if apply, err := flags.ApprovalOf(cmd).Confirm(); !apply {
					return err
				}
				ended, err := fleethealth.EndHoliday(client)
				if errors.Is(err, fleethealth.ErrNoHoliday) {
					fmt.Println("No holiday was set; nothing changed.")
					return nil
				}
				if err != nil {
					return err
				}
				fmt.Printf("Holiday ended. Stale threshold: %d days\n", ended.NormalStaleDays())
				summary := fmt.Sprintf("ended the holiday; stale threshold back to %d days", ended.NormalStaleDays())
				if ended.HolidayStaleDays != 0 {
					summary = fmt.Sprintf("ended the holiday of %d stale days until %s; stale threshold back to %d days", ended.HolidayStaleDays, ended.HolidayUntil, ended.NormalStaleDays())
				}
				return flags.Record(client, auditAction, auditTarget, summary)
			}

			// Checked here as well as in SetHoliday, so a refused holiday is refused before anyone is asked to confirm
			if err := current.ValidateHoliday(staleDays, until, timeProvider.Now()); err != nil {
				return err
			}
			fmt.Printf("Setting a holiday: stale after %d days until %s (UTC, inclusive); normally %d days\n", staleDays, until, current.NormalStaleDays())
			if apply, err := flags.ApprovalOf(cmd).Confirm(); !apply {
				return err
			}
			stored, err := fleethealth.SetHoliday(timeProvider, client, staleDays, until)
			if err != nil {
				return err
			}
			fmt.Printf("Holiday set: stale after %d days until %s, then %d days again\n", stored.HolidayStaleDays, stored.HolidayUntil, stored.NormalStaleDays())
			return flags.Record(client, auditAction, auditTarget, fmt.Sprintf("set a holiday: stale after %d days until %s (normally %d days)", stored.HolidayStaleDays, stored.HolidayUntil, stored.NormalStaleDays()))
		},
	}
	holidayCmd.Flags().IntVar(&staleDays, "stale-days", 0, "Days without a completed sync before a Mac is stale during the holiday (1 to 365, at least the normal threshold)")
	holidayCmd.Flags().StringVar(&until, "until", "", "The holiday's last day as YYYY-MM-DD (a UTC date, inclusive; today or later, at most 366 days ahead)")
	holidayCmd.Flags().BoolVar(&end, "end", false, "End the holiday now")
	FleetCmd.AddCommand(holidayCmd)

	var useDefault bool
	minVersionCmd := &cobra.Command{
		Use:   "min-santa-version (<version> | off | --default)",
		Short: "Set the Santa release below which a Mac is listed under Version, or turn that list off",
		Long: `A Mac whose Santa release is below the minimum Santa version is listed under Version. Pass a release such as
2026.10 to set the minimum, off to list no Mac under Version, or --default to clear the stored value, so that the
default, ` + fleethealth.DefaultMinSantaVersion + `, applies again. The stale threshold and any holiday are left as they are. The fleet job uses the
setting from its next run.`,
		// Arguments are checked in RunE, so each refusal says what the command takes
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) > 1:
				return fmt.Errorf("pass one release or off, not %d values", len(args))
			case useDefault && len(args) == 1:
				return errors.New("pass either a release or off, or --default, not both")
			case !useDefault && len(args) == 0:
				return errors.New("nothing to change: pass a release such as 2026.10, off, or --default to return to the default")
			}
			version := ""
			if !useDefault {
				// Checked here as well as in SetMinSantaVersion, so a refused value is refused before anyone is asked to
				// confirm
				var err error
				if version, err = fleethealth.ValidateMinSantaVersion(args[0]); err != nil {
					return err
				}
			}
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			// The raw item, not fleethealth.Get: a stored value that cannot be used decodes as none, and --default has
			// to tell it from nothing stored
			out, err := client.GetItem(fleethealth.Key(), true)
			if err != nil {
				return fmt.Errorf("failed to read the fleet health setting: %w", err)
			}
			current := fleethealth.FromItem(out.Item)
			_, attributeStored := out.Item["MinSantaVersion"]
			inForce, _, usable := current.MinSantaVersionInForce()
			was := termsafe.String(inForce)
			switch {
			case !usable && attributeStored:
				was = "the default " + inForce
				fmt.Printf("Minimum Santa version in force: %s (the stored value cannot be used and is ignored)\n", was)
			case !usable:
				was = "the default " + inForce
				fmt.Printf("Minimum Santa version in force: %s\n", was)
			default:
				fmt.Printf("Minimum Santa version in force: %s (stored)\n", was)
			}

			if useDefault {
				if !attributeStored {
					fmt.Println("No minimum Santa version is stored; nothing changed.")
					return nil
				}
				fmt.Printf("Clearing the stored minimum Santa version; the default %s will apply\n", fleethealth.DefaultMinSantaVersion)
				if apply, err := flags.ApprovalOf(cmd).Confirm(); !apply {
					return err
				}
				before, err := fleethealth.ClearMinSantaVersion(client)
				if errors.Is(err, fleethealth.ErrNoMinSantaVersion) {
					fmt.Println("No minimum Santa version was stored; nothing changed.")
					return nil
				}
				if err != nil {
					return err
				}
				fmt.Printf("Stored minimum Santa version cleared. Minimum Santa version: %s (default), from the next fleet run\n", fleethealth.DefaultMinSantaVersion)
				// What ClearMinSantaVersion read, not what was shown above: the entry names the value that was removed.
				// The fleet job ignored a value that cannot be used, and the entry says so rather than repeat it.
				cleared := "an unusable value"
				if version, _, stored := before.MinSantaVersionInForce(); stored {
					cleared = version
				}
				return flags.Record(client, minVersionAuditAction, auditTarget, fmt.Sprintf("cleared the stored minimum Santa version (was %s); the default %s applies",
					cleared, fleethealth.DefaultMinSantaVersion))
			}

			off := version == fleethealth.MinSantaVersionOff
			if off {
				fmt.Println("Turning the minimum Santa version check off: no Mac will be listed under Version")
			} else {
				fmt.Printf("Setting the minimum Santa version to %s: a Mac with an older Santa release will be listed under Version\n", termsafe.String(version))
			}
			if apply, err := flags.ApprovalOf(cmd).Confirm(); !apply {
				return err
			}
			stored, err := fleethealth.SetMinSantaVersion(client, version)
			if err != nil {
				return err
			}
			if off {
				fmt.Println("Minimum Santa version: off (stored), from the next fleet run")
				return flags.Record(client, minVersionAuditAction, auditTarget, fmt.Sprintf("turned the minimum Santa version check off (was %s)", was))
			}
			fmt.Printf("Minimum Santa version: %s (stored), from the next fleet run\n", termsafe.String(stored.MinSantaVersion))
			return flags.Record(client, minVersionAuditAction, auditTarget, fmt.Sprintf("set the minimum Santa version to %s (was %s)", stored.MinSantaVersion, was))
		},
	}
	minVersionCmd.Flags().BoolVar(&useDefault, "default", false, "Clear the stored value, so the default ("+fleethealth.DefaultMinSantaVersion+") applies")
	FleetCmd.AddCommand(minVersionCmd)

	FleetCmd.AddCommand(&cobra.Command{
		Use:   "auto-clean-sync (on | off)",
		Short: "Turn the fleet job's automatic clean syncs for rules hash outliers on or off",
		Long: `Santa's rules hash depends on the order a Mac wrote its rules, so Macs holding the same rules can report different
hashes, and a clean sync brings such a Mac back to the hash of the others. The fleet job therefore requests a clean
sync for a Mac whose rules hash differs, once per rule change and at most ` + fmt.Sprint(fleetjob.MaxCleanSyncRequestsPerRun) + ` Macs a run, and lists the Mac only
when its hash still differs afterwards. off makes the job list every such Mac straight away and request nothing; on is
the default. The rest of the fleet health setting is left as it is. The fleet job uses the setting from its next run.`,
		// Arguments are checked in RunE, so each refusal says what the command takes
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
				return errors.New("pass on or off: `rudolph fleet auto-clean-sync on` or `rudolph fleet auto-clean-sync off`")
			}
			on := args[0] == "on"
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			current, err := fleethealth.Get(client, true)
			if err != nil {
				return err
			}
			fmt.Printf("Automatic clean syncs: %s\n", autoCleanSyncText(current))
			if current.AutoCleanSyncOn() == on {
				fmt.Printf("Automatic clean syncs are already %s; nothing changed.\n", args[0])
				return nil
			}
			if on {
				fmt.Printf("Turning automatic clean syncs on: the fleet job will request a clean sync for a Mac whose rules hash differs, at most %d a run\n", fleetjob.MaxCleanSyncRequestsPerRun)
			} else {
				fmt.Println("Turning automatic clean syncs off: the fleet job will list every Mac whose rules hash differs and request no clean sync")
			}
			if apply, err := flags.ApprovalOf(cmd).Confirm(); !apply {
				return err
			}
			stored, err := fleethealth.SetAutoCleanSync(client, on)
			if err != nil {
				return err
			}
			fmt.Printf("Automatic clean syncs: %s, from the next fleet run\n", autoCleanSyncText(stored))
			return flags.Record(client, autoCleanSyncAction, auditTarget, "turned automatic clean syncs for rules hash outliers "+args[0])
		},
	})

	FleetCmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the fleet health setting in force: the stale threshold, any holiday, the minimum Santa version and automatic clean syncs",
		Long: `Prints what the fleet job's next run will use: the stale threshold, with the holiday while one is on, the minimum
Santa version, with whether it is stored or the default, and whether automatic clean syncs for rules hash outliers are
on. Nothing is written.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := writer(cmd)
			if err != nil {
				return err
			}
			current, err := fleethealth.Get(client, true)
			if err != nil {
				return err
			}
			if current.HolidayOn(clock.ConcreteTimeProvider{}.Now()) {
				fmt.Printf("Stale threshold: %d days, holiday: %d days until %s\n", current.NormalStaleDays(), current.HolidayStaleDays, current.HolidayUntil)
			} else {
				fmt.Printf("Stale threshold: %d days\n", current.NormalStaleDays())
			}
			version, _, stored := current.MinSantaVersionInForce()
			source := "default"
			if stored {
				source = "stored"
			}
			fmt.Printf("Minimum Santa version: %s (%s)\n", termsafe.String(version), source)
			fmt.Printf("Automatic clean syncs: %s\n", autoCleanSyncText(current))
			return nil
		},
	})
}

// autoCleanSyncText is the setting as the fleet job reads it: anything stored other than off is the default.
func autoCleanSyncText(c fleethealth.Config) string {
	if c.AutoCleanSyncOn() {
		return "on (default)"
	}
	return "off (stored)"
}

// client is what the fleet commands need from DynamoDB
type client interface {
	fleethealth.Writer
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
		return nil, errors.New("this DynamoDB client cannot write the fleet health setting")
	}
	return c, nil
}
