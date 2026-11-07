package machine

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spf13/cobra"
)

// serialIndex is the sensor data index by serial number that rudolph-web lists Macs from (docs/sync.md, "Sensor data
// row"). It projects everything the list shows, so listing costs one Query and never a Scan of the table.
const serialIndex = "DataType_SerialKey"

const defaultLimit = 50

// MachinesCmd is the `machines` command
var MachinesCmd = &cobra.Command{
	Use:   "machines [--limit <n>]",
	Short: "List the Macs that have synced, by serial number",
	Long: `List the Macs that have synced, ordered by serial number: machine ID, hostname, serial number, Santa version
and the day (UTC) the Mac last synced. A Mac that reports no serial number is not listed; find it with
rudolph lookup machine-ids. Sensor data expires 90 days after a Mac's last sync.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		limit, _ := cmd.Flags().GetInt("limit")
		if limit < 1 {
			return errors.New("--limit must be at least 1")
		}
		client, err := Client(cmd)
		if err != nil {
			return err
		}
		rows, more, err := listMachines(client, limit)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Println("no machine found")
			return nil
		}
		fmt.Printf("%-36s  %-24s  %-14s  %-12s  %s\n", "MACHINE ID", "HOSTNAME", "SERIAL", "SANTA", "LAST SEEN (UTC)")
		for _, r := range rows {
			fmt.Printf("%-36s  %-24s  %-14s  %-12s  %s\n", r.MachineID, dash(r.Hostname), dash(r.SerialNum), dash(r.SantaVersion), dash(r.LastSeenDay))
		}
		if more {
			fmt.Printf("(showing the first %d; raise --limit to see more)\n", limit)
		}
		return nil
	},
}

func init() {
	MachinesCmd.Flags().Int("limit", defaultLimit, "The most Macs to list")
}

// machineRow is a sensor data row as the serial index projects it
type machineRow struct {
	MachineID    string `dynamodbav:"MachineID"`
	Hostname     string `dynamodbav:"Hostname"`
	SerialNum    string `dynamodbav:"SerialNum"`
	SantaVersion string `dynamodbav:"SantaVersion"`
	LastSeenDay  string `dynamodbav:"LastSeenDay"`
}

// listMachines returns up to limit Macs from the serial index, and whether there are more
func listMachines(client dynamodb.QueryAPI, limit int) (rows []machineRow, more bool, err error) {
	var start map[string]awstypes.AttributeValue
	for {
		// One more than asked, to tell whether there are more
		out, err := client.Query(&awsdynamodb.QueryInput{
			IndexName:              aws.String(serialIndex),
			KeyConditionExpression: aws.String("DataType = :dt"),
			ExpressionAttributeValues: map[string]awstypes.AttributeValue{
				":dt": &awstypes.AttributeValueMemberS{Value: string(sensordata.GetDataType())},
			},
			Limit:             aws.Int32(int32(limit + 1 - len(rows))),
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, false, fmt.Errorf("failed to read the %s index: %w", serialIndex, err)
		}
		var page []machineRow
		if err := attributevalue.UnmarshalListOfMaps(out.Items, &page); err != nil {
			return nil, false, fmt.Errorf("failed to read the %s index: %w", serialIndex, err)
		}
		rows = append(rows, page...)
		if len(rows) > limit {
			return rows[:limit], true, nil
		}
		if len(out.LastEvaluatedKey) == 0 {
			return rows, false, nil
		}
		start = out.LastEvaluatedKey
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
