variable "lambda_api_zip" {
  type        = string
  description = "Full path to zip with go binary for Lambda to be uploaded to S3"
}

variable "lambda_authorizer_zip" {
  type        = string
  description = "Full path to zip with go binary for Lambda to be uploaded to S3"
}

// These variables are provided by config.auto.tfvars.json
variable "region" {
  type    = string

  validation {
    condition     = can(regex("[a-z0-9\\-]+", var.region))
    error_message = "The aws region is not valid."
  }
}

variable "aws_account_id" {
  type    = string

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
  type = string
  description = "Type of handler to use for eventupload"
}

variable "eventupload_kinesis_name" {
  type = string
  description = "Name of the kinesis stream"
}

variable "eventupload_autocreate_policies" {
  type = bool
  default = true
}

variable "route53_zone_name" {
  type = string
  description = "Route 53 Zone ID"
}

variable "use_existing_route53_zone" {
  type = bool
  description = "Use exisiting Route 53 zone"
  default = false
}

variable "eventupload_output_lambda_name" {
  type = string
  default = ""
}

variable "enable_s3_logging" {
  type = bool
  default = true
}

variable "kms_key_administrators_arns" {
  type = list(string)
  description = "List of KMS Key Administrator ARNs to allow access to Rudolph KMS key operations"
  default = []
}

variable "sync_token_sha256" {
  type        = list(string)
  description = "SHA-256 hex digests of the bearer tokens Santa sends via SyncExtraHeaders. List two digests while rotating. Never put plain tokens here."
  sensitive   = true
}

variable "authorizer_result_ttl_in_seconds" {
  type        = number
  description = "How long API Gateway caches the authorizer result for a token, in seconds (1-3600)"
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
  description = "Steady-state requests per second the API stage accepts before answering 429"
  default     = 2000
}

variable "api_throttling_burst_limit" {
  type        = number
  description = "Requests the API stage accepts in a burst above api_throttling_rate_limit"
  default     = 4000
}

variable "alarm_sns_topic_arn" {
  type        = string
  description = "OPTIONAL: ARN of an SNS topic that Rudolph's CloudWatch alarms notify"
  default     = ""
}
