package lookup

import (
	"errors"
	"strings"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/spf13/cobra"
)

func init() {
	var limit int32

	var lookupMachineIDsCmd = &cobra.Command{
		Use:   "machine-ids [<prefix>] [--limit <n>]",
		Short: "Find the machine IDs that start with a prefix (every machine ID without one)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 {
				return errors.New("--limit must be at least 1")
			}
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)

			prefix := strings.Join(args, "")

			sensorDataFinderService := sensordata.GetSensorDataFinder(dynamodbClient)

			machineIDs, err := sensorDataFinderService.GetMachineIDsStartingWith(prefix, limit)
			if err != nil {
				return err
			}
			printMachineIDs(machineIDs, limit)
			return nil
		},
	}

	lookupMachineIDsCmd.Flags().Int32Var(&limit, "limit", 10, "The most machine IDs to show")

	LookupCmd.AddCommand(lookupMachineIDsCmd)
}
