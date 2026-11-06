output "api_gateway_id" {
  value = aws_api_gateway_rest_api.api_gateway.id
}

output "api_gateway_execution_arn" {
  value = aws_api_gateway_rest_api.api_gateway.execution_arn
}

output "api_domain_name" {
  value = local.api_domain_name
}

output "raw_url" {
  value = aws_api_gateway_stage.stage.invoke_url
}

# For the separate web view, which lists execution events from this table: grant its role dynamodb:Query and
# dynamodb:GetItem on the table ARN and "<ARN>/index/*", and kms:Decrypt on store_kms_key_arn.
output "events_dynamodb_table_name" {
  value = module.rule_store.events_dynamodb_table_name
}

output "events_dynamodb_table_arn" {
  value = module.rule_store.events_dynamodb_table_arn
}

output "store_kms_key_arn" {
  value = module.rule_store.kms_key_arn
}
