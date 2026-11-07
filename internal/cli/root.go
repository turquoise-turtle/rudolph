package cli

import (
	"fmt"
	"os"

	"github.com/airbnb/rudolph/internal/cli/audit"
	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/cli/info"
	"github.com/airbnb/rudolph/internal/cli/lookup"
	"github.com/airbnb/rudolph/internal/cli/machine"
	"github.com/airbnb/rudolph/internal/cli/repair"
	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/cli/rules"
	"github.com/spf13/cobra"
)

// The command reference is docs/cli.md.

func init() {
	RootCmd.PersistentFlags().StringVar(&env, "ENV", "", "The Rudolph environment (deployment) to use; its configuration is read from deployments/environments/<env>/config.auto.tfvars.json or ~/.rudolph-cli/<env>.json. Defaults to the ENV environment variable")
	RootCmd.PersistentFlags().StringVar(&env, "env", "", "Same as --ENV")
	RootCmd.PersistentFlags().StringVar(&region, "region", "", "The AWS region of the deployment; overrides the environment's region (default: the environment's, or the REGION environment variable)")
	RootCmd.PersistentFlags().StringVar(&prefix, "prefix", "", "The deployment's prefix; overrides the environment's prefix")
	RootCmd.PersistentFlags().StringVar(&dynamodbTableName, "dynamodb_table", "", "The DynamoDB table of the deployment; overrides the environment's table (default: <prefix>_rudolph_store, or the DYNAMODB_NAME environment variable)")

	// Add subcommands
	info.InfoCmd.Annotations = map[string]string{optionalEnvironment: "true"}
	RootCmd.AddCommand(info.InfoCmd)
	RootCmd.AddCommand(rule.RuleCmd)
	RootCmd.AddCommand(rules.RulesCmd)
	RootCmd.AddCommand(config.ConfigCmd)
	RootCmd.AddCommand(repair.RepairCmd)
	RootCmd.AddCommand(lookup.LookupCmd)
	RootCmd.AddCommand(group.GroupCmd)
	RootCmd.AddCommand(machine.MachineCmd)
	RootCmd.AddCommand(machine.MachinesCmd)
	RootCmd.AddCommand(audit.AuditCmd)
}

var (
	env               string
	region            string
	prefix            string
	dynamodbTableName string
)

// noEnvironment is the annotation of commands that do not read or write the sync server's table, so need no
// environment
const noEnvironment = "rudolph/no-environment"

// optionalEnvironment is the annotation of commands that read the table when an environment is configured and still
// work without one (they see empty --region and --dynamodb_table flags)
const optionalEnvironment = "rudolph/optional-environment"

// RootCmd is the entry point command for the CLI, exported for use elsewhere
var RootCmd = &cobra.Command{
	Use:          "rudolph",
	Short:        "Manage the rules and configuration of a Rudolph Santa sync server",
	SilenceUsage: true,
	Long: `Manage the rules and configuration of a Rudolph Santa sync server. Commands read and write the server's
DynamoDB table, found from the environment given with --env (or ENV), or from --region and --dynamodb_table.
See docs/cli.md.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Annotations[noEnvironment] != "" {
			return nil
		}
		_, err := retrieveConfig(cmd)
		if err != nil && cmd.Annotations[optionalEnvironment] != "" {
			// Only worth a warning when an environment was asked for: without one, the command says how to set it
			if flagString(cmd, "ENV") != "" || flagString(cmd, "env") != "" || os.Getenv("ENV") != "" {
				fmt.Fprintln(os.Stderr, "[WARNING]", err)
			}
			return nil
		}
		return err
	},
}

// Execute is the main entry point for the CLI
func Execute(version string) {
	RootCmd.Version = version
	if err := RootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
