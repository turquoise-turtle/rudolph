locals {
  firehose_name = var.eventupload_firehose_name == "" ? format("%s_rudolph_eventsupload_firehose", var.prefix) : var.eventupload_firehose_name
}

resource "aws_cloudwatch_log_group" "eventsupload_firehose" {
  name              = "/aws/kinesisfirehose/${local.firehose_name}"
  retention_in_days = var.log_retention_in_days
}

resource "aws_kinesis_firehose_delivery_stream" "eventsupload_firehose" {
  name        = local.firehose_name
  destination = "extended_s3"

  # Firehose keeps records it cannot deliver yet for up to 24 hours. Under the module's key, as the bucket is, rather
  # than an AWS owned key; Firehose creates a grant on the key when the stream is created or encryption starts.
  server_side_encryption {
    enabled  = true
    key_type = "CUSTOMER_MANAGED_CMK"
    key_arn  = aws_kms_key.rudolph_eventsupload_kms_key.arn
  }

  # Upgrading, the stream starts encrypting only once eventupload may use the key, so no upload is refused meanwhile
  depends_on = [aws_iam_role_policy_attachment.rudolph_firehose_eventupload_kms]

  extended_s3_configuration {
    role_arn    = aws_iam_role.eventsupload_firehose_role.arn
    bucket_arn  = aws_s3_bucket.rudolph_eventsupload_firehose.arn
    kms_key_arn = aws_kms_key.rudolph_eventsupload_kms_key.arn
    cloudwatch_logging_options {
      enabled        = true
      log_group_name = aws_cloudwatch_log_group.eventsupload_firehose.name
      # By reference, so the stream exists first: the role may not create streams
      log_stream_name = aws_cloudwatch_log_stream.s3_delivery.name
    }
    // TODO (@ryan) add logic here to tranform data for athena usage
    # processing_configuration {
    #   enabled = "true"

    #   processors {
    #     type = "Lambda"

    #     parameters {
    #       parameter_name  = "LambdaArn"
    #       parameter_value = "${module.firehose_processor.lambda_alias_invoke_arn.lambda_function_arn}:$LATEST"
    #     }
    #   }
    # }
  }
}
