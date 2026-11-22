variable "lambda_api_zip" {
  type        = string
  description = "Full path to zip with go binary for Lambda to be uploaded to S3"
}

variable "lambda_authorizer_zip" {
  type        = string
  description = "Full path to zip with go binary for Lambda to be uploaded to S3"
}

variable "lambda_ranker_zip" {
  type        = string
  description = "Full path to zip with the ranker's go binary (build/package/ranker_deployment.zip) to be uploaded to S3"
}

variable "lambda_fleet_zip" {
  type        = string
  description = "Full path to zip with the fleet job's go binary (build/package/fleet_deployment.zip) to be uploaded to S3"
}

// These variables are provided by config.auto.tfvars.json
variable "region" {
  type = string

  validation {
    condition     = can(regex("[a-z0-9\\-]+", var.region))
    error_message = "The aws region is not valid."
  }
}

variable "aws_account_id" {
  type = string

  validation {
    condition     = can(regex("[0-9]+", var.aws_account_id))
    error_message = "The aws_account_id should be a number."
  }
}

variable "org" {
  type        = string
  description = "A lowercase string unique to your organization. Used to deduplicate certain AWS resources."

  validation {
    condition     = can(regex("[a-z0-9\\-]+", var.org))
    error_message = "The org value must contain only lowercase alphanumeric characters or dashes."
  }
}

variable "stage_name" {
  type        = string
  description = "Name of stage to use for this deployment"

  validation {
    condition     = can(regex("[a-z\\-]+", var.stage_name))
    error_message = "The stage value must contain only lowercase alphabet characters."
  }
}

variable "prefix" {
  type        = string
  description = "Unique prefix to use for resources. This gets passed in from 'make deploy' command"

  validation {
    condition     = can(regex("[a-z0-9\\-]+", var.prefix))
    error_message = "The prefix value must contain only lowercase alphanumeric characters or dashes."
  }
}

variable "allowed_inbound_ips" {
  type        = list(string)
  description = "Restricts all inbound access to given ip addresses. By default, allows all inbound access."
  default     = []
}

variable "eventupload_handler" {
  type        = string
  description = "Type of handler to use for eventupload"
}

variable "eventupload_kinesis_name" {
  type        = string
  description = "Name of the kinesis stream"
}

variable "eventupload_autocreate_policies" {
  type    = bool
  default = true
}

variable "route53_zone_name" {
  type        = string
  description = "Route 53 Zone ID"
}

variable "use_existing_route53_zone" {
  type        = bool
  description = "Use exisiting Route 53 zone"
  default     = false
}

variable "eventupload_output_lambda_name" {
  type    = string
  default = ""
}

variable "enable_s3_logging" {
  type    = bool
  default = true
}

variable "kms_key_administrators_arns" {
  type        = list(string)
  description = "List of KMS Key Administrator ARNs to allow access to Rudolph KMS key operations"
  default     = []
}

variable "sync_token_sha256" {
  type        = list(string)
  description = "SHA-256 hex digests of the bearer tokens Santa sends via SyncExtraHeaders. List two digests while rotating. Never put plain tokens here."
  sensitive   = true
}

variable "authorizer_result_ttl_in_seconds" {
  type        = number
  description = "How long API Gateway caches the authorizer result for a token, in seconds (0-3600; 0 disables caching)"
  default     = 300
}

variable "mtls_truststore_uri" {
  type        = string
  description = "OPTIONAL: S3 URI (s3://bucket/key.pem) of a PEM truststore. When set, the custom domain requires client certificates (mTLS)."
  default     = ""
}

variable "mtls_truststore_version" {
  type        = string
  description = "OPTIONAL: S3 object version of the mTLS truststore"
  default     = ""
}

variable "disable_execute_api_endpoint" {
  type        = bool
  description = "Disable the default execute-api endpoint so clients must use the custom domain. Always disabled when mTLS is enabled."
  default     = false
}

