package info

import (
	"fmt"
	"os"
	"time"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/internal/cli/machine"
	"github.com/airbnb/rudolph/internal/cli/santa_sensor"
	"github.com/spf13/cobra"
)

var (
	InfoCmd *cobra.Command
)

// selfMachineID returns this Mac's machine ID; a variable so tests can run on machines without Santa
var selfMachineID = santa_sensor.GetSelfMachineID

func init() {
	InfoCmd = &cobra.Command{
		Use:   "info",
		Short: "Show this Mac's machine ID and, with a deployment, what Rudolph knows about it",
		Long: `Show this Mac's machine ID. When a deployment is configured (--env or ENV, or --region and --dynamodb_table),
also show the configuration Rudolph sends this Mac and, if it has synced, what it reported (as rudolph machine show).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return info(cmd)
		},
	}
}

func info(cmd *cobra.Command) error {
	machineID, err := selfMachineID()
	if err != nil {
		return fmt.Errorf("failed to get MachineUUID: %w", err)
	}

	fmt.Println("Your machine ID is:", machineID)

	region, _ := cmd.Flags().GetString("region")
	table, _ := cmd.Flags().GetString("dynamodb_table")
	if region == "" || table == "" {
		fmt.Println("To also see what Rudolph knows about this Mac, set ENV (or pass --env) to the deployment.")
		return nil
	}

	client, err := machine.Client(cmd)
	if err != nil {
		return err
	}
	id, err := flags.ResolveMachineID(client, machineID)
	if err != nil {
		return err
	}
	summary, err := machine.Load(client, id)
	if err != nil {
		return err
	}
	fmt.Println()
	summary.Print(os.Stdout, time.Now())
	return nil
}
