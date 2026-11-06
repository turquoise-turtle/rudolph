package lookup

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	LookupCmd = &cobra.Command{
		Use:   "lookup",
		Short: "Find machine IDs by prefix or serial number, from the data Macs send when they sync",
	}
)

// printMachineIDs prints one machine ID per line, or says that none was found
func printMachineIDs(machineIDs []string, limit int32) {
	if len(machineIDs) == 0 {
		fmt.Println("no machine found")
		return
	}
	for _, machineID := range machineIDs {
		fmt.Println(machineID)
	}
	if int32(len(machineIDs)) >= limit {
		fmt.Printf("(showing the first %d; raise --limit to see more)\n", limit)
	}
}
