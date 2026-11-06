# Setting Up New Environments
It is easy to deploy and maintain multiple different Rudolph environments, as they are configured in just 6 simple files.
This document will walk through the files necessary.

## `init-env`
The easiest way to create skeletons for all necessary files is via:

```
make init-env
```

## The Directory
You will need a directory under `deployments/environments/{{ENV}}/`. For example, this current environment's `ENV` is "example", as it is located at `deployments/environment/example`.

### `main.tf`
Instantiates the Rudolph module from this checkout:

```
module "santa_api" {
  source = "../../terraform_modules/santa_api"
  ...
}
```

Always keep the local `source`. Pointing it at a remote repository (for example `github.com/airbnb/rudolph//...`)
deploys whatever is on that repository's default branch instead of the code you built and tested.

### `_variables.tf`
Declares the variables that `config.auto.tfvars.json` and `make deploy` provide, and passes them to the module in
`main.tf`. `make init-env` copies both files from this example; when you upgrade Rudolph, compare them with the example
to pick up new settings.

### `versions.tf`
Pins Terraform and the AWS provider. Use the same constraint as the module (`deployments/terraform_modules/santa_api/_providers.tf`):
```
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}
```

### `_backend.tf`
The first "complicated" file you'll need to set up is `_backend.tf` which describes how the Terraform state will be stored.

Consider the provided example:
```
terraform {
  backend "s3" {
    bucket     = "your-terraform-tfstate-bucket"
    key        = "rudolph-example-prefix.tfstate"
    region     = "us-east-1"
    acl        = "private"
    encrypt    = true
    kms_key_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
  }
}

provider "aws" {
  region = "us-east-1"
  default_tags {
   tags = {
    Name = "Rudolph"
   }
  }
}
```

#### `bucket`
This must be a preexisting AWS S3 bucket in your AWS account. It is used for storing the Terraform statefile.
Sharing the same bucket across multiple deployments of Rudolph is OK.

#### `key`
This is the S3 bucket key in which your statefile is stored. It must be writable and must not be in use
by anything else, especially another Rudolph deployment.

We generally recommend including your `{{ENV}}` in some capacity in the key, to properly namespace it.

#### `region`
This is the AWS region into which you deploy Rudolph. Make sure this region is consistent everywhere else.

#### `acl`
Always put "private"!

#### `encrypt`
If you wish to encrypt your TF State with serverside encryption, set this to `true`.

#### `kms_key_id`
Only necessary if `encrypt = true`. Provide the KMS Key Id of an existing KMS key on the AWS account that
will be used for serverside encryption. The key must be usable by your IAM user.

### `config.auto.tfvars.json`
You **must** name this file exactly as specified above.

This file contains configurations that determine how your Rudolph deployment works.

Consider this provided example:
```
{
  "aws_account_id": "001122334455",
  "org": "acme",
  "prefix": "production",
  "stage_name": "prod",
  "region": "us-west-2",
  "eventupload_handler": "NONE",
  "eventupload_kinesis_name": "",
  "eventupload_autocreate_policies": false,
  "route53_zone_name": "mywebsite.com",
  "use_existing_route53_zone": true,
  "eventupload_output_lambda_name": "",
  "sync_token_sha256": ["<64 hex characters>"],
  "authorizer_result_ttl_in_seconds": 300,
  "mtls_truststore_uri": "",
  "mtls_truststore_version": "",
  "disable_execute_api_endpoint": false,
  "log_retention_in_days": 30,
  "api_throttling_rate_limit": 2000,
  "api_throttling_burst_limit": 2000,
  "alarm_sns_topic_arn": "",
  "api_4xx_alarm_min_requests": 100
}
```

#### `aws_account_id`
Your AWS account's ID.

#### `org`
This string should be unique to your team or organization. This is used to prevent certain AWS resource Id collisions.
It is highly recomended to be universally unique.

#### `prefix`
This string prefixes all AWS resource names and identifiers so you can distinguish between different deployment
environments within the same AWS account. It must be unique within a given AWS Account, and it is highly recommended
to make this universally unique.

#### `stage_name`
This is used for your Lambda aliases and your API Gateway stage names. You can just use `prod`.

#### `region`
The AWS region, which must match the previous 2 aws regions.

#### `eventupload_handler`
It takes a value of `NONE`, `KINESIS`, or `FIREHOSE`. Use `"NONE"` if you do not intend to send events to a sink. Whatever you choose, blocked and unknown executions are also stored in the execution events table for the web view (see [Execution events table](../../../docs/events-table.md)).

#### `eventupload_autocreate_policies`
If `true` Rudolph will automatically create sufficient IAM policies to the specified kinesis stream or firehose.

#### `route53_zone_name`
The domain name of the Route53 zone into which Rudolph will be deployed. You did remember to buy a domain name, right?

#### `use_existing_route53_zone`
Generally, the process of purchasing a domain name will automatically create a public hosted zone, so leave this as
`true`.

Specifying `false` will have Rudolph automatically create the Hosted Zone for you... but this can have issues later on
if you share the Hosted Zone between environments, as it becomes managed by Terraform. If you're not careful, Terraform
may accidentally destroy a Hosted Zone that's currently in use by another environment.

#### `eventupload_output_lambda_name`
An AWS Lambda Function name in the same region to forward `/eventupload` events to. Leave as `""` if you do not intend
to use this feature.

#### `sync_token_sha256`
Required. The SHA-256 hex digests of the bearer tokens Santa sends through `SyncExtraHeaders`. List two digests while
rotating tokens. Never put the tokens themselves here. See [configuring Santa](../../../docs/configuring-santa.md).

#### `authorizer_result_ttl_in_seconds`
How long API Gateway caches the authorizer's decision for a token. Defaults to `300`. A removed token can keep working
for this long.

#### `mtls_truststore_uri` and `mtls_truststore_version`
Optional. An `s3://` URI of a PEM bundle of CA certificates, and optionally its S3 object version. When set, the custom
domain requires client certificates issued by those CAs, and the default `execute-api` endpoint is disabled.

#### `disable_execute_api_endpoint`
Disable the default `execute-api` endpoint even without mTLS, so hosts can only use the custom domain. Defaults to `false`.

#### `log_retention_in_days`
Retention of the Lambda and Firehose CloudWatch log groups. Defaults to `30`.

#### `api_throttling_rate_limit` and `api_throttling_burst_limit`
Requests per second, and requests at once, that API Gateway accepts on each route before answering HTTP 429. Both default to `2000`. See [Deployment Variables](../../../docs/deploy.md#deployment-variables) before changing them.

#### `alarm_sns_topic_arn`
Optional. An SNS topic notified when one of Rudolph's CloudWatch alarms fires or recovers. Leave as `""` to create the alarms without notifications.

#### `api_4xx_alarm_min_requests`
The `api_4xx` alarm ignores 5-minute periods with fewer requests than this. Defaults to `100`, which covers fleets from about 40 Macs; lower it for smaller fleets.
