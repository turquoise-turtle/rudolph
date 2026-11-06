#
# Example tf outputs file
#
#   This file is not strictly necessary in each environment directory, but you can
#   include it to print out your API's hostname at the end of each terraform apply
#   for convenience.
#
output "sync_base_url" {
  value = "https://${module.santa_api.api_domain_name}/"
}

output "events_dynamodb_table_arn" {
  value = module.santa_api.events_dynamodb_table_arn
}

output "events_dynamodb_table_name" {
  value = module.santa_api.events_dynamodb_table_name
}

# The key the DynamoDB tables are encrypted with; rudolph-web's role needs kms:Decrypt on it.
output "store_kms_key_arn" {
  value = module.santa_api.store_kms_key_arn
}

output "api_gateway_id" {
  value = module.santa_api.api_gateway_id
}

output "raw_url" {
  value = module.santa_api.raw_url
}
