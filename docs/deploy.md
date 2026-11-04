# Deployment
Ready to build Rudolph? Let's go at it!

---

## Getting Ready
You will only need to do this stuff once.

### Golang
You will need a recent golang version (see `go.mod`) installed. Go get it from [the golang website](https://golang.org/dl/).

### Amazon Web Services
You will need an AWS account handy, as well as an IAM role with sufficient privileges.

### Terraform
You will need Terraform v1.5+ installed (tested with 1.16). We recommend using [tfenv](https://github.com/tfutils/tfenv).
Rudolph uses the `hashicorp/aws` provider `~> 6.0`.

### Registered Domain
You will need a registered DNS domain name for Rudolph's API server.

Purchasing a domain is very easy, simply use AWS's Route 53 service to register a new domain. The exact URL
is rarely seen by end users of Santa-Rudolph, so we advise picking any random cheap one to start things up quickly.

---

## Initial Setup
First, you will need to set up a new environment.

### Pick an ENV
Pick a new environment name for your deployment.

An environment is a unique instance of Rudolph is associated with a filesystem directory under [deployments/environments](/deployments/environments/).

### EXPORT your ENV
When working with a specific deployment environment, always make sure your `ENV` environment variable is set
properly.

For example, to direct commands at the `deployments/environment/example` environment, you would use:

```
export ENV=example
```

### Create your Terraform Files
You will now create all necessary Terraform configuration files. For convenience, you can set up everything via:

```
make init-env
```

You will end up creating:

* A new directory under `deployments/environments`
* A `main.tf` file, which uses the modules in this checkout (`../../terraform_modules/santa_api`)
* A `_variables.tf` file
* A `versions.tf` file
* A `_backend.tf` file
* A `config.auto.tfvars.json` file
* (Optional) A `_outputs.tf` file

You will also need to edit some of these files. A good resource to understanding these configuration files
is the example ENV's [`README.md`](/deployments/environments/example/README.md).

### Create a Sync Token
Every Santa request must carry a bearer token. Generate one and put its SHA-256 digest (never the token itself) into
`sync_token_sha256` in `config.auto.tfvars.json`:

```
TOKEN="$(openssl rand -base64 32)"
printf '%s' "$TOKEN" | shasum -a 256 | cut -d ' ' -f 1   # macOS; on Linux use sha256sum instead of shasum -a 256
```

See [configuring Santa](/docs/configuring-santa.md) for how hosts send the token, how to rotate it and how to enable
client certificates (mTLS).

### Deployment Variables
These `config.auto.tfvars.json` settings control authentication and logging:

| Variable | Default | Description |
|---|---|---|
| `sync_token_sha256` | (required) | List of SHA-256 hex digests of valid sync tokens. Two entries while rotating. |
| `authorizer_result_ttl_in_seconds` | `300` | How long API Gateway caches the authorizer's decision per token (0-3600). `0` invokes the authorizer Lambda on every request. |
| `mtls_truststore_uri` | `""` | `s3://` URI of a PEM truststore. When set, the custom domain requires client certificates. |
| `mtls_truststore_version` | `""` | S3 object version of the truststore. |
| `disable_execute_api_endpoint` | `false` | Disable the default `execute-api` endpoint so only the custom domain works. Always disabled when mTLS is enabled. |
| `log_retention_in_days` | `30` | Retention of the Lambda and Firehose CloudWatch log groups. |


### Install Dependencies
Download and install all golang dependencies with:

```
make deps
```

### First Build and Deploy
You can deploy your entire application now. Luckily, this step is super easy! Do not forget to authenticate with aws-cli.

```
make deploy
```

Upon deploying successfully, you will see the `sync_base_url` output:

> Apply complete! Resources: 1 added, 14 changed, 1 destroyed.
>
> Outputs:
>
> sync_base_url = "https://production-rudolph.acme.inc/"

**_Remember this value_**, you will need it later when configuring Santa sensors.


### Test it Out
Make an HTTP POST request to your new deployment. The request should hit a URL that looks something like:

```
POST /preflight/AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE
Host: {{PREFIX}}-rudolph.{YOUR_DOMAIN_NAME}
authorization:Bearer {{YOUR_SYNC_TOKEN}}
content-type:application/json
accept:*/*
{
  "serial_num": "1234"
}
```

Without a valid `authorization` header the API answers `401`. `GET /health` also requires the token.

---

## Future Deployments
Deploying code and making any infrastructure changes is easy:

```
export ENV=YOURENV
make deploy
```

---

## Upgrading an Existing Deployment
Deployments created before Santa 2026.8 support need a few one-time steps.

### 0. Deploy code and infrastructure together
This release changes the Terraform configuration and the Lambda handlers in ways that only work together. Always run
`make deploy` from a checkout that contains both, never apply the Terraform changes on top of an older build:

- `binary_media_types = ["application/json"]` makes API Gateway pass Santa's (deflate-compressed) request bodies to
  Lambda base64 encoded. Only handlers that base64-decode and decompress the body can read them; deploying this setting
  with older handlers makes **every sync fail**.
- Older builds of the API router, eventupload and xsrf handlers log the full request including its headers, which
  would write the sync token to CloudWatch Logs.

### 0b. Update anything that reads the event stream
The eventupload handler now forwards everything Santa 2026.8 sends, which changes the records that arrive in Firehose,
Kinesis or your own Lambda:

- Execution event records gain the fields Rudolph used to drop: `entitlement_info`, `cs_flags`, `signing_status`,
  `secure_signing_time`, `signing_time`, `static_rule` and the remaining `quarantine_*` fields.
- Two **new record shapes** share the same stream: file access events (`rule_name`, `rule_version`, `target`,
  `process_chain`, `access_time`, `decision`) and audit events (`standalone_mode_rule_creation`). They do not have
  `file_sha256` or `execution_time`, and `decision` on a file access record is a `FILE_ACCESS_DECISION_*` value, not
  an execution decision.
- Every record therefore carries **`event_type`**, one of `execution`, `file_access` or `audit`, alongside
  `machine_id`.

Before deploying, make consumers tolerate unknown record shapes: filter on `event_type = 'execution'` in queries and
dashboards that assume execution events, and add the new columns to any Glue/Athena schema (a table with a fixed
schema will otherwise drop the new fields or fail on the new shapes).

An event too large for the sink to ever store (larger than a Firehose or Kinesis record, or a Lambda async payload)
is now dropped instead of failing the upload, so it no longer blocks every later event from that Mac. Each drop is
logged in the eventupload function's log group with a line containing `dropped` (counts, machine ID and reason only);
add a metric filter on it if you need to know how often it happens. Consumers may also see an event twice: when the
sink fails part of an upload, Santa re-sends the whole batch. See [Delivery to the event sink](sync.md#delivery-to-the-event-sink).

### 0c. Alarm on rejected request bodies
Sync handlers now refuse a request body that decompresses to more than 6 MiB with HTTP 413, so that a compressed
payload cannot make a Lambda allocate unbounded memory. Normal syncs are orders of magnitude smaller, but a host that
uploads a very large batch of events (a bundle scan of a huge application, or a long backlog after an outage) could hit
the limit and then never get those events uploaded. Add a CloudWatch alarm on 413 responses, for example a metric
filter on the API Gateway access or execution logs, or the `4XXError` metric for the `/eventupload/{machine_id}`
resource, so a host stuck in that state is noticed rather than silently dropping events.

### 1. Roll out the sync token
Once deployed, the API rejects every request without a valid token. Create a token (see above) and add its digest to
`sync_token_sha256`. Hosts must run Santa 2026.8+ and receive the `SyncExtraHeaders` configuration profile.

**Do not issue the token to hosts while an older build (one that logs request headers) is deployed.** Deploy this
release first, then push the configuration profile. Hosts keep enforcing their current rules but cannot sync until they
receive the profile.

If you cannot accept that gap and push the profile before upgrading, or if a token was ever used with an older build,
treat the token as leaked: after the upgrade, rotate it (see [configuring Santa](/docs/configuring-santa.md)) and
delete the old log streams in the `/aws/lambda/PREFIX_rudolph_*` log groups.

### 2. Fix the environment directory
Environments created by the old `make init-env` contain `main.tf` and `variables.tf` symlinks to
`deployments/terraform_modules/default_main.tf` and `default_variables.tf`, which no longer exist. Replace them with copies
of the example environment's files and add the new settings to `config.auto.tfvars.json`:

```
cd deployments/environments/$ENV
rm main.tf variables.tf
cp ../example/main.tf ../example/_variables.tf ./
```

If your `main.tf` sourced `github.com/airbnb/rudolph//deployments/terraform_modules/santa_api`, change the source to
`../../terraform_modules/santa_api` so you deploy the code in your checkout. Copy the example's `versions.tf` as well.

### 3. Upgrade the AWS provider
The AWS provider moved from `5.15.0` (or `~> 3.76.1`) to `~> 6.0`. Update the dependency lock file:

```
terraform -chdir=deployments/environments/$ENV init -upgrade
```

### 4. Import resources that AWS created implicitly
Terraform now manages two kinds of resources that already exist in older deployments. Without importing them, `apply`
fails with "already exists" errors. The simplest way is a temporary `imports.tf` in your environment directory
(Terraform 1.5+), replacing `PREFIX`, `STAGE` and `REST_API_ID` (the `api_gateway_id` of your deployment):

```hcl
# The API Gateway stage, previously created through the deployment's stage_name
import {
  to = module.santa_api.aws_api_gateway_stage.stage
  id = "REST_API_ID/STAGE"
}

# Lambda log groups, previously created by Lambda without retention
import {
  to = module.santa_api.module.rudolph_api_authorizer.module.authorizer_function.aws_cloudwatch_log_group.api_handler
  id = "/aws/lambda/PREFIX_rudolph_authorizer"
}
import {
  to = module.santa_api.module.health_function.aws_cloudwatch_log_group.api_handler
  id = "/aws/lambda/PREFIX_rudolph_health"
}
import {
  to = module.santa_api.module.xsrf_function.aws_cloudwatch_log_group.api_handler
  id = "/aws/lambda/PREFIX_rudolph_xsrf"
}
import {
  to = module.santa_api.module.preflight_function.aws_cloudwatch_log_group.api_handler
  id = "/aws/lambda/PREFIX_rudolph_preflight"
}
import {
  to = module.santa_api.module.eventupload_function.aws_cloudwatch_log_group.api_handler
  id = "/aws/lambda/PREFIX_rudolph_eventupload"
}
import {
  to = module.santa_api.module.ruledownload_function.aws_cloudwatch_log_group.api_handler
  id = "/aws/lambda/PREFIX_rudolph_ruledownload"
}
import {
  to = module.santa_api.module.postflight_function.aws_cloudwatch_log_group.api_handler
  id = "/aws/lambda/PREFIX_rudolph_postflight"
}
```

Skip a log group import if the function has never been invoked (the group does not exist yet). Run `make plan`, check
that only the expected changes are listed, `make deploy`, then delete `imports.tf`.

The deployment also adds the `DataType_MachineID` index to the DynamoDB table and moves the Lambda source bucket
settings to separate `aws_s3_bucket_*` resources; neither needs manual steps. Creating the index backfills it from every
existing item that has `DataType` and `MachineID`: this takes a while on large tables, and on the on-demand
(`PAY_PER_REQUEST`) table the backfill is billed as write requests to the index, roughly one per indexed item.
