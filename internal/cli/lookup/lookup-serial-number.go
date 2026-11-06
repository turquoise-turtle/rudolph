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

	var lookupSerialNumberCmd = &cobra.Command{
		Use:   "serial-number <serial-number> [--limit <n>]",
		Short: "Find the machine IDs that synced with a serial number",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 {
				return errors.New("--limit must be at least 1")
			}
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)

			serialNumber := strings.Join(args, "")

			sensorDataFinderService := sensordata.GetSensorDataFinder(dynamodbClient)

			machineIDs, err := sensorDataFinderService.GetMachineIDsFromSerialNumber(serialNumber, limit)
			if err != nil {
				return err
			}
			printMachineIDs(machineIDs, limit)
			return nil
		},
	}

	lookupSerialNumberCmd.Flags().Int32Var(&limit, "limit", 5, "The most machine IDs to show")

	LookupCmd.AddCommand(lookupSerialNumberCmd)
}
