output "dynamodb_table_arn" {
    value = aws_dynamodb_table.store.arn
}

output "dynamodb_table_name" {
  value = aws_dynamodb_table.store.name
}
