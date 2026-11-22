// Package fleet is the `rudolph fleet` command: settings of the fleet health lists (docs/fleet-health.md).
package fleet

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/airbnb/rudolph/pkg/model/fleethealth"
	"github.com/spf13/cobra"
)

const (
	auditAction = "fleet.holiday"
	auditTarget = "fleet"
)

// FleetCmd is the `fleet` command; it only holds the subcommands
var FleetCmd = &cobra.Command{
	Use:   "fleet",
	Short: "Settings of the fleet health lists the daily fleet job builds",
	Long: `The fleet job lists, once a day, the Macs that need attention (docs/fleet-health.md). A Mac is stale when its last
completed sync is older than the stale threshold, 10 days unless changed.`,
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
