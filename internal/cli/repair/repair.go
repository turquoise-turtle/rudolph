package repair

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/scan"
)

var (
	RepairCmd *cobra.Command
)

func init() {
	var deleteItems bool
	RepairCmd = &cobra.Command{
		Use:   "repair [--delete [--yes|--dry-run]]",
		Short: "Scan the whole table for legacy items (MachineInfo#, MachineConfig#); with --delete, delete them",
		Long: `Scan the whole table for items written by early versions of Rudolph under the MachineInfo# and MachineConfig#
partition keys, which nothing reads any more, and list them. Without --delete this is a dry run that changes
nothing. With --delete the items found are deleted, after a confirmation (none with --yes).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deleteFound := deleteItems
			approval := flags.ApprovalOf(cmd)
			if deleteItems && approval.DryRun {
				fmt.Println("Legacy items found would be deleted as they are found.")
				approval.Write()
				deleteFound = false
			} else if deleteItems {
				fmt.Println("Legacy items found are deleted as they are found.")
				apply, err := approval.Confirm()
				if err != nil {
					return err
				}
				deleteFound = apply
			} else {
				fmt.Println("Dry run: legacy items are listed, not deleted (pass --delete to delete them).")
			}
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient, err := dynamodb.GetClient(table, region)

			if err != nil {

				return err

			}
			scanService := scan.GetScanService(dynamodbClient)

			deleted, err := repair(scanService, dynamodbClient, deleteFound)
			if !deleteFound || deleted == 0 {
				return err
			}
			// Recorded even when the repair stopped part way: the items deleted so far are gone
			return flags.RecordOutcome(dynamodbClient, "repair.delete", flags.GlobalTarget, fmt.Sprintf("deleted %d legacy items (MachineInfo#, MachineConfig#)", deleted), err)
		},
	}
	RepairCmd.Flags().BoolVar(&deleteItems, "delete", false, "Delete the legacy items found")
	flags.AddYesFlag(RepairCmd)
	flags.AddDryRunFlag(RepairCmd)
}

// repair lists the legacy items, deleting them when delete is set, and returns how many it deleted
func repair(scanService scan.ScanService, deleter dynamodb.DeleteItemAPI, delete bool) (deleted int, err error) {
	pageSize := int32(10)

	input := awsdynamodb.ScanInput{
		ConsistentRead: aws.Bool(false),
		Limit:          aws.Int32(pageSize),
	}

	page := 0
	callback := func(out *awsdynamodb.ScanOutput) (err error) {
		page += 1
		fmt.Println("==========")
		fmt.Printf("Page #%d\n", page)
		fmt.Printf("  Scanned %d items\n", out.ScannedCount)
		fmt.Printf("  Discovered %d items\n", out.Count)

		var items []dynamodbItem
		err = attributevalue.UnmarshalListOfMaps(out.Items, &items)
		if err != nil {
			err = fmt.Errorf("failed to unmarshal output items: %w", err)
			return
		}

		for _, item := range items {
			if strings.HasPrefix(item.PartitionKey, "MachineInfo#") || strings.HasPrefix(item.PartitionKey, "MachineConfig#") {
				fmt.Printf("    ++ LEGACY ITEM LOCATED: (%+v | %+v) ++\n", item.PartitionKey, item.SortKey)
				if delete {
					_, err := deleter.DeleteItem(item.PrimaryKey)
					if err != nil {
						return fmt.Errorf("failed to delete item: %w", err)
					}
					deleted++
					fmt.Printf("      Item Deleted\n")
				}
			}
		}

		fmt.Println("")
		return nil
	}

	stop := func(out *awsdynamodb.ScanOutput) (bool, error) {
		// Don't stop until done.
		return false, nil
	}

	err = scanService.ScanAll(input, callback, stop)
	return deleted, err
}

type dynamodbItem struct {
	dynamodb.PrimaryKey
}
