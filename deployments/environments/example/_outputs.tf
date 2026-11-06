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
