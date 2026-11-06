output "dynamodb_table_arn" {
    value = aws_dynamodb_table.store.arn
}

output "dynamodb_table_name" {
  value = aws_dynamodb_table.store.name
}

output "events_dynamodb_table_name" {
  value = aws_dynamodb_table.events.name
}

output "events_dynamodb_table_arn" {
  value = aws_dynamodb_table.events.arn
}

# KMS key of both tables. A reader of the events table (the web view) needs kms:Decrypt on it, through DynamoDB.
output "kms_key_arn" {
  value = aws_kms_key.store_sse_key.arn
}
