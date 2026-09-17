#
# REST API Authorizer
#
# This module covers the REST API Gateway Authorizer
#
resource "aws_api_gateway_authorizer" "api_authorizer" {
  name                   = "${var.prefix}_rudolph_lambda_authorizer" # Authorizers are scoped per gateway, so names don't conflict
  rest_api_id            = var.api_gateway_id
  authorizer_uri         = module.authorizer_function.lambda_invoke_arn
  authorizer_credentials = aws_iam_role.invocation_role.arn

  # Options defining how this authorizer operates
  type                             = "REQUEST" # Validate entire request

  # Santa sends "Authorization: Bearer <token>" through SyncExtraHeaders. Requests without the header are rejected
  # with a 401 by API Gateway without invoking the Lambda, and results are cached per token, so a host's whole sync
  # is served by one authorizer invocation. The Lambda returns a policy covering all sync routes for this reason.
  identity_source                  = "method.request.header.Authorization"
  authorizer_result_ttl_in_seconds = var.authorizer_result_ttl_in_seconds
}

#
# Lambda for authorizer
#
module "authorizer_function" {
  source = "../api-handler"

  prefix                    = var.prefix
  region                    = var.region
  alias_name                = var.alias_name
  lambda_source_bucket      = var.lambda_source_bucket
  lambda_source_key         = var.lambda_source_key
  lambda_source_hash        = var.lambda_source_hash
  endpoint                  = "authorizer"
  api_gateway_execution_arn = var.api_gateway_execution_arn
  log_retention_in_days     = var.log_retention_in_days

  env_vars = var.env_vars

}
