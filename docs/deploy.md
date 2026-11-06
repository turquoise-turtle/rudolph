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
| `api_throttling_rate_limit` | `2000` | Requests per second API Gateway accepts **on each route** before answering HTTP 429: each of the 6 methods has its own limit, so all routes together can take 6 times this. A sync is 4-6 requests about every 10 minutes, one or two per route, so 100,000 Macs send each route about 170-330 requests per second on average. It is a ceiling against floods, not a way to shed load: Santa retries a 429 (see [upgrading](#0d-check-the-api-throttle)). It does not protect other APIs in the account (see there). |
| `api_throttling_burst_limit` | `2000` | Requests accepted at once on each route before answering HTTP 429 (each method's token bucket size). Keep it at least the rate limit; the account's regional burst limit (5,000 by default) caps all routes together. |
| `alarm_sns_topic_arn` | `""` | SNS topic notified when an alarm fires or recovers. Without it the alarms exist in CloudWatch but notify no one. |
| `api_4xx_alarm_min_requests` | `100` | The `api_4xx` alarm ignores 5-minute periods with fewer requests than this. A Mac sends about 2-3 requests every 5 minutes, so the default covers fleets from about 40 Macs; lower it for smaller fleets, at the cost of alarms on a few scanner requests. |

### Alarms
Rudolph creates these CloudWatch alarms (names start with `PREFIX_rudolph_`):

| Alarm | Fires when |
|---|---|
| `api_5xx` | API Gateway answered 5 or more 5xx responses in 5 minutes. |
| `api_4xx` | More than 5% of requests were answered 4xx in two 5-minute periods in a row, counting only periods with at least `api_4xx_alarm_min_requests` requests (default 100, so that a few scanner requests in a quiet night do not fire it; fleets below about 40 Macs need a lower value to be covered): the stage is throttling (429; raise `api_throttling_rate_limit`) or the authorizer is rejecting sync tokens (401/403, e.g. after a botched token rotation). Throttled requests never reach Lambda, so this is the only alarm that sees them. |
| `request_too_large` | A handler refused a request body above 6 MiB with HTTP 413. That host cannot sync or upload those events until the cause is fixed. |
| `lambda_errors` | Any Rudolph Lambda function, the hourly ranker included, returned an error. |
| `lambda_timeouts` | Any Rudolph Lambda function timed out (`Task timed out` in its log group). |
| `dynamodb_throttles` | Reads or writes to the table were throttled. |
| `event_sink_failures` | eventupload could not forward events to Firehose, Kinesis or the output Lambda (only created when a sink is configured). |

The 413, timeout and event sink alarms use metric filters on the Lambda log groups, published in the
`Rudolph/PREFIX` namespace. Request bodies above API Gateway's own 10 MB limit are refused before reaching Lambda and
only show up in API Gateway's `4XXError` metric (and so in `api_4xx`).

The filters count the one line the API router logs per request, such as `POST /eventupload/{machine_id} machine=... status=413 duration=...`. The machine ID comes from the request path, so it is logged only when it is a UUID (otherwise `machine=invalid`); a client cannot add text or lines to the log that way. When a handler answers 500, it logs the cause on the line before, as `internal server error: "..."`, and never puts it in the response: `GET /health` answers `{"status":"unhealthy"}` without the AWS error.


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
the limit and then never get those events uploaded. The `request_too_large` alarm (see [Alarms](#alarms)) fires on
413 responses so a host stuck in that state is noticed rather than silently dropping events; set
`alarm_sns_topic_arn` to be notified.

### 0d. Check the API throttle
The API is now throttled per route: when one route (preflight, eventupload, ruledownload, postflight, xsrf or health) gets more than `api_throttling_rate_limit` requests per second (default 2,000, burst 2,000), API Gateway answers HTTP 429 before the request reaches Lambda. Each route has its own limit; they are not shared. Santa does not wait for its next sync after a 429: it retries the request within the same sync stage, up to 5 attempts in all, backing off 4, 8, 16 and 32 seconds, and fails the sync only when the last attempt is throttled too. A throttle set below the fleet's peak load therefore does not shed load: throttled Macs come back within a minute and add retries on top of the normal traffic, and syncs that run out of attempts fail.

Before upgrading, look at the peak of the `Count` metric of your API stage (`AWS/ApiGateway`, per minute, divided by 60) over a few weeks, including Monday mornings and after outages. That is all routes together, so the busiest route is below it: a per-route `api_throttling_rate_limit` above that peak keeps every route clear of throttling. Set `api_throttling_burst_limit` at least as high as the rate. The defaults leave room for about 100,000 Macs. The new `api_4xx` alarm (see [Alarms](#alarms)) fires when requests are throttled.

The throttle bounds a flood on any one route, which is what a runaway client sends, but it does not protect other APIs in the same AWS account and region. They all share the account's regional API Gateway limits (10,000 requests per second, burst 5,000 by default), and Rudolph's 6 routes at the default 2,000 each can take 12,000 requests per second together. If other APIs in the account matter, either keep 6 × `api_throttling_rate_limit` well below the account limit (the per-route need is about the number of Macs / 300 requests per second, so 1,000 suits fleets up to about 50,000 Macs with headroom), or deploy Rudolph in its own account. Ask AWS for a higher quota if the fleet needs more.

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

### 5. Machine list indexes
This applies to every existing deployment, including one already upgraded for Santa 2026.8. The release adds two global secondary indexes on the sensor data rows for the web view's machine list, `DataType_SerialKey` and `DataType_HostnameKey` (see [Sensor data row](sync.md#sensor-data-row)), and preflight starts writing `LastSeenDay`. No manual steps are needed; `make deploy` creates both indexes.

What to expect when you apply:

- **Online backfill.** DynamoDB fills each new index from the existing sensor data rows while the table stays fully available: syncs keep working throughout. How long it takes depends on the size of the whole table, not just the sensor data rows (DynamoDB scans every item); it can take a while on a large table. On the on-demand (`PAY_PER_REQUEST`) table the backfill is billed as writes to the index, roughly one per sensor data row per index.
- **One index at a time.** DynamoDB builds only one new index per table at a time, so the hashicorp/aws provider creates them one after the other within the same `apply`: it requests `DataType_SerialKey` (or whichever comes first), waits until it is `ACTIVE`, then requests the next. `apply` therefore stays on the table for both backfills; this is normal, do not interrupt it. If your deployment is also gaining `DataType_MachineID` in this upgrade (step 4), that index is built in the same sequence.
- **If `apply` times out.** The provider waits up to 60 minutes for each index (its default `update` timeout). If a backfill takes longer, `apply` fails with a timeout but DynamoDB keeps building the index. Wait until `aws dynamodb describe-table --table-name PREFIX_rudolph_store` (or your `dynamodb_table_name`) shows every index as `ACTIVE`, then run `make deploy` again: it creates any index that is still missing and changes nothing else on the table.
- **Rows filled in as Macs sync.** The backfill copies existing rows into an index at once, but only rows that already have its sort key. `SerialKey` and `LastSeenDay` are new in this release, so no existing row is in `DataType_SerialKey`, and rows backfilled into `DataType_HostnameKey` (those that already have `HostnameKey`) have no last-seen date. Each Mac's next preflight rewrites its row with both. The machine list is therefore complete within one sync interval of the deploy **for Macs that are syncing**; a Mac that is offline stays out of the serial list and shows no last-seen date until it next syncs, and its row expires 90 days after its last preflight if it never does.

Rudolph does not query these indexes itself, so its IAM policies do not change; whatever reads them for the web view needs `dynamodb:Query` on the two index ARNs (`<table ARN>/index/DataType_SerialKey` and `<table ARN>/index/DataType_HostnameKey`).

### 6. Global rule policy index
This applies to every existing deployment. The release adds the `GlobalRulePolicy_SK` global secondary index, which the web view uses to filter global rules by policy, and starts writing `GlobalRulePolicy` on global rule rows (see [Global rules and the policy index](rules.md#global-rules-and-the-policy-index)). `make deploy` creates the index; like the machine list indexes, it is built online and one index at a time (see step 5).

**After applying, run `rudolph rules backfill-policy-index` once** (with the same `--region` and `--dynamodb_table` you use for other `rudolph` commands). Until then, global rules created by an older Rudolph are missing from the web view's policy filter. The command reads only the global rules, adds `GlobalRulePolicy` to those that lack it, changes nothing that Santa sees and prints how many rules it updated; running it again updates none. Its credentials need `dynamodb:Query` and `dynamodb:UpdateItem` on the table, as other rule commands already do. Whatever reads the index for the web view needs `dynamodb:Query` on `<table ARN>/index/GlobalRulePolicy_SK`.

### 7. Execution events table
This applies to every existing deployment. The release adds a second DynamoDB table, `PREFIX_rudolph_events`, that the eventupload Lambda fills with blocked and unknown executions for the web view, whatever event sink is configured (see [Execution events table](events-table.md)). `make deploy` creates the table, its three indexes, the `PREFIX_rudolph_events_write_policy` IAM policy for the eventupload role and the Lambda's `EVENTS_DYNAMODB_NAME` variable; no manual steps are needed.

- **Starts empty.** Only events uploaded after the deploy are stored; Santa does not re-send events it already delivered. The web view's lists and day counts fill in from the deploy onwards.
- **Deploy code and infrastructure together** (step 0). An older Lambda ignores `EVENTS_DYNAMODB_NAME` and writes nothing; a new Lambda without the IAM policy answers every upload that holds a blocked or unknown execution with HTTP 500, and Santa keeps re-sending those events until the policy exists.
- **Failures of the table hold back events.** A throttled or failing events table answers uploads with HTTP 500 like a failing sink, so Santa keeps the events and retries the whole upload at its next sync: Firehose, Kinesis and Lambda sinks then receive those records twice, so their consumers must tolerate duplicates. The `api_5xx` alarm covers this.
- **Costs** grow with the number of uploaded events; see [Costs](events-table.md#costs).
- **Restoring a backup.** Like the store table, the store module accepts `events_dynamodb_table_name` to point at a table restored under another name.

### 8. Top binaries ranker
This applies to every existing deployment. The release adds a Lambda, `PREFIX_rudolph_ranker`, run every hour by an EventBridge rule, that builds the web view's top-blocked and top-allowed lists in the events table (see [Top binaries](events-table.md#top-binaries)), and the eventupload Lambda starts writing `LastSeenDay` on machine markers.

- **New Terraform variable.** The `santa_api` module needs `lambda_ranker_zip`, the path of `build/package/ranker_deployment.zip`, which `make build` now creates and `make deploy` passes. If your environment directory was copied from `example` before this release, add the variable to its `_variables.tf` and pass it in `main.tf` (`lambda_ranker_zip = var.lambda_ranker_zip`), as `example` now does; `terraform` fails with a missing variable otherwise.
- **No other manual steps.** `make deploy` creates the function, its role and policy, its log group (with `log_retention_in_days`), the schedule, and adds it to the `lambda_errors` and `lambda_timeouts` alarms. The lists exist from the first run, within an hour of the deploy.
- **Costs.** About US$2 a month for 1,000 Macs and about US$19 for 100,000; see [Top binaries](events-table.md#top-binaries).
- **Machine counts fill in.** Markers written before this release have no `LastSeenDay`, so `Machines` counts only the Macs that have uploaded an event of the binary since the deploy; it is complete for a window once the window has passed since the deploy.