variable "log_retention_in_days" {
  type        = number
  description = "Retention of Rudolph's CloudWatch log groups, in days"
  default     = 30
}

variable "api_throttling_rate_limit" {
  type        = number
  description = "Steady-state requests per second API Gateway accepts on each route (each method has its own limit) before answering 429"
  default     = 2000
}

variable "api_throttling_burst_limit" {
  type        = number
  description = "Requests API Gateway accepts at once on each route (each method's token bucket size) before answering 429"
  default     = 2000
}

variable "alarm_sns_topic_arn" {
  type        = string
  description = "OPTIONAL: ARN of an SNS topic that Rudolph's CloudWatch alarms notify"
  default     = ""
}

variable "api_4xx_alarm_min_requests" {
  type        = number
  description = "Requests in 5 minutes below which the api_4xx alarm does not fire"
  default     = 100
}

variable "monthly_budget_usd" {
  type        = number
  description = "Monthly budget in US dollars for the costs tagged Name = Rudolph; 0 means no budget"
  default     = 0
}

variable "budget_alert_emails" {
  type        = list(string)
  description = "Email addresses the monthly budget notifies"
  default     = []
}

variable "table_deletion_protection" {
  type        = bool
  description = "DynamoDB deletion protection on the rule store and events tables; set false and apply before make destroy"
  default     = true
}

variable "ranker_schedule_expression" {
  type        = string
  description = "When the ranker rebuilds the web view's top lists, e.g. rate(1 hour) or cron(0 8-18 ? * MON-FRI *)"
  default     = "rate(1 hour)"
}

variable "ranker_schedule_timezone" {
  type        = string
  description = "IANA time zone of ranker_schedule_expression, e.g. America/New_York"
  default     = "UTC"
}

variable "fleet_schedule_expression" {
  type        = string
  description = "When the fleet job rebuilds the web view's fleet health lists and Lockdown readiness figures, e.g. rate(1 day) or cron(0 6 * * ? *)"
  default     = "rate(1 day)"
}

variable "fleet_schedule_timezone" {
  type        = string
  description = "IANA time zone of fleet_schedule_expression, e.g. Australia/Sydney"
  default     = "UTC"
}

variable "fleet_alarm_days" {
  type        = number
  description = "Days in a row without a completed fleet job run before its alarm fires (1 to 7); the alarm needs alarm_sns_topic_arn"
  default     = 3
}

variable "firehose_events_retention_in_days" {
  type        = number
  description = "Days the Firehose events bucket keeps each event in total (at least 2): the object expires a day before this, and S3 deletes its noncurrent version 1 day later"
  default     = 180
}

variable "store_kms_key_user_arns" {
  type        = list(string)
  description = "OPTIONAL: IAM principal ARNs that may use the store tables' KMS key through DynamoDB besides Rudolph's Lambdas (rudolph-web's role, CLI operators). Empty lets every principal of the account."
  default     = []
}

variable "enable_api_access_logs" {
  type        = bool
  description = "Write an API Gateway access log line per request to /aws/apigateway/PREFIX_rudolph_access; needs the account's API Gateway CloudWatch role (aws apigateway get-account)"
  default     = false
}

variable "manage_api_gateway_account_role" {
  type        = bool
  description = "Create and set the account's API Gateway CloudWatch role, shared by every REST API in the account and region; only when no role is set and no other stack manages it"
  default     = false
}

variable "enable_waf" {
  type        = bool
  description = "Protect the stage with an AWS WAF web ACL: the AWS IP reputation list and a per-IP rate limit"
  default     = false
}

variable "waf_rate_limit_per_5_minutes" {
  type        = number
  description = "Requests from one source IP in 5 minutes above which the WAF blocks it; a fleet behind one NAT shares an IP"
  default     = 100000
}

variable "access_log_retention_days" {
  type        = number
  description = "Whole days after which each S3 access log expires (a delete marker); the versioned log is deleted 3 days later"
  default     = 180
}
