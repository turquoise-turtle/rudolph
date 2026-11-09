output "integration_id" {
  value = aws_api_gateway_integration.resource_integration.id
}

# Hashes the configuration inputs, not the resources: AWS fills in computed attributes after the first apply, and
# hashing those replaced the API Gateway deployment on every apply.
output "integration_sha" {
  value = sha1(jsonencode([
    var.gateway_resource_id,
    var.http_method,
    var.lambda_invocation_arn,
    var.authorizer_id,
    var.request_model,
    var.success_response_model,
  ]))
}
