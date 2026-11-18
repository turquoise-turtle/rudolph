output "lambda_function_arn" {
  value = aws_lambda_function.api_handler.arn
}

output "lambda_alias_invoke_arn" {
  value = aws_lambda_alias.api_handler.invoke_arn
}

output "lambda_invoke_arn" {
  value = aws_lambda_function.api_handler.invoke_arn
}

output "lambda_role_name" {
  value = aws_iam_role.api_handler_role.id
}

# With its /rudolph/ path, which an ARN built from the name would leave out
output "lambda_role_arn" {
  value = aws_iam_role.api_handler_role.arn
}

output "lambda_function_name" {
  value = aws_lambda_function.api_handler.function_name
}

output "log_group_name" {
  value = aws_cloudwatch_log_group.api_handler.name
}
