output "api_gateway_authorizer_id" {
  value = aws_api_gateway_authorizer.api_authorizer.id
}

# Authorizer settings only take effect once the API is redeployed
output "api_gateway_authorizer_sha" {
  value = sha1(jsonencode(aws_api_gateway_authorizer.api_authorizer))
}

output "lambda_function_name" {
  value = module.authorizer_function.lambda_function_name
}

output "log_group_name" {
  value = module.authorizer_function.log_group_name
}
