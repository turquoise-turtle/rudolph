output "api_gateway_authorizer_id" {
  value = aws_api_gateway_authorizer.api_authorizer.id
}

# Authorizer settings only take effect once the API is redeployed. Only configured attributes are hashed: the whole
# resource includes attributes AWS computes after apply, which replaced the deployment on every apply.
output "api_gateway_authorizer_sha" {
  value = sha1(jsonencode([
    aws_api_gateway_authorizer.api_authorizer.name,
    aws_api_gateway_authorizer.api_authorizer.type,
    aws_api_gateway_authorizer.api_authorizer.authorizer_uri,
    aws_api_gateway_authorizer.api_authorizer.authorizer_credentials,
    aws_api_gateway_authorizer.api_authorizer.identity_source,
    aws_api_gateway_authorizer.api_authorizer.authorizer_result_ttl_in_seconds,
  ]))
}

output "lambda_function_name" {
  value = module.authorizer_function.lambda_function_name
}

output "log_group_name" {
  value = module.authorizer_function.log_group_name
}
