package ddblocal

import (
	"fmt"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// StoreTerraformFile is the Terraform file that defines Rudolph's DynamoDB table. Integration tests create their table
// from it, so a key or index that production has (or lacks) is exactly what the tests run against.
func StoreTerraformFile() string {
	return filepath.Join(repoRoot(), "deployments", "terraform_modules", "santa_api", "modules", "store", "dynamodb.tf")
}

// TableSchema is the part of the Terraform aws_dynamodb_table "store" definition that DynamoDB Local can create.
type TableSchema struct {
	HashKey, RangeKey string
	// Attributes maps attribute names to their DynamoDB scalar type ("S", "N" or "B").
	Attributes map[string]string
	Indexes    []IndexSchema
}

// IndexSchema is a global secondary index of the table.
type IndexSchema struct {
	Name              string
	HashKey, RangeKey string
	ProjectionType    string
	NonKeyAttributes  []string
}

// EventsTerraformFile is the definition of the execution events table read by the web view (rudolph-web's
// docs/data-layer-design.md, R6).
func EventsTerraformFile() string {
	return filepath.Join(repoRoot(), "deployments", "terraform_modules", "santa_api", "modules", "store", "events.tf")
}

// LoadTableSchema reads the table definition from StoreTerraformFile.
func LoadTableSchema() (TableSchema, error) {
	return LoadTableSchemaFrom(StoreTerraformFile(), "store")
}

// LoadEventsTableSchema reads the aws_dynamodb_table "events" definition from EventsTerraformFile.
func LoadEventsTableSchema() (TableSchema, error) {
	return LoadTableSchemaFrom(EventsTerraformFile(), "events")
}

// LoadTableSchemaFrom reads the aws_dynamodb_table resource with the given name from a Terraform file.
func LoadTableSchemaFrom(path, resource string) (TableSchema, error) {
	file, diags := hclparse.NewParser().ParseHCLFile(path)
	if diags.HasErrors() {
		return TableSchema{}, fmt.Errorf("parse %s: %s", path, diags.Error())
	}
	body := file.Body.(*hclsyntax.Body)
	for _, block := range body.Blocks {
		if block.Type != "resource" || len(block.Labels) != 2 || block.Labels[0] != "aws_dynamodb_table" || block.Labels[1] != resource {
			continue
		}
		schema := TableSchema{Attributes: map[string]string{}}
		var err error
		if schema.HashKey, err = stringAttr(block.Body, "hash_key", true); err != nil {
			return schema, err
		}
		if schema.RangeKey, err = stringAttr(block.Body, "range_key", false); err != nil {
			return schema, err
		}
		for _, nested := range block.Body.Blocks {
			switch nested.Type {
			case "attribute":
				name, err := stringAttr(nested.Body, "name", true)
				if err != nil {
					return schema, err
				}
				typ, err := stringAttr(nested.Body, "type", true)
				if err != nil {
					return schema, err
				}
				schema.Attributes[name] = typ
			case "global_secondary_index":
				var index IndexSchema
				if index.Name, err = stringAttr(nested.Body, "name", true); err != nil {
					return schema, err
				}
				if index.HashKey, err = stringAttr(nested.Body, "hash_key", true); err != nil {
					return schema, err
				}
				if index.RangeKey, err = stringAttr(nested.Body, "range_key", false); err != nil {
					return schema, err
				}
				if index.ProjectionType, err = stringAttr(nested.Body, "projection_type", true); err != nil {
					return schema, err
				}
				if attr, ok := nested.Body.Attributes["non_key_attributes"]; ok {
					value, diags := attr.Expr.Value(nil)
					if diags.HasErrors() {
						return schema, fmt.Errorf("non_key_attributes of %s must be a literal list: %s", index.Name, diags.Error())
					}
					for it := value.ElementIterator(); it.Next(); {
						_, element := it.Element()
						index.NonKeyAttributes = append(index.NonKeyAttributes, element.AsString())
					}
				}
				schema.Indexes = append(schema.Indexes, index)
			case "local_secondary_index":
				return schema, fmt.Errorf("local secondary indexes are not supported by ddblocal yet")
			}
		}
		return schema, nil
	}
	return TableSchema{}, fmt.Errorf("no resource \"aws_dynamodb_table\" %q in %s", resource, path)
}

func stringAttr(body *hclsyntax.Body, name string, required bool) (string, error) {
	attr, ok := body.Attributes[name]
	if !ok {
		if required {
			return "", fmt.Errorf("%s is missing in the table definition", name)
		}
		return "", nil
	}
	value, diags := attr.Expr.Value(&hcl.EvalContext{})
	if diags.HasErrors() || value.IsNull() || !value.IsKnown() {
		return "", fmt.Errorf("%s must be a literal string in the table definition", name)
	}
	return value.AsString(), nil
}

// CreateTableInput builds the CreateTable request for a table with this schema.
func (s TableSchema) CreateTableInput(tableName string) *dynamodb.CreateTableInput {
	input := &dynamodb.CreateTableInput{
		TableName:   aws.String(tableName),
		BillingMode: types.BillingModePayPerRequest,
		KeySchema:   keySchema(s.HashKey, s.RangeKey),
	}
	for name, typ := range s.Attributes {
		input.AttributeDefinitions = append(input.AttributeDefinitions, types.AttributeDefinition{
			AttributeName: aws.String(name), AttributeType: types.ScalarAttributeType(typ),
		})
	}
	for _, index := range s.Indexes {
		projection := &types.Projection{ProjectionType: types.ProjectionType(index.ProjectionType)}
		if len(index.NonKeyAttributes) > 0 {
			projection.NonKeyAttributes = index.NonKeyAttributes
		}
		input.GlobalSecondaryIndexes = append(input.GlobalSecondaryIndexes, types.GlobalSecondaryIndex{
			IndexName:  aws.String(index.Name),
			KeySchema:  keySchema(index.HashKey, index.RangeKey),
			Projection: projection,
		})
	}
	return input
}

func keySchema(hashKey, rangeKey string) []types.KeySchemaElement {
	keys := []types.KeySchemaElement{{AttributeName: aws.String(hashKey), KeyType: types.KeyTypeHash}}
	if rangeKey != "" {
		keys = append(keys, types.KeySchemaElement{AttributeName: aws.String(rangeKey), KeyType: types.KeyTypeRange})
	}
	return keys
}
