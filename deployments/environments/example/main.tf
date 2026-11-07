module "santa_api" {
  # Always deploy the modules from this checkout. Environments live in deployments/environments/<ENV>.
  source = "../../terraform_modules/santa_api"

  prefix              = var.prefix
  org                 = var.org
  region              = var.region
  aws_account_id      = var.aws_account_id
  stage_name          = var.stage_name
  allowed_inbound_ips = var.allowed_inbound_ips

  # Santa client authentication
  sync_token_sha256                = var.sync_token_sha256
  authorizer_result_ttl_in_seconds = var.authorizer_result_ttl_in_seconds
  mtls_truststore_uri              = var.mtls_truststore_uri
  mtls_truststore_version          = var.mtls_truststore_version
  disable_execute_api_endpoint     = var.disable_execute_api_endpoint

  # StreamAlert integration
  eventupload_handler             = var.eventupload_handler
  eventupload_kinesis_name        = var.eventupload_kinesis_name
  eventupload_autocreate_policies = var.eventupload_autocreate_policies
  eventupload_output_lambda_name  = var.eventupload_output_lambda_name

  # The route53 zone id
  route53_zone_name         = var.route53_zone_name
  use_existing_route53_zone = var.use_existing_route53_zone

  lambda_api_zip        = var.lambda_api_zip
  lambda_authorizer_zip = var.lambda_authorizer_zip
  lambda_ranker_zip     = var.lambda_ranker_zip

  enable_s3_logging = var.enable_s3_logging

  log_retention_in_days = var.log_retention_in_days

  kms_key_administrators_arns = var.kms_key_administrators_arns

  # Operations
  api_throttling_rate_limit  = var.api_throttling_rate_limit
  api_throttling_burst_limit = var.api_throttling_burst_limit
  alarm_sns_topic_arn        = var.alarm_sns_topic_arn
  api_4xx_alarm_min_requests = var.api_4xx_alarm_min_requests

  # Cost and data protection
  monthly_budget_usd        = var.monthly_budget_usd
  budget_alert_emails       = var.budget_alert_emails
  table_deletion_protection = var.table_deletion_protection

  # Ranker
  ranker_schedule_expression = var.ranker_schedule_expression
  ranker_schedule_timezone   = var.ranker_schedule_timezone
}
