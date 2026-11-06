package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// environmentFileDirs are where an environment's config.auto.tfvars.json is looked for, relative to the working
// directory: deployments/environments/<env>/ is where scripts/new_env.sh creates it in a checkout of this repo; the
// terraform/ directories are where earlier versions kept it.
var environmentFileDirs = []string{
	"deployments/environments",
	"terraform/deployments",
	"terraform/environments",
}

type environmentConfiguration struct {
	Prefix       string `json:"prefix"`
	Region       string `json:"region"`
	AWSAccountID string `json:"aws_account_id"`
	// DynamoDBTableName overrides the store module's table name, which is otherwise <prefix>_rudolph_store
	DynamoDBTableName string `json:"dynamodb_table_name"`
}

// tableName returns the name of the DynamoDB table the environment's sync server uses
func (c environmentConfiguration) tableName() string {
	if c.DynamoDBTableName != "" {
		return c.DynamoDBTableName
	}
	return fmt.Sprintf("%s_rudolph_store", c.Prefix)
}

func fileExists(name string) bool {
	info, err := os.Stat(name)
	return err == nil && !info.IsDir()
}

// environmentFiles returns the files an environment's configuration is read from, in order of preference
func environmentFiles(env string) []string {
	var files []string
	for _, dir := range environmentFileDirs {
		files = append(files, filepath.Join(dir, env, "config.auto.tfvars.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".rudolph-cli", env+".json"))
	}
	return append(files, env+".json")
}

// flagString returns the value of a flag, or "" when cmd has no such flag
func flagString(cmd *cobra.Command, name string) string {
	if cmd.Flags().Lookup(name) == nil {
		return ""
	}
	value, _ := cmd.Flags().GetString(name)
	return value
}

// typedString returns the value of a flag given on the command line, or ""
func typedString(cmd *cobra.Command, name string) string {
	if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
		return f.Value.String()
	}
	return ""
}

// setUnlessGiven sets a flag that was not given on the command line, so a typed --region, --prefix or
// --dynamodb_table wins over the environment's configuration
func setUnlessGiven(cmd *cobra.Command, name, value string) {
	if f := cmd.Flags().Lookup(name); f != nil && !f.Changed {
		_ = cmd.Flags().Set(name, value)
	}
}

// retrieveConfig reads the configuration of the environment named by --ENV (or --env, or the ENV environment
// variable) and sets --region, --prefix and --dynamodb_table from it, unless they were given. Without an environment
// file, --prefix gives the table <prefix>_rudolph_store, and the DYNAMODB_NAME and REGION environment variables (as the
// Lambdas use them) stand in for --dynamodb_table and --region.
func retrieveConfig(cmd *cobra.Command) (*environmentConfiguration, error) {
	env := flagString(cmd, "ENV")
	if env == "" {
		env = flagString(cmd, "env")
	}
	if env == "" {
		env = os.Getenv("ENV")
	}

	if env != "" {
		for _, file := range environmentFiles(env) {
			if !fileExists(file) {
				continue
			}
			config, err := readEnvironmentFile(file)
			if err != nil {
				return nil, err
			}
			// A typed --prefix names another deployment, so the file's table (and its override) is not that one's
			if typed := typedString(cmd, "prefix"); typed != "" {
				config.Prefix = typed
				config.DynamoDBTableName = ""
			}
			setUnlessGiven(cmd, "prefix", config.Prefix)
			setUnlessGiven(cmd, "region", config.Region)
			setUnlessGiven(cmd, "dynamodb_table", config.tableName())
			return config, nil
		}
	}

	// Without an environment file: what was typed, else the variables the Lambdas are configured with
	config := &environmentConfiguration{
		Prefix:            flagString(cmd, "prefix"),
		Region:            flagString(cmd, "region"),
		DynamoDBTableName: flagString(cmd, "dynamodb_table"),
	}
	if config.Region == "" {
		config.Region = os.Getenv("REGION")
	}
	if config.DynamoDBTableName == "" && config.Prefix != "" {
		config.DynamoDBTableName = config.tableName()
	}
	if config.DynamoDBTableName == "" {
		config.DynamoDBTableName = os.Getenv("DYNAMODB_NAME")
	}
	if config.Region != "" && config.DynamoDBTableName != "" {
		setUnlessGiven(cmd, "region", config.Region)
		setUnlessGiven(cmd, "dynamodb_table", config.DynamoDBTableName)
		return config, nil
	}

	if env == "" {
		return nil, errors.New("no environment: pass --env <environment> (or set ENV), or give --region and --dynamodb_table (or set REGION and DYNAMODB_NAME)")
	}
	return nil, fmt.Errorf("no configuration for environment %q: looked for %s", env, strings.Join(environmentFiles(env), ", "))
}

// readEnvironmentFile reads an environment's configuration, which needs a region and a prefix or table name
func readEnvironmentFile(file string) (*environmentConfiguration, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	config := &environmentConfiguration{}
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if strings.TrimSpace(config.Region) == "" {
		return nil, fmt.Errorf("%s: region is empty; set \"region\" to the AWS region of the deployment", file)
	}
	if config.Prefix == "" && config.DynamoDBTableName == "" {
		return nil, fmt.Errorf("%s: prefix is empty; set \"prefix\" (or \"dynamodb_table_name\")", file)
	}
	return config, nil
}
