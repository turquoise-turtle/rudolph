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

# KMS key of both tables. While store_kms_key_user_arns is empty, its key policy lets principals of the account use it
# through DynamoDB, so a reader of the events table (the web view) needs no extra KMS permission; once the list is
# set, or the policy otherwise tightened, the reader must be listed or granted the key through IAM.
output "kms_key_arn" {
  value = aws_kms_key.store_sse_key.arn
}
