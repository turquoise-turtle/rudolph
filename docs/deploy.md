# Deployment
Ready to build Rudolph? Let's go at it!

For the first deployment to a real AWS account, follow the [first production deployment runbook](smoke-test.md): it goes from an AWS account (which may already hold other resources) to a verified production deployment of Rudolph and rudolph-web, cleans up its test data, and moves the fleet onto it in stages, with what to check and record at each step.

---

## Getting Ready
You will only need to do this stuff once.

### Golang
You will need Go 1.27 or later (the `go` directive in `go.mod`; `go.mod` also names the `toolchain` the builds were tested with, which `go` downloads if yours is older) installed, for `make plan` as well as `make deploy`: both check the environment with Go before running Terraform. Go get it from [the golang website](https://golang.org/dl/).

### Amazon Web Services
You will need an AWS account handy, as well as an IAM role with sufficient privileges.

The account does not need to be empty: Terraform manages only the resources in its own state, all named after your `prefix`. The role that runs Terraform needs to create IAM roles and policies, Lambda, API Gateway, DynamoDB, KMS keys, S3 buckets, ACM certificates, Route 53 records, CloudWatch alarms, log groups and metric filters, and EventBridge Scheduler schedules (`scheduler:CreateSchedule`, `scheduler:GetSchedule`, `scheduler:UpdateSchedule` and `scheduler:DeleteSchedule`, plus `iam:PassRole` on the roles it hands to Lambda, API Gateway, EventBridge Scheduler and Firehose, all under `arn:aws:iam::ACCOUNT_ID:role/rudolph/PREFIX_rudolph_*`, see [cli-iam.md](cli-iam.md#deployer)); with a [cost budget](#cost-budget), also AWS Budgets: `budgets:ViewBudget`, `budgets:ModifyBudget`, `budgets:TagResource`, `budgets:UntagResource` and `budgets:ListTagsForResource` (the provider's default tags apply to the budget). Only these deployer credentials need the Budgets permissions; no Rudolph Lambda does.

### Terraform
You will need Terraform v1.10+ installed (tested with 1.17). We recommend using [tfenv](https://github.com/tfutils/tfenv).
Rudolph uses the `hashicorp/aws` provider `~> 6.72`.

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
* A `.terraform.lock.hcl` file, the provider lock file (see [Provider lock file](#provider-lock-file))
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
| `alarm_sns_topic_arn` | `""` | SNS topic notified when an alarm fires or recovers. Without it the alarms exist in CloudWatch but notify no one, except `fleet_job`, which is then not created. |
| `api_4xx_alarm_min_requests` | `100` | The `api_4xx` alarm ignores 5-minute periods with fewer requests than this. A Mac sends about 2-3 requests every 5 minutes, so the default covers fleets from about 40 Macs; lower it for smaller fleets, at the cost of alarms on a few scanner requests. |
| `monthly_budget_usd` | `0` | Monthly AWS Budgets limit in US dollars for the costs tagged `Name = Rudolph` (see [Cost budget](#cost-budget)). `0` creates no budget. |
| `budget_alert_emails` | `[]` | Who the budget emails. Required when `monthly_budget_usd` is above 0. |
| `table_deletion_protection` | `true` | DynamoDB deletion protection on the rule store and events tables (see [Table deletion protection](#table-deletion-protection)). |
| `firehose_events_retention_in_days` | `180` | Days the Firehose events bucket keeps each event in total (at least `2`): the bucket is versioned, so objects expire a day before this and S3 deletes the expired, noncurrent version 1 day later. Only used when Terraform creates the Firehose sink (see [Telemetry and Retention](security-risks.md#telemetry-and-retention)). |
| `store_kms_key_user_arns` | `[]` | IAM role or user ARNs that may use the store tables' KMS key through DynamoDB besides Rudolph's own Lambdas (ArnLike patterns; a role's ARN includes its path). Empty lets every principal of the account, so table permissions alone decide who reads the tables. When set, list rudolph-web's role and every CLI operator, or they lose access (see [upgrading](#25-telemetry-retention-and-encryption)). |
| `ranker_schedule_expression` | `"rate(1 hour)"` | When the ranker rebuilds the web view's top-blocked, top-allowed and top-unknown lists: an EventBridge Scheduler `rate()`, `cron()` or `at()` expression, in `ranker_schedule_timezone` (see [Ranker schedule](#ranker-schedule)). |
| `ranker_schedule_timezone` | `"UTC"` | IANA time zone of `ranker_schedule_expression`, such as `America/New_York`. The schedule follows its daylight saving. |
| `fleet_schedule_expression` | `"rate(1 day)"` | When the fleet job rebuilds the web view's fleet health lists and Lockdown readiness figures: an EventBridge Scheduler `rate()`, `cron()` or `at()` expression, in `fleet_schedule_timezone` (see [Fleet job schedule](#fleet-job-schedule)). |
| `fleet_schedule_timezone` | `"UTC"` | IANA time zone of `fleet_schedule_expression`, such as `America/New_York`. The schedule follows its daylight saving. |
| `fleet_alarm_days` | `3` | The `fleet_job` alarm fires when the fleet job has not completed a run for this many UTC days in a row: a whole number from 1 to 7 (CloudWatch evaluates at most 7 days of daily periods). The alarm only exists when `alarm_sns_topic_arn` is set. |
| `enable_api_access_logs` | `false` | Write one API Gateway access log line per request, including requests refused before any Lambda runs (rejected tokens, throttling, WAF blocks), to the log group `/aws/apigateway/PREFIX_rudolph_access`, kept for `log_retention_in_days`. Needs the account's API Gateway CloudWatch role first (see [API access log](#api-access-log)). |
| `manage_api_gateway_account_role` | `false` | Create an IAM role with `AmazonAPIGatewayPushToCloudWatchLogs` and set it as the account's API Gateway CloudWatch role (`aws_api_gateway_account`). The setting is shared by every REST API in the account and region and replaces any role already there: only for accounts where `aws apigateway get-account` shows no `cloudwatchRoleArn` and no other stack manages it. |
| `enable_waf` | `false` | Associate an AWS WAF web ACL with the stage: the AWS managed IP reputation list and a per-IP rate limit, with CloudWatch metrics (see [WAF](#waf)). |
| `waf_rate_limit_per_5_minutes` | `100000` | Requests from one source IP in any 5 minutes above which the WAF blocks that IP (HTTP 403) until its rate falls. A fleet behind one NAT or VPN shares an IP; see [WAF](#waf) for sizing. Only used with `enable_waf`. |
| `access_log_retention_days` | `180` | Whole days after which each S3 server access log (of the Lambda source bucket and, with the Firehose sink, the events bucket) expires. The log buckets are versioned, so expiry only puts a delete marker on the log; the data itself is deleted 3 days later, when its noncurrent version expires, and the delete marker after that: a log is gone about `access_log_retention_days` + 3 days after it was written. At least 1. Only the log buckets Rudolph creates, not `existing_logging_bucket_name`. |
| `eventupload_lambda_timeout_seconds` | `10` | Timeout of the eventupload Lambda (3-900 seconds); event sinks and the events table stop 1.5 seconds before it (see [Delivery to the event sink](sync.md#delivery-to-the-event-sink)). Raise it for large uploads or busy Kinesis shards. A `santa_api` module variable that the example environment does not pass: add it to your `_variables.tf` and `main.tf` to change it. |

### Alarms
Rudolph creates these CloudWatch alarms (names start with `PREFIX_rudolph_`):

| Alarm | Fires when |
|---|---|
| `api_5xx` | API Gateway answered 5 or more 5xx responses in 5 minutes. |
| `api_4xx` | More than 5% of requests were answered 4xx in two 5-minute periods in a row, counting only periods with at least `api_4xx_alarm_min_requests` requests (default 100, so that a few scanner requests in a quiet night do not fire it; fleets below about 40 Macs need a lower value to be covered): the stage is throttling (429; raise `api_throttling_rate_limit`) or the authorizer is rejecting sync tokens (401/403, e.g. after a botched token rotation). Throttled requests never reach Lambda, so this is the only alarm that sees them. Ruledownloads that are the client's mistake count here too, not in `api_5xx`: a ruledownload without a preflight, or whose sync state expired or was forgotten while its last page was being built (400), and the last page of a sync that a newer sync of the Mac overtook (409, Santa gave up on a slow sync and started again), or a strategy 1 or 2 last page without `seq` for a numbered sync (409); a few are normal, many point at slow ruledownloads or a client resending old cursors. |
| `request_too_large` | A handler answered HTTP 413: a request body that decompresses to more than 6 MiB, or an event upload with more elements or JSON values than any Santa sends; that host cannot sync or upload those events until the cause is fixed. The size case only fires behind a front end that passes compressed bodies through to the handler: API Gateway decompresses bodies itself and refuses one too large for Lambda before any handler runs, so through API Gateway such requests show in `api_4xx` or `api_5xx` instead. The count case fires through API Gateway too (see [Limits on what a host reports](sync.md#limits-on-what-a-host-reports)). |
| `lambda_errors` | Any Rudolph Lambda function except the authoriser, the ranker included, returned an error. The authoriser is left out because it rejects a wrong token by returning an error, so any scanner with a made-up token would fire it; rejected tokens show in `api_4xx`, and an authoriser that really fails (crashes or times out) makes API Gateway answer 500, which `api_5xx` covers. **Gap:** the authoriser also answers 401, not 500, for configuration failures (no valid digests in `sync_token_sha256`, malformed digests, or a request ARN it cannot parse), so a mis-set `sync_token_sha256` refuses every sync but shows only in `api_4xx`, which evaluates only periods with at least `api_4xx_alarm_min_requests` requests (default 100). Small fleets should lower `api_4xx_alarm_min_requests`. |
| `lambda_timeouts` | Any Rudolph Lambda function timed out (`Task timed out` in its log group). |
| `dynamodb_store_read_throttles`, `dynamodb_store_write_throttles`, `dynamodb_events_read_throttles`, `dynamodb_events_write_throttles` | Reads or writes to the rule store or events table, or to any of its global secondary indexes, were throttled. One alarm per table and direction, each summing the table's metric with one per index (throttling on an index is reported under its own `GlobalSecondaryIndexName` dimension). |
| `event_sink_failures` | eventupload could not forward events to Firehose, Kinesis or the output Lambda (only created when a sink is configured). |
| `fleet_job` | The fleet job has not completed a run for `fleet_alarm_days` UTC days in a row (default 3): each day, the function's `Invocations` minus its `Errors` was below 1, or it was not invoked at all. Failures, timeouts and running out of memory all count. Only created when `alarm_sns_topic_arn` is set. The fleet job is in neither `lambda_errors` nor `lambda_timeouts`: one failed night leaves the lists a day older and notifies no one. There are no alarms on stale or incomplete Macs. See [Schedule and alarm](fleet-health.md#schedule-and-alarm). |

The 413, timeout and event sink alarms use metric filters on the Lambda log groups, published in the
`Rudolph/PREFIX` namespace. Request bodies above API Gateway's own 10 MB limit are refused before reaching Lambda and
only show up in API Gateway's `4XXError` metric (and so in `api_4xx`).

The filters count the one line the API router logs per request, such as `POST /eventupload/{machine_id} machine=... status=413 duration=...`. The machine ID comes from the request path, so it is logged only when it is a UUID (otherwise `machine=invalid`); a client cannot add text or lines to the log that way. When a handler answers 500, it logs the cause on the line before, as `internal server error: "..."`, and never puts it in the response: `GET /health` answers `{"status":"unhealthy"}` without the AWS error.


### Cost budget
With `monthly_budget_usd` above 0, Rudolph creates an AWS Budgets cost budget, `PREFIX-rudolph-monthly`, that emails `budget_alert_emails` when actual spend passes 80% and 100% of the amount in a calendar month, and when AWS forecasts that the month will end above 100%.

- **Only Rudolph's costs.** The account may hold other resources, so the budget counts only costs tagged `Name = Rudolph`, the provider's default tag in `_backend.tf` (a `TagKeyValue` filter of `user:Name$Rudolph`). Cost Explorer and Budgets only see a tag once it is activated as a cost allocation tag: in the Billing and Cost Management console, Cost allocation tags, activate `Name`. Only the management (payer) account of an AWS Organization can activate cost allocation tags; in a member account, ask whoever runs the management account. Activation takes up to 24 hours and applies from then on; until then the budget sees no spend. Costs that carry no tag are not counted, so consider an account-wide budget in the console as well.
- **Alerts only.** AWS Budgets never stop or limit spending; exceeding the budget changes nothing in the account.
- **Cost.** See [costs.md](costs.md#budgets).

### Table deletion protection
`table_deletion_protection` (default `true`) turns on DynamoDB deletion protection for both tables, `PREFIX_rudolph_store` and `PREFIX_rudolph_events`: nothing can delete them while it is on, `terraform destroy` included. **`make destroy` fails while it is on.** To tear a deployment down, set `"table_deletion_protection": false` in `config.auto.tfvars.json`, run `make justdeploy` from the deployed commit against the existing build in `build/package/`, then `make destroy`. With that build the plan changes only `deletion_protection_enabled` on the two tables. Do not use `make deploy` for this: it rebuilds, and the re-zipped binaries have new hashes, so the plan also replaces the S3 source objects and updates every Lambda's code. If the build is gone and you have to run `make build` (at the deployed commit, never newer code), those changes are expected; anything else is not. Destroying deletes every rule, configuration and event.

### Ranker schedule
The ranker, `PREFIX_rudolph_ranker`, rebuilds the web view's top-blocked, top-allowed and top-unknown lists (see [Top binaries](events-table.md#top-binaries)). An EventBridge Scheduler schedule, `PREFIX_rudolph_ranker`, runs it at `ranker_schedule_expression` in the time zone `ranker_schedule_timezone`: by default every hour, in UTC. The schedule invokes the function through its own role, `PREFIX_rudolph_ranker_scheduler_role`, which may invoke only the ranker. A failed run is not retried; the next scheduled run rebuilds every list from scratch.

To run it only in working hours, for example hourly from 8am to 6pm on weekdays, New York time:

```json
"ranker_schedule_expression": "cron(0 8-18 ? * MON-FRI *)",
"ranker_schedule_timezone": "America/New_York"
```

Scheduler `cron()` expressions have six fields (minutes, hours, day of month, month, day of week, year) and need `?` in either day of month or day of week. The schedule follows the time zone's daylight saving, so the runs stay at 8am to 6pm local time all year.

The trade-off of fewer runs:

- **Stale-list warning.** rudolph-web warns when a top list is older than its `top_list_stale_after` setting (whole hours, default `3h`, which suits the hourly default). With a working-hours schedule, raise it to fit, or the warning shows on every top-list page outside the scheduled hours (with the example above, every evening, overnight and all weekend) although the ranker is healthy. rudolph-web's `docs/deploy.md` gives the figures: with hourly runs from 7am to 5pm on weekdays a list is normally up to 14 hours old overnight and about 62 hours old over a weekend, so `15h` still warns every weekend and `63h` never warns on a healthy schedule.
- **The 1-day list stops at the last run.** Each list covers events up to the run that built it, so outside the scheduled hours the 1-day list shows the day as it stood at the last run, and the longer windows lag by the same amount. The windows are UTC days whatever the schedule's time zone.
- **Cost.** The ranker costs little either way (see [costs.md](costs.md)); fewer runs cost proportionally less, about a third with the example above (55 runs a week instead of 168).

### Fleet job schedule
The fleet job, `PREFIX_rudolph_fleet`, rebuilds the web view's fleet health lists and Lockdown readiness figures (see [Fleet health and Lockdown readiness](fleet-health.md)). An EventBridge Scheduler schedule, `PREFIX_rudolph_fleet`, runs it at `fleet_schedule_expression` in the time zone `fleet_schedule_timezone`: by default once a day, in UTC. The schedule invokes the function through its own role, `PREFIX_rudolph_fleet_scheduler_role`, which may invoke only the fleet job. A failed run is not retried; the next run rebuilds everything from scratch.

`rate(1 day)` runs at the time of day the schedule was created. To pick the hour, for example 6am New York time, before staff arrive:

```json
"fleet_schedule_expression": "cron(0 6 * * ? *)",
"fleet_schedule_timezone": "America/New_York"
```

- **Once a day is enough.** The lists change slowly (a Mac goes stale over days, readiness looks back 30 days), and each run reads every Mac. Running it hourly costs 24 times as much for the job (see [costs.md](costs.md#fleet-health-and-lockdown-readiness)).
- **Do not skip days** without raising `fleet_alarm_days`: the `fleet_job` alarm counts UTC days without a completed run, so with a schedule that skips days, such as weekdays only, set `fleet_alarm_days` to more than the longest run of days without a run.
- **To refresh the lists now**, for example after setting a holiday, invoke the function by hand (see [Running it by hand](fleet-health.md#running-it-by-hand)).
- **Optional settings on the function**, not Terraform variables: `FLEET_MIN_SANTA_VERSION` (default `2026.9`), the release below which a Mac is listed under `Version`, and `FLEET_MAX_MACS` (default 250,000). To change one, add it to the `environment` block of `aws_lambda_function.fleet` in `fleet.tf`.

### State locking
The example `_backend.tf` sets `use_lockfile = true` (Terraform 1.10 or later): while a plan or apply runs, Terraform holds a lock object, `<key>.tflock`, next to the state object in the state bucket, so a second plan or apply at the same time (another person, or CI) fails at once with "Error acquiring the state lock" instead of overwriting the first one's state. To have it retry until the lock is free instead, give it `-lock-timeout`, for example `terraform -chdir=deployments/environments/$ENV plan -lock-timeout=10m`; the `make` targets do not pass one. If the state bucket's policy, or the deployer's IAM policy, limits access to the state key, it must also allow `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject` on `<key>.tflock` (for the example, `arn:aws:s3:::your-terraform-tfstate-bucket/rudolph-example-prefix.tfstate.tflock`); without `s3:DeleteObject` the first apply succeeds but leaves the lock behind, and every later plan fails. A lock left by an interrupted run is removed with `terraform -chdir=deployments/environments/$ENV force-unlock <lock ID>`, once you are sure that run has stopped.

### Provider lock file
`deployments/environments/example/.terraform.lock.hcl` is committed: it pins the AWS provider version and the checksums of its packages for `linux_amd64`, `darwin_arm64` and `darwin_amd64`, so `terraform init` refuses a provider package that differs from the one the example was validated with. `make init-env` copies it into each new environment. Keep it in the environment (environments are not tracked by Git, so copy it again when you upgrade Rudolph). To move to a newer provider within `versions.tf`'s constraint, update the example's with `terraform providers lock -platform=linux_amd64 -platform=darwin_arm64 -platform=darwin_amd64` after `terraform init -upgrade -backend=false` in `deployments/environments/example`, and add any platform you deploy from that is not listed.

### API access log
With `enable_api_access_logs`, the stage writes one JSON line per request to `/aws/apigateway/PREFIX_rudolph_access`: request ID, source IP, time, method, resource path (the template, such as `/preflight/{machine_id}`, so no machine ID), status, response length and latency, why API Gateway answered an error (`errorResponseType`, such as `UNAUTHORIZED`, `THROTTLED` or `WAF_FILTERED`), and the authorizer, integration and WAF results. It is the only record of requests API Gateway refuses itself, which never reach a Lambda. It costs CloudWatch Logs ingestion: a line is about 400 bytes and a Mac sends about 700 requests a day, so about US$4 a month per 1,000 Macs at us-east-1 prices (see [costs.md](costs.md#optional-access-log-and-waf)).

API Gateway writes access logs with the account's API Gateway CloudWatch role (`cloudwatchRoleArn`), an account-wide setting per region shared by every REST API there; Rudolph's own Lambda logs do not need it. Without it, the apply that turns logging on fails with `CloudWatch Logs role ARN must be set in account settings to enable logging`. Check first:

```sh
aws apigateway get-account --region "$REGION" --query cloudwatchRoleArn --output text
```

- **A role ARN:** the account already has a role (another stack or a person set it). Leave `manage_api_gateway_account_role` off; the role needs `AmazonAPIGatewayPushToCloudWatchLogs` (or the same permissions), which roles made for this have.
- **`None`:** either set a role yourself (in the console, API Gateway, Settings, "CloudWatch log role ARN"), or set `manage_api_gateway_account_role` to `true` and apply once before or together with `enable_api_access_logs`. Rudolph then creates `PREFIX_rudolph_apigateway_cloudwatch_role` and manages the account setting (`aws_api_gateway_account`). Only do this when no other stack manages the setting: two stacks managing it overwrite each other on every apply. **Turning `manage_api_gateway_account_role` off again, or `make destroy` while it is on, clears the role for every REST API in the account and region:** destroying `aws_api_gateway_account` makes the AWS provider (6.x) reset the account's `cloudwatchRoleArn` to empty and destroys the role, so access and execution logging stops for every REST API there, Rudolph's and other stacks', and any apply that turns logging on fails, until someone sets a role again. Before either, set another role in the account settings, or remove the resources from Terraform's state (`terraform state rm` of `module.santa_api.aws_api_gateway_account.cloudwatch[0]`, the role and its attachment) to keep them.

The deployer needs, approximately (these lists are not exhaustive; a policy built from them may need more, which the failing call names): `logs:CreateLogGroup`, `logs:PutRetentionPolicy`, `logs:TagResource` (the provider's `default_tags` tag the group), `logs:DescribeLogGroups`, `logs:ListTagsForResource` and `logs:DeleteLogGroup` for the log group, and with `manage_api_gateway_account_role` also `iam:CreateRole`, `iam:TagRole`, `iam:GetRole`, `iam:ListRolePolicies`, `iam:ListAttachedRolePolicies`, `iam:AttachRolePolicy`, `iam:DetachRolePolicy`, `iam:DeleteRole` and `iam:PassRole` on that role, and `apigateway:GET` and `apigateway:PATCH` on `arn:aws:apigateway:REGION::/account`.

### WAF
With `enable_waf`, Rudolph creates a regional AWS WAF web ACL, `PREFIX_rudolph`, and associates it with the stage. It has two rules, both of which look only at the source address:

- **`aws-ip-reputation-list`:** the AWS managed rule group `AWSManagedRulesAmazonIpReputationList`, which blocks addresses AWS threat intelligence has seen in botnets, scanning and other attacks.
- **`per-ip-rate-limit`:** blocks a source IP that sends more than `waf_rate_limit_per_5_minutes` requests in any 5 minutes, until its rate falls below the limit again.

Everything else is allowed. The AWS core rule set and any body size rule are deliberately left out: Santa uploads events as deflated binary bodies, often larger than the core rule set's 8 KB body limit, and its other rules would read the compressed bytes as text, so they would block real uploads. A blocked request is answered HTTP 403 by WAF and never reaches the authorizer or a Lambda; Santa fails that sync and tries again at the next one. Each rule and the web ACL publish CloudWatch metrics (`AWS/WAFV2`, metric names `PREFIX_rudolph_ip_reputation`, `PREFIX_rudolph_rate_limit` and `PREFIX_rudolph`), and sampled requests are kept for the console.

**Sizing the rate limit.** The limit is per source IP, and a whole office behind one NAT, or a VPN's egress, shares one address. A Mac sends about 2-3 requests every 5 minutes (a sync of 4-6 requests about every 10 minutes, see the throttling comment in `deployments/terraform_modules/santa_api/main.tf`), more during a clean sync or when everyone opens their laptops in the morning. The default of 100,000 requests in 5 minutes is deliberately generous: about 30,000 Macs behind one address at the usual rate, or several thousand during a clean sync of the whole site. It still stops a single source sending more than about 330 requests per second. To tighten it, take the largest number of Macs that share one public address and allow at least 15 requests per Mac (3 per 5 minutes, five times over for peaks), and never below the WAF minimum of 10. Watch the `PREFIX_rudolph_rate_limit` metric after a change: any blocked request from your own address range means the limit is too low.

**If the IP reputation list blocks your own office or VPN.** AWS's list can include a shared egress address, such as a VPN provider's, a cloud NAT gateway's or an office ISP's carrier-grade NAT, and then every Mac behind it fails to sync with HTTP 403. It shows as blocked requests in the `PREFIX_rudolph_ip_reputation` metric (`AWS/WAFV2`, `BlockedRequests`) and, with the source addresses, in the web ACL's sampled requests in the WAF console. To restore syncing at once, set `enable_waf` to `false` and apply, which removes the web ACL. To keep the rate limit meanwhile, change the `aws-ip-reputation-list` rule's `override_action` from `none {}` to `count {}` in `deployments/terraform_modules/santa_api/waf.tf` (a local change: matches are then only counted, in the same metric) and apply; ask the address's owner or AWS to review the listing, and switch back once the address is no longer listed.

The deployer needs, approximately (not an exhaustive list): `wafv2:CreateWebACL`, `wafv2:GetWebACL`, `wafv2:UpdateWebACL`, `wafv2:DeleteWebACL`, `wafv2:TagResource` and `wafv2:ListTagsForResource` (the provider's `default_tags` tag the web ACL), `wafv2:AssociateWebACL`, `wafv2:DisassociateWebACL` and `wafv2:GetWebACLForResource`, and `apigateway:SetWebACL` on the stage. The web ACL costs about US$7 a month plus US$0.60 per million requests (see [costs.md](costs.md#optional-access-log-and-waf)).

### Install Dependencies
Download and install all golang dependencies with:

```
make deps
```

`make deps-check` checks them: it runs [govulncheck](https://go.dev/doc/security/vuln/) (pinned in the `Makefile`), which fails if a known vulnerability affects Rudolph's code, then lists every module in the build graph that has a newer version or is deprecated. That list is for information: it includes modules that only appear in other modules' `go.mod` files and are never built (for example `github.com/golang/protobuf`, which `google.golang.org/protobuf` names for its own tests). Run it before a release and after updating dependencies (`go get -u ./...`, `go get -u -t ./...`, `go mod tidy`).

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

### CLI permissions
The `rudolph` CLI uses the operator's own AWS credentials, not a Rudolph role (see [cli.md](cli.md)). Besides the reads and writes of the items it manages, every command that writes also records an [audit entry](cli.md#the-audit-trail), and `rudolph audit` reads them. If you restrict the CLI's IAM policy by partition key (`dynamodb:LeadingKeys`), allow `dynamodb:PutItem` and `dynamodb:Query` on the `Audit#*` partitions of the store table. Without `PutItem` every change is still written, but the command warns that it was not audited and exits non-zero. The Lambdas never read or write audit entries, so their policies do not change. [cli-iam.md](cli-iam.md) has example policies: a read-only operator and a rule administrator limited to the partitions the CLI writes.

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
  an execution decision (Santa sends nothing else; the sinks receive the decision as uploaded, as only the events
  table drops file access events with any other decision).
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
The AWS provider moved from `5.15.0` (or `~> 3.76.1`) to `~> 6.72`, and Terraform itself must be 1.10 or later (`required_version`). Update `versions.tf` in your environment directory to match the example (see step 2), then update the dependency lock file:

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
- **If `apply` times out.** The provider waits up to 60 minutes for each index (its default `update` timeout). If a backfill takes longer, `apply` fails with a timeout but DynamoDB keeps building the index. Wait until `aws dynamodb describe-table --table-name PREFIX_rudolph_store` shows every index as `ACTIVE`, then run `make deploy` again: it creates any index that is still missing and changes nothing else on the table.
- **Rows filled in as Macs sync.** The backfill copies existing rows into an index at once, but only rows that already have its sort key. `SerialKey` and `LastSeenDay` are new in this release, so no existing row is in `DataType_SerialKey`, and rows backfilled into `DataType_HostnameKey` (those that already have `HostnameKey`) have no last-seen date. Each Mac's next preflight rewrites its row with both. The machine list is therefore complete within one sync interval of the deploy **for Macs that are syncing**; a Mac that is offline stays out of the serial list and shows no last-seen date until it next syncs, and its row expires 90 days after its last preflight if it never does.

Rudolph's Lambdas do not query these indexes, so their IAM policies do not change (`rudolph machines` reads `DataType_SerialKey` with the operator's own credentials); whatever reads them for the web view needs `dynamodb:Query` on the two index ARNs (`<table ARN>/index/DataType_SerialKey` and `<table ARN>/index/DataType_HostnameKey`).

### 6. Global rule policy index
This applies to every existing deployment. The release adds the `GlobalRulePolicy_SK` global secondary index, which the web view uses to filter global rules by policy, and starts writing `GlobalRulePolicy` on global rule rows (see [Global rules and the policy index](rules.md#global-rules-and-the-policy-index)). `make deploy` creates the index; like the machine list indexes, it is built online and one index at a time (see step 5).

**After applying, run `rudolph rules backfill-policy-index` once** (with the same `--region` and `--dynamodb_table` you use for other `rudolph` commands). Until then, global rules created by an older Rudolph are missing from the web view's policy filter. The command reads only the global rules, adds `GlobalRulePolicy` to those that lack it, changes nothing that Santa sees and prints how many rules it updated; running it again updates none. Its credentials need `dynamodb:Query` and `dynamodb:UpdateItem` on the table, as other rule commands already do. Whatever reads the index for the web view needs `dynamodb:Query` on `<table ARN>/index/GlobalRulePolicy_SK`.

### 7. Execution events table
This applies to every existing deployment. The release adds a second DynamoDB table, `PREFIX_rudolph_events`, that the eventupload Lambda fills with blocked and unknown executions for the web view, whatever event sink is configured (see [Execution events table](events-table.md)). `make deploy` creates the table, its three indexes, the `PREFIX_rudolph_events_write_policy` IAM policy for the eventupload role and the Lambda's `EVENTS_DYNAMODB_NAME` variable; no manual steps are needed.

- **Starts empty.** Only events uploaded after the deploy are stored; Santa does not re-send events it already delivered. The web view's lists and day counts fill in from the deploy onwards.
- **Deploy code and infrastructure together** (step 0). An older Lambda ignores `EVENTS_DYNAMODB_NAME` and writes nothing; a new Lambda without the IAM policy answers every upload that holds a blocked or unknown execution with HTTP 500, and Santa keeps re-sending those events until the policy exists.
- **Failures of the table hold back events.** A throttled or failing events table answers uploads with HTTP 500 like a failing sink, so Santa keeps the events and retries the whole upload at its next sync: Firehose, Kinesis and Lambda sinks then receive those records twice, so their consumers must tolerate duplicates. The `api_5xx` alarm covers this.
- **Costs** grow with the number of uploaded events; see [Costs](events-table.md#costs).
- **Restoring a backup.** The store module (`modules/store`) accepts `dynamodb_table_name` and `events_dynamodb_table_name` to use tables restored under other names, but `santa_api` passes neither, and the Lambdas always use `PREFIX_rudolph_store` and `PREFIX_rudolph_events`. Restoring either table under another name therefore needs a change to the module (`rule_store.tf`, and for the store table `lambda.tf`). `santa_api` used to declare a `dynamodb_table_name` variable that nothing read, so setting it never changed the table; it has been removed. If your environment's `main.tf` passes `dynamodb_table_name` to the `santa_api` module, delete that line: `terraform plan` otherwise fails with `Unsupported argument`.

### 8. Top binaries ranker
This applies to every existing deployment. The release adds a Lambda, `PREFIX_rudolph_ranker`, run every hour by an EventBridge rule (replaced by an EventBridge Scheduler schedule in [step 11](#11-ranker-schedule)), that builds the web view's top-blocked and top-allowed lists in the events table (see [Top binaries](events-table.md#top-binaries)), and the eventupload Lambda starts writing `LastSeenDay` on machine markers.

- **New Terraform variable.** The `santa_api` module needs `lambda_ranker_zip`, the path of `build/package/ranker_deployment.zip`, which `make build` now creates and `make deploy` passes. If your environment directory was copied from `example` before this release, add the variable to its `_variables.tf` and pass it in `main.tf` (`lambda_ranker_zip = var.lambda_ranker_zip`), as `example` now does; `terraform` fails with a missing variable otherwise.
- **No other manual steps.** `make deploy` creates the function, its role and policy, its log group (with `log_retention_in_days`), the schedule, and adds it to the `lambda_errors` and `lambda_timeouts` alarms. The lists exist from the first run, within an hour of the deploy.
- **Costs.** About US$2 a month for 1,000 Macs and about US$19 for 100,000; see [Top binaries](events-table.md#top-binaries).
- **Machine counts fill in.** Markers written before this release have no `LastSeenDay`, so `Machines` counts only the Macs that have uploaded an event of the binary since the deploy; it is complete for a window once the window has passed since the deploy.

### 9. Lambda runtime and access-log bucket encryption
This applies to every existing deployment. Expect these in the plan; `make deploy` handles them with no manual steps:

- **Access-log KMS keys destroyed.** The access-log buckets' KMS keys and aliases (`aws_kms_key.s3_logging` and `aws_kms_alias.s3_logging`, in the Lambda source module and, when Terraform created the Firehose sink, the Firehose module) are destroyed. AWS schedules a destroyed key for deletion after a 30-day waiting period rather than deleting it at once.
- **Logging buckets switch to SSE-S3 in place.** Their default encryption changes from SSE-KMS to SSE-S3 (`AES256`), because S3 does not deliver server access logs to SSE-KMS buckets. Objects already in a bucket keep their old encryption, but there are almost certainly none, since no access logs were delivered while the bucket used SSE-KMS.
- **Lambda runtime updated in place.** Every function's runtime changes from `provided.al2` to `provided.al2023` (Amazon Linux 2 reached end of support on 30 June 2026). The functions are updated, not replaced; the binaries are the same `bootstrap` builds for linux/arm64.

### 10. Cost budget and table deletion protection
This applies to every existing deployment. Expect this in the plan:

- **Deletion protection turned on in place.** `table_deletion_protection` defaults to `true`, so the plan updates both DynamoDB tables in place to set `deletion_protection_enabled = true`; nothing is replaced and syncs are unaffected. From then on `make destroy` fails until you turn it off again (see [Table deletion protection](#table-deletion-protection)). To keep the old behaviour, set `"table_deletion_protection": false`.
- **No budget unless you ask for one.** `monthly_budget_usd` defaults to `0`, which creates nothing. To add one, see [Cost budget](#cost-budget); the role that runs Terraform then needs the Budgets permissions listed under [Amazon Web Services](#amazon-web-services).
- **New variables.** If your environment directory was copied from `example` before this release, add `monthly_budget_usd`, `budget_alert_emails` and `table_deletion_protection` to its `_variables.tf`, pass them in `main.tf` and add them to `config.auto.tfvars.json`, as `example` now does. Without that the module defaults apply (deletion protection on, no budget).

### 11. Ranker schedule
This applies to every existing deployment. The ranker now runs from an EventBridge Scheduler schedule, which takes a time zone, instead of an EventBridge rule, which only knows UTC (see [Ranker schedule](#ranker-schedule)). Expect this in the plan:

- **Rule replaced by a schedule.** The plan destroys `aws_cloudwatch_event_rule.ranker` (`PREFIX_rudolph_ranker_hourly`), `aws_cloudwatch_event_target.ranker` and `aws_lambda_permission.ranker_schedule`, and creates `aws_scheduler_schedule.ranker` (`PREFIX_rudolph_ranker`) with its role `PREFIX_rudolph_ranker_scheduler_role` and that role's policy. With the defaults the ranker keeps running every hour; at most one run is missed or doubled around the apply, which the next run makes good.
- **Deployer permissions.** The role that runs Terraform now needs `scheduler:CreateSchedule`, `scheduler:GetSchedule`, `scheduler:UpdateSchedule` and `scheduler:DeleteSchedule`, and `iam:PassRole` on `arn:aws:iam::ACCOUNT_ID:role/rudolph/PREFIX_rudolph_ranker_scheduler_role`. Without `iam:PassRole` creating the schedule fails after the rule is already gone; rerun `make deploy` once the permission is added. Keep the role's EventBridge rule permissions (`events:DescribeRule`, `events:ListTargetsByRule`, `events:RemoveTargets`, `events:DeleteRule`) and `lambda:RemovePermission` until this apply has run: Terraform needs them to read and destroy the old rule.
- **New variables.** If your environment directory was copied from `example` before this release, add `ranker_schedule_expression` and `ranker_schedule_timezone` to its `_variables.tf`, pass them in `main.tf` and add them to `config.auto.tfvars.json`, as `example` now does. Without that the module defaults apply (every hour, UTC).

### 12. S3 access logs granted by bucket policy
This applies to every deployment with `enable_s3_logging` on (the default) and no `existing_logging_bucket_name`, and, for the events bucket's own ACL (below), to every deployment where Terraform created the Firehose sink, whatever its logging settings. The access-log buckets now let S3 deliver server access logs through their bucket policy, with ACLs disabled, as AWS recommends, instead of the `log-delivery-write` ACL. Expect this in the plan, for the Lambda source module's log bucket (`PREFIX-ORG-rudolph-source-logging`) and, when Terraform created the Firehose sink, the Firehose module's (`PREFIX-ORG-rudolph-events-logging`):

- **ACL resource removed.** `aws_s3_bucket_acl.s3_logging` is destroyed, and in the Firehose module also `aws_s3_bucket_acl.rudolph_eventsupload_firehose` (the events bucket's `private` ACL). Destroying an `aws_s3_bucket_acl` only removes it from the Terraform state: the bucket keeps its ACL.
- **Bucket policy replaced in place.** `aws_s3_bucket_policy.s3_logging` gains a statement allowing `logging.s3.amazonaws.com` to `s3:PutObject` under the source bucket's log prefix, only for this account (`aws:SourceAccount`) and that source bucket (`aws:SourceArn`). The SSL-only deny stays.
- **Ownership switched to `BucketOwnerEnforced`.** `aws_s3_bucket_ownership_controls.s3_logging` (and the events bucket's) changes from `BucketOwnerPreferred` to `BucketOwnerEnforced`, which disables ACLs.

**The upgrade needs two applies.** AWS refuses `BucketOwnerEnforced` (`InvalidBucketAclWithObjectOwnership`) while the bucket's ACL grants access to anyone outside the account, and the old `log-delivery-write` ACL grants the S3 log delivery group; since destroying the ACL resource leaves the ACL in place, Terraform alone cannot get past this. The ownership controls depend on the bucket policy, so the order is safe:

1. `make deploy`. The bucket policy is updated first, so log delivery is already granted by policy; then the log bucket's ownership change fails with `InvalidBucketAclWithObjectOwnership`. Everything else is applied. (The events bucket's `private` ACL grants nothing outside the account, so its change succeeds.)
2. Reset each access-log bucket's ACL to the default private ACL:

   ```sh
   aws s3api put-bucket-acl --bucket "${PREFIX}-${ORG}-rudolph-source-logging" --acl private
   # Only when Terraform created the Firehose sink:
   aws s3api put-bucket-acl --bucket "${PREFIX}-${ORG}-rudolph-events-logging" --acl private
   ```

3. `make deploy` again: the ownership controls now change to `BucketOwnerEnforced`.

Logs keep arriving throughout, granted first by both, then by the policy alone. Check with [smoke-test.md](smoke-test.md) step 11.2. With `existing_logging_bucket_name` set, Terraform does not manage that bucket's policy or ACL: its owner must grant `logging.s3.amazonaws.com` `s3:PutObject` on the prefixes `PREFIX-ORG-rudolph-source/` and `PREFIX-ORG-rudolph-events/`, as above.

### 13. Audit trail
This applies to every existing deployment, and needs no Terraform change. From this release every change the CLI writes is also recorded in the store table under `Audit#<YYYY-MM-DD>` partitions (see [The audit trail](cli.md#the-audit-trail)). If the CLI's credentials are restricted by partition key, allow `dynamodb:PutItem` and `dynamodb:Query` on `Audit#*` before upgrading the CLI (see [CLI permissions](#cli-permissions)); otherwise each change is written but the command exits non-zero, warning that it was not audited.

### 14. No deprecated Terraform
This applies to every existing deployment. The module no longer uses the deprecated `aws_s3_bucket_object` resource. Update `versions.tf` as in step 3 (Terraform `>= 1.10.0`, provider `~> 6.72`) and run `terraform init -upgrade`. The first `plan` then shows:

- **Lambda source zips: forgotten, then uploaded again.** The three source zips (`santa_api_source`, `santa_api_authorizer_source`, `ranker_source`) were `aws_s3_bucket_object` resources and are now `aws_s3_object`. The AWS provider cannot move state between these two types (a `moved` block fails with "Move Resource State Not Supported"), and `import` blocks only work in a root module, so the module has `removed` blocks with `destroy = false`. The plan lists each `module.santa_api.aws_s3_bucket_object.*` as "will no longer be managed by Terraform, but will not be destroyed" and each `module.santa_api.aws_s3_object.*` as created. The keys are content-addressed (`rudolph-source-<MD5 of the zip>.zip`), so with the same build the new resources upload the same bytes to the same keys and no Lambda function changes. If the build changed, the Lambda functions update as on any deploy, and the previous zips stay in the source bucket instead of being deleted; they are a few MB each, and you can delete `rudolph-source-*.zip` objects that no function uses.

- **Global secondary indexes: `key_schema`.** The store and events tables' indexes now name their keys in `key_schema` blocks instead of the deprecated `hash_key`/`range_key`. The plan shows no change for them (checked against the provider: same keys, nothing rebuilt). Run this first plan, and the apply, with a normal refresh: do not pass `-refresh=false`. With the state from before the upgrade, a plan that skips the refresh shows an in-place update of the tables' global secondary indexes; a normal plan reads them back from DynamoDB and shows none.

Nothing is replaced. If the plan shows a Lambda function being replaced, stop and check that the provider is 6.72 or later.

### 15. API deployment replaced only when the API changes
This applies to every existing deployment, and needs no action. The API Gateway deployment (`module.santa_api.aws_api_gateway_deployment.api_deployment`) used to be replaced on every apply, because its trigger hashed whole resources, including attributes AWS fills in after the first apply. It now hashes only the configured settings that need a redeployment: the REST API's binary media types, API key source, execute-api endpoint switch and resource policy; the gateway responses and the models' schemas (neither was covered before); each route's path, method, Lambda and authoriser, and the names of its request and response models; and the authoriser's settings. Settings written as constants in the module (the integration type, the method response headers) are not hashed: a release that changes one of them notes that the deployment must be replaced, and `make force-api-redeploy` does that. The first apply after upgrading still replaces it once (the trigger's value changes), in the same plan as step 14 if you upgrade across both; after that, a deploy that changes only Lambda code leaves it alone. A replacement causes no downtime: the new deployment is created and the stage moved to it before the old one is deleted.

### 16. Lambda source zips no longer re-uploaded on every apply
This applies to every existing deployment, and needs no action. The three source zips (`santa_api_source`, `santa_api_authorizer_source`, `ranker_source`) set `etag` to the zip's MD5, but S3's ETag is not the MD5 of an object uploaded in parts (the provider does that for files over 5 MB) or encrypted with SSE-KMS (the source bucket is), so every plan showed them "updated in-place" (`etag` and `version_id`) and every apply uploaded the same bytes again as a new object version. They now use `source_hash`, which Terraform keeps in its state and never compares with S3. If the deployment already had `aws_s3_object` source zips (step 14 done earlier), the first plan after upgrading updates each of the three in place, once, which uploads one more version of each zip; no Lambda function changes. If you are upgrading across step 14 too, the zips are created with `source_hash` and there is no extra update. After that, a plan with an unchanged build shows no changes.

### 17. Environment checked against the example before plan and apply
This applies to every existing deployment. `make init-env` copies the example environment's `main.tf` and `_variables.tf` once, so an environment created before a release that added a module variable (for example `ranker_schedule_timezone`, step 11) lacks its declaration and module argument. Terraform then only warns "Value for undeclared variable" about the value in `config.auto.tfvars.json`, and plans and applies without the setting. `make plan`, `make deploy`, `make justdeploy` and `make force-api-redeploy` now run `go run ./cmd/envcheck deployments/environments/$ENV` first and refuse to go on when the environment:

- does not declare, in any `.tf` file, a variable the example's `_variables.tf` declares;
- has no `module "santa_api"` block, or its block does not set an argument the example's `main.tf` sets (`source` and `version`, which choose where the module comes from, are not compared);
- sets a value in `config.auto.tfvars.json`, another `*.auto.tfvars.json` file or `terraform.tfvars.json` that it does not declare.

Variables and module arguments of your own that the example does not have are allowed, and module arguments set in override files (`override.tf`, `*_override.tf`, `override.tf.json`, `*_override.tf.json`) count, as Terraform merges them in. Apart from those override files, the check reads `.tf` files only, not `.tf.json`, and only JSON variable files, not `.tfvars` ones. Like Terraform, it skips hidden files (names starting with `.`), backup files ending in `~` and lock files named `#...#`, and the `"//"` comment key in JSON variable files. `make tf-init` and `make destroy` are not checked. The check runs with Go, so `make plan` now needs Go installed, as `make deploy` already did. To fix a refused environment, compare it with the example:

```
diff -u deployments/environments/example/main.tf deployments/environments/$ENV/main.tf
diff -u deployments/environments/example/_variables.tf deployments/environments/$ENV/_variables.tf
```

If the environment has no customisations, copy the example's `main.tf` and `_variables.tf` over it (as in step 2); otherwise add the missing declarations and module arguments by hand. Then run `make plan` again and check the plan picks up the settings.

### 18. Top unknown binaries
This applies to every existing deployment. The eventupload Lambda now also counts `BLOCK_UNKNOWN` and `ALLOW_UNKNOWN` executions on the day counters (`UnknownBlocked`, `UnknownAllowed`), and the ranker builds three more kinds of list, `Unknown`, `UnknownBlocked` and `UnknownAllowed`, marking binaries that a global rule now covers (see [Top binaries](events-table.md#top-binaries)).

- **Terraform apply needed.** `make deploy` applies it. The plan updates the ranker's role policy (a new statement allowing `dynamodb:GetItem` on the store table, `dynamodb:LeadingKeys` `GlobalRules` only), adds `DYNAMODB_NAME` to the ranker Lambda's environment, and changes the description of the ranker schedule; no resource is replaced. A ranker deployed without the apply runs without `DYNAMODB_NAME`: it still builds every list but logs that it can't mark global rules. If the code is deployed with the new environment but an older role policy, every run fails with `AccessDeniedException` on `GetItem` (the `lambda_errors` alarm fires) and the lists stay on their last snapshot, so deploy code and infrastructure together (step 0).
- **The unknown lists fill from the deploy day.** Day counters written before the deploy have no unknown counts, so the unknown lists start empty and a window is complete once that many days have passed since the deploy. `Blocked` and `Allowed` are unchanged.
- **Costs.** The ranker writes 2.5 times as many snapshot rows (twenty lists instead of eight) and makes up to about 300 point reads a run: about US$4 a month for 1,000 Macs instead of US$2, and about US$21 for 100,000 instead of US$19; see [Top binaries](events-table.md#top-binaries). The day counter update is the same write as before, so storing events costs nothing more.

### 19. Group members
This applies to every existing deployment. Preflight and `rudolph group add|remove` now keep a member row per Mac and group (`GroupMembers#<group>`/`<machine id>`) in step with the Mac's groups item, in the same transaction, so that `rudolph group members <group>` can list a group's Macs (see [Listing a group's Macs](groups.md#listing-a-groups-macs)).

1. **Apply the IAM change before the new Lambdas run.** The Lambdas' store policy gains `GroupMembers#*` in the write statement's `dynamodb:LeadingKeys`. A Lambda running the new code under the old policy cannot write the rows: every preflight that changes a Mac's reported groups fails its transaction (with `AccessDeniedException` in the logs), so the Mac's reported groups stay as they were until the policy is applied, when the next preflight writes them. `make deploy` applies the policy and the Lambdas in one apply, in no guaranteed order, so apply the policy on its own first, from a fresh build:

   ```sh
   export ENV=YOURENV
   make build tf-init
   terraform -chdir=deployments/environments/$ENV apply \
     -target=module.santa_api.module.rule_store.aws_iam_policy.lambda_policy \
     --var lambda_api_zip="$PWD/build/package/api_deployment.zip" \
     --var lambda_authorizer_zip="$PWD/build/package/api_authorizer_deployment.zip" \
     --var lambda_ranker_zip="$PWD/build/package/ranker_deployment.zip" \
     --var lambda_fleet_zip="$PWD/build/package/fleet_deployment.zip"
   ```

   The plan updates `aws_iam_policy.lambda_policy` in place and nothing else. (`lambda_fleet_zip` is needed from the release of step 28 on.)
2. **Deploy.** `make deploy`. The rest of the plan is the new Lambda code.
3. **Backfill the rows, once.** Macs whose groups have not changed since the deploy have no rows yet, so run `rudolph group members --rebuild` (with the new CLI). It scans the table once, shows how many rows it writes, and asks first; `--dry-run` shows the counts without writing. If the CLI's credentials are restricted by partition key, allow `dynamodb:Scan` on the table and `dynamodb:BatchWriteItem`, `dynamodb:PutItem`, `dynamodb:UpdateItem`, `dynamodb:DeleteItem` and `dynamodb:Query` on `GroupMembers#*` first (see [CLI permissions](#cli-permissions)).

- **Costs.** No change on steady-state syncs; a membership change costs about two more write units per group joined or left (transactional writes cost double); see [Group members](costs.md#group-members).
- **rudolph-web**, if it changes a Mac's assigned groups, must write the member row in the same transaction from the same release; otherwise run `rudolph group members --rebuild` again after such changes.

### 20. File access events stored
This applies to every existing deployment. The eventupload Lambda now also stores Santa's file access (FAA) events in the events table, as items `FileAccess#<machine ID>`, listed in the `Fleet_TS` index under `FileAccess` and in `Decision_TS` under their `FILE_ACCESS_DECISION_*` decision (see [File access events](events-table.md#file-access-events)). They are still forwarded to the event sinks as before.

- **No IAM change at the time.** When this step was released, the eventupload role had `dynamodb:PutItem` on the whole events table, so `make deploy` changed only the Lambda code. Step 21 limits the role to its own partitions, `FileAccess#*` included; deploy both together if you upgrade across them.
- **Only Macs with an FAA policy upload them.** The policy comes from the Mac's configuration profile (`FileAccessPolicy` or `FileAccessPolicyPlist`), not from Rudolph (see [configuring Santa](configuring-santa.md#fileaccesspolicy-and-fileaccesspolicyplist-optional)); without one there are no events and nothing changes.
- **Log line.** The eventupload count line gains `file_access_received=<n> file_access_stored=<m>` after `stored=<m>`; queries that parse `received` and `stored` keep working.
- **Costs.** About 3 write units and 1 KB of storage for 90 days per stored file access event; see [Costs](events-table.md#costs).

### 21. Least-privilege IAM, keys, logs and buckets
This applies to every existing deployment. An IAM review tightened every role, both KMS key policies, the Lambdas' log permissions and the S3 buckets. **It is not code-only: run a full `make deploy`** (no `-target`), and read the plan first. The Lambda code does not change, and the CLI and rudolph-web need nothing: the store key still lets the account's principals use it through DynamoDB. Expect:

- **Logs: own group only.** Every Lambda role (the six handlers, the authoriser and the ranker) loses its `AWSLambdaBasicExecutionRole` attachment (`aws_iam_role_policy_attachment.basic_execution` in each handler module, `aws_iam_role_policy_attachment.ranker_basic_execution`), which allowed writing to and creating every log group of the account, and gains an inline policy (`aws_iam_role_policy.logs`, `aws_iam_role_policy.ranker_logs`) allowing `logs:CreateLogStream` and `logs:PutLogEvents` on its own log group only. Both happen in the same apply, in no guaranteed order: for a few seconds a function may be unable to write its logs (it keeps running; only those log lines are lost). Never apply one without the other. Every group must be a Terraform resource, which it is once step 4's imports are done: `terraform -chdir=deployments/environments/$ENV state list | grep aws_cloudwatch_log_group` lists eight (nine with the Firehose sink).
- **Store policy: no Scan, DescribeTable or index.** `PREFIX_rudolph_store_policy` is updated in place: it no longer grants `dynamodb:Scan`, `dynamodb:DescribeTable`, the table's indexes or the legacy `MachineInfo#*` partition, none of which preflight, ruledownload or postflight use (only the CLI scans or queries indexes, with its own credentials), and its write statement now requires `dynamodb:LeadingKeys` to be present. Its attachments, a list until now, are keyed by function: the plan shows the ruledownload, preflight and postflight attachments as **moved** (`lambda_policy[1]` to `lambda_policy["ruledownload"]`, `[2]` to `["preflight"]`, `[3]` to `["postflight"]`), not replaced, and destroys `lambda_policy[0]`, the health function's. If the plan instead replaces any of those three, stop: the state's order differs from the list's, and replacing an attachment can detach the policy from a role that keeps it. Move each attachment to its key with `terraform state mv` (check the `role` of each in `terraform state show`) and plan again.
- **Health: its own policy.** The health function gets `PREFIX_rudolph_health_store_policy` (`aws_iam_role_policy.health`), `dynamodb:Query` on the `GlobalRules` partition only, which is all the health check does. If its old attachment goes first, `/health` may answer 500 for a few seconds; syncs are unaffected.
- **Events table: eventupload's partitions only.** `PREFIX_rudolph_events_write_policy` is updated in place to limit eventupload's writes to `Exec#*`, `FileAccess#*`, `BinaryDay#*` and `BinarySeen#*` (so it can no longer write the ranker's top lists), and its store table statements, like the ranker's, gain the `dynamodb:LeadingKeys` presence check.
- **KMS key policies, in place.** The store key's statement for the account's principals is split in two: use through DynamoDB as before, and `kms:CreateGrant` only for AWS resources (`kms:GrantIsForAWSResource`), as DynamoDB creates grants. Principals in `kms_key_administrators_arns` keep managing both keys but can no longer encrypt, decrypt or make data keys with them; one that read the tables through DynamoDB still can, through the account statement. With the Firehose sink, its key loses the statement that let every principal of the account use it directly: the Firehose role uses it through S3 under its own IAM policy (unchanged). Anyone else who reads the events bucket's objects now needs `kms:Decrypt` on that key in their own IAM policy.
- **Firehose sink** (only when Terraform creates it). The eventupload role keeps only `firehose:PutRecordBatch`, the only call it makes (it could also delete the delivery stream and change its destination). The Firehose role writes log events only to its own log group and the `S3Delivery` stream Terraform creates (no `logs:DescribeLogStreams` on every group, no `logs:CreateLogGroup`), and trusts `firehose.amazonaws.com` for this account only (`aws:SourceAccount`). The old log grant named a group that does not exist (`PREFIX-rudolph-eventsupload-firehose`, with hyphens), so Firehose could not log delivery errors until now. The module now receives `eventupload_firehose_name` and `kms_key_administrators_arns`: with a custom `eventupload_firehose_name`, the module used to create the stream and its log group under the default name while the Lambda sent to yours; the plan now creates them under your name and destroys the default-named ones. If you created a stream of that name yourself, set `eventupload_autocreate_policies` to `false` instead.
- **Scheduler trust.** The ranker's scheduler role is trusted only by the ranker's schedule (`aws:SourceArn` `arn:aws:scheduler:REGION:ACCOUNT_ID:schedule/default/PREFIX_rudolph_ranker`), updated in place.
- **S3 public access blocks.** Each bucket gains an `aws_s3_bucket_public_access_block` with all four settings on: the Lambda source bucket and its access-log bucket and, with the Firehose sink, the events bucket and its access-log bucket. Nothing relies on public access: the bucket policies hold the SSL-only deny and, on the log buckets, a grant to `logging.s3.amazonaws.com` for this account and source bucket only, which AWS does not count as public. ACLs are disabled since step 12; a log bucket still on the `log-delivery-write` ACL (step 12 not finished) keeps receiving logs, as the S3 log delivery group is not a public grantee. The deployer needs `s3:PutBucketPublicAccessBlock` and `s3:GetBucketPublicAccessBlock`.
- **Authoriser permission removed.** The authoriser's `aws_lambda_permission` is destroyed: API Gateway invokes the authoriser with its invocation role (`authorizer_credentials`), not through the function's resource policy. The other functions' permissions move to index `[0]` (`api_handler_permissions` to `api_handler_permissions[0]`), which Terraform does by itself; nothing is replaced.
- **API resource policy** (only with `allowed_inbound_ips`). The statement allowing calls through AWS services, which the unconditional allow already covered, is removed. The REST API's policy changes in place, and the API deployment is replaced once (step 15); access does not change.

Afterwards, check with [smoke-test.md](smoke-test.md): `GET /health` answers 200, a sync succeeds, each function's log group gets new lines (`aws logs tail "/aws/lambda/${PREFIX}_rudolph_preflight" --since 10m`), the next ranker run succeeds (no `lambda_errors` alarm) and events keep arriving in the events table. For operators' and deployers' own policies, see [cli-iam.md](cli-iam.md).

### 22. Group names without invisible format characters
This applies to deployments with device groups. Group names now refuse invisible format characters (bidi overrides and isolates, zero-width spaces, soft hyphens and the like; the zero-width joiner and non-joiner stay allowed), and the CLI prints such characters as visible escapes. Preflight drops reported group names that contain them, so a group registered earlier with such a name no longer matches any Mac's reported groups (assigned membership still works).

After deploying, run `rudolph group list` with the new CLI and look for names shown with escapes such as `\u202e` or `\u200b`. For each one, create a group under a clean name (`rudolph group create`), add its rules and assignments to the new group, then delete the old one by the name as it is stored, for example `rudolph group delete $'ops\u202e'` in zsh or bash. Deleting a group leaves its assignments on the Macs: remove them with `rudolph group remove -m <machine-id> $'ops\u202e'`, before or after the delete (`rudolph group show -m <machine-id>` lists them). `group get`, `update`, `delete`, `members`, `add` and `--group` on `rule` and `config` still accept such a name while the group is registered, and `group remove` also accepts it while the Mac has it assigned; only `group create` refuses it.

### 23. State locking, provider lock file, access-log expiry, and optional access log and WAF
This applies to every existing deployment. A Terraform audit added state locking, a committed provider lock file and expiry on the S3 access-log buckets, and two opt-in features, an API Gateway access log and a WAF, both off by default. With the defaults, the only change to AWS is the access-log expiry.

1. **Environment files.** Add the five new variables (`enable_api_access_logs`, `manage_api_gateway_account_role`, `enable_waf`, `waf_rate_limit_per_5_minutes` and `access_log_retention_days`) to your `_variables.tf` and pass them in `main.tf`, as the example does; until then `make plan` and `make deploy` refuse to run (step 17). Copy the example's `main.tf` and `_variables.tf` if you have not customised them. Setting them in `config.auto.tfvars.json` is optional: the defaults keep the access log and WAF off and expire access logs after 180 days.
2. **State locking.** Add `use_lockfile = true` to the `backend "s3"` block of your `_backend.tf` (see [State locking](#state-locking)). If a bucket or IAM policy limits access to the state key, first allow `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject` on `<key>.tflock` too. Then run `terraform -chdir=deployments/environments/$ENV init -reconfigure`: the backend's settings changed, but the bucket and key did not, so the state stays where it is and nothing is migrated. Plans and applies from then on take the lock; no one else should run Terraform against the environment while you make this change.
3. **Provider lock file.** Copy `deployments/environments/example/.terraform.lock.hcl` into `deployments/environments/$ENV/`, replacing the one your own `terraform init` wrote, and run `make tf-init`, which installs the locked version. Compare the `version` first (`grep -A1 hashicorp/aws deployments/environments/$ENV/.terraform.lock.hcl`): if yours pins a newer AWS provider than the example's, keep yours, as a state written by a newer provider cannot be planned with an older one. Copy it again whenever you upgrade Rudolph.
4. **Access-log expiry.** `make plan` adds `aws_s3_bucket_lifecycle_configuration.s3_logging` to the access-log bucket of the Lambda source bucket (`module.santa_api.module.lambda_source[0]`) and, with the Firehose sink created by Terraform, to that of the events bucket (`module.santa_api.module.firehose[0]`); nothing else changes. The buckets are versioned, so S3's next daily lifecycle run after the apply only puts a delete marker on every access log older than `access_log_retention_days` (default 180); the logs themselves are deleted 3 days after that, when their noncurrent versions expire, and the delete markers after that. Until then a log can still be restored by deleting its delete marker. Copy any logs you must keep elsewhere first, or raise the value. Buckets named by `existing_logging_bucket_name` are not touched. The deployer needs `s3:PutLifecycleConfiguration` and `s3:GetLifecycleConfiguration` on those buckets.
5. **Optional: access log.** To turn on `enable_api_access_logs`, check the account's API Gateway CloudWatch role first (`aws apigateway get-account --region "$REGION" --query cloudwatchRoleArn`), and set `manage_api_gateway_account_role` only if it is empty and nothing else manages it (see [API access log](#api-access-log)). The plan creates the log group and updates the stage in place (`access_log_settings`); the API deployment is not replaced.
6. **Optional: WAF.** To turn on `enable_waf`, size `waf_rate_limit_per_5_minutes` for the largest number of Macs behind one public address (see [WAF](#waf)). The plan creates the web ACL and its association with the stage. After the apply, check that syncs still succeed ([smoke-test.md](smoke-test.md)) and that the `PREFIX_rudolph_rate_limit` and `PREFIX_rudolph_ip_reputation` metrics show no blocked requests from your own addresses.

### 24. File access events with a malformed decision (optional clean-up)
This applies to every deployment that stores file access events (step 20); any client holding the sync token could upload them. The eventupload Lambda now stores a file access event only when its decision is `FILE_ACCESS_DECISION_` followed by 1 to 64 upper-case letters or underscores, as every decision Santa sends is (see [File access events](events-table.md#file-access-events)); others are dropped and logged. Nothing needs doing: items a Mac stored before this change under any other decision (an execution decision such as `BLOCK_BINARY`, say) stay in the events table until their 90-day TTL removes them, and rudolph-web from its matching release pages past them when it lists executions by decision.

If you want them gone sooner, find them first and delete them only once you have checked the list; nothing here deletes data by itself. For each execution decision the table stores (listed in [What is stored](events-table.md#what-is-stored)), query the `Decision_TS` index for that decision and keep the items whose `PK` begins `FileAccess#`, for example for `BLOCK_BINARY` (the query reads, and is billed for, every execution of that decision kept, not only the items it returns):

```sh
aws dynamodb query --table-name "${PREFIX}_rudolph_events" --index-name Decision_TS \
  --key-condition-expression "Decision = :d" --filter-expression "begins_with(PK, :fa)" \
  --expression-attribute-values '{":d":{"S":"BLOCK_BINARY"},":fa":{"S":"FileAccess#"}}' \
  --projection-expression "PK, SK"
```

Then delete each listed item by its `PK` and `SK` with `aws dynamodb delete-item`.

### 25. Telemetry retention and encryption
This applies to every existing deployment; the first two changes only with the Firehose sink created by Terraform (`eventupload_handler` `FIREHOSE` and `eventupload_autocreate_policies` `true`). See [Telemetry and Retention](security-risks.md#telemetry-and-retention) for every copy of the telemetry and how long it is kept.

- **Firehose events expire.** The events bucket kept every object forever. It now has lifecycle rules, and the bucket keeps events 180 days in total by default (`firehose_events_retention_in_days`): objects expire a day before that, 179 days after delivery, and the expired, noncurrent version is kept 1 day within the 180 to recover an object deleted by mistake, then deleted. The 180 days are approximate: S3 rounds each lifecycle step up to the next midnight UTC and removes objects asynchronously, with some lag. A second rule removes the delete marker an expired object leaves, and incomplete multipart uploads are removed after 7 days. **The first apply deletes events:** within a day or two S3 expires every object older than 179 days and permanently deletes every noncurrent version that has been noncurrent for more than a day, including any you deleted or overwrote by hand. To keep more, set `"firehose_events_retention_in_days"` in `config.auto.tfvars.json` before you apply, or copy the bucket elsewhere first.
- **The delivery stream encrypts with the module key.** The plan updates the stream in place to start server-side encryption with the Firehose module's KMS key (`CUSTOMER_MANAGED_CMK`), and gives the eventupload role `kms:GenerateDataKey` and `kms:Decrypt` on that key alone, in a policy of its own, because Firehose may check them when the role calls `PutRecordBatch` (AWS does not document whether it does). The stream starts encrypting only after that policy is attached, so uploads are not refused during the apply. Firehose creates a grant on the key, so the role that runs Terraform needs `kms:CreateGrant` on it (an administrator has it). Afterwards check that events keep arriving in the bucket and that the stream's `KMSKeyAccessDenied` metric stays at 0.
- **Optional store key restriction.** `store_kms_key_user_arns` (default `[]`) leaves the store key's policy as it was. To limit who can read the tables through the key, list rudolph-web's role and the roles or users of every CLI operator **first**, then apply: anyone left out, other than Rudolph's own Lambdas and principals whose own IAM policy grants `kms:Decrypt` on the key, gets access denied from DynamoDB on both tables. A role's ARN includes its path (`arn:aws:iam::ACCOUNT_ID:role/PATH/NAME`); for IAM Identity Center roles use a prefix pattern such as `arn:aws:iam::ACCOUNT_ID:role/aws-reserved/sso.amazonaws.com/AWSReservedSSO_RudolphOperators_*` (with the Region segment, `.../sso.amazonaws.com/REGION/AWSReservedSSO_...`, if your roles have one). The plan refuses anything but IAM role and user ARNs of a 12-digit account: no sts assumed-role ARNs (list the role's ARN instead), and no wildcard in the account or straight after `role/` or `user/`, which would let every principal in again. Nor does it accept a wildcard covering a whole path segment (`role/aws-reserved/*`, or `/*/` mid-path), which would let in every role under it, every Identity Center permission set included. List the deployer (the role that runs Terraform) as well, or give it the key through IAM: creating or replacing a table, and restoring one from point-in-time recovery, make DynamoDB call `CreateGrant` on the key as that principal. Key administrators (`kms_key_administrators_arns`) are not held back by the list: they can change the key policy or create grants, and so reach the data. [Telemetry and Retention](security-risks.md#telemetry-and-retention) lists the other limits of the list; see also [cli-iam.md](cli-iam.md).
- **New variables.** If your environment directory was copied from `example` before this release, add `firehose_events_retention_in_days` and `store_kms_key_user_arns` to its `_variables.tf`, pass them in `main.tf` and add them to `config.auto.tfvars.json`, as `example` now does (`make plan` refuses the environment until you do, see step 17).

### 26. Ranker memory and counter display fields
This applies to every existing deployment. The ranker held every binary's file name, signing ID and team ID as stored, up to 4,096 bytes each, so about 45 uploads of 1,000 invented binaries with maximum-length names, from anyone with the sync token, took it past its 256 MB and failed every run until those counters expired. See [Display fields of a day counter](events-table.md#display-fields-of-a-day-counter) and [Top binaries](events-table.md#top-binaries).

- **Code only.** `make deploy` as usual: the plan updates the ranker and eventupload functions' code in place. The ranker keeps its 256 MB, which the compact totals leave plenty of room in (see [Top binaries](events-table.md#top-binaries)).
- **Day counters keep only usable display fields.** From this release eventupload gives a day counter a `FileName` or `SigningID` only of at most 255 bytes and a `TeamID` only of exactly 10 upper-case letters or digits, taking each from the first event of the upload that has a usable one; other values are skipped, not cut. Stored events are unchanged.
- **Counters already written need no clean-up.** They keep their long names until they expire, 100 days after their first event, and the ranker now drops unusable values as it reads them. A top list row whose only name, signing ID or team ID was unusable loses that field at the first run after the deploy, and an unknown row whose `GlobalRule` was found through a signing ID over 255 bytes, or a team ID of another form, loses it; add the rule by SHA-256 or by the real identifier.
- **New log line.** Each run logs `ranker: binaries=<n>`, the number of distinct binaries it folded. A log filter or alarm that expects the ranker's log group to be empty on success needs to allow it. The number of binaries is still not bounded, so compare it with `Max Memory Used` on the `REPORT` line now and then, and raise `memory_size` in `ranker.tf` if that nears 256 MB.

### 27. Rule download pages sized by the response
This applies to every existing deployment; there is nothing to do but deploy. Rule download pages were sized by the configured `batch_size`, Santa's event upload batch size (50 by default), so a clean sync of 1,000 global rules took 21 requests and one of 80,000 took 1,600; and a Mac's machine and group rules were all sent in one response, which failed above Lambda's 6 MB. See [Page size](rule-pagination.md#page-size) and [When they do not fit one page](rule-pagination.md#when-they-do-not-fit-one-page).

- **No Terraform change.** `make deploy` only updates the Lambda code. No stored row changes, and `batch_size` keeps its meaning for event uploads.
- **Pages are larger and fewer.** A response now carries up to 2 MiB of rules: a clean sync of up to about 14,500 small global rules is one rule download request (80,000 rules: 6), and a first sync with 1,000 global and 100 machine rules reads 31 read units, not about 40. In the router log a clean sync shows fewer `POST /ruledownload/{machine_id}` lines, each taking longer; the handler's line reads `Ruledownload clean page: <n> global rule(s) in <bytes> bytes from <q> queries, more pages: <true|false>`. A log filter or dashboard that counts rule download requests per sync, or expects `batch_size` rules a page, needs to allow for it. Incremental syncs do not change.
- **Very large machine and group rule sets now sync.** When a Mac's machine-specific rules exceed what is left of the page they start on, they go on over further pages (log line `Ruledownload machine rules: more pages`), each of which reads all of the Mac's machine rule rows again. Below about 2 MiB of such rules for one Mac, some 15,000 small ones, nothing changes.
- **Syncs in flight during the deploy.** A cursor issued by the previous version is served by the new one, which continues that sync in larger pages. A cursor issued by the new version and answered by the previous one (while both run, or after a rollback) is served too when it is on the global rules or the feed. One on the machine-specific rules (only issued for a Mac whose machine and group rules exceed a page) is either answered with all of them at once, as before, or refused with 400, which Santa does not retry: that sync ends and the Mac's next sync starts again from preflight.
- **Cost estimates.** The tables in [costs.md](costs.md#estimates-by-fleet-size-rule-count-and-sync-interval) and `scripts/cost_grid.py` now count a clean sync as one rule download request up to about 14,500 rules.

### 28. Fleet health and Lockdown readiness
This applies to every existing deployment. A new scheduled Lambda, the fleet job (`PREFIX_rudolph_fleet`), builds the web view's fleet health lists and Lockdown readiness figures once a day; eventupload writes a marker per Mac and unknown binary for it; preflight records each Mac's first-seen date; and `rudolph fleet holiday` raises the stale threshold over a holiday. See [Fleet health and Lockdown readiness](fleet-health.md).

1. **Apply eventupload's IAM change before the new Lambdas run.** The events write policy gains `UnknownSeen#*` in its `dynamodb:LeadingKeys`. eventupload running the new code under the old policy cannot write the markers: every upload holding an unknown execution is answered HTTP 500 (`AccessDeniedException` in its log; `api_5xx` fires), and Santa uploads the same events again at every sync until the policy is applied. In monitor mode that is nearly every upload. `make deploy` applies the policy and the Lambdas in one apply, in no guaranteed order, so apply the policy on its own first, from a fresh build:

   ```sh
   export ENV=YOURENV
   make build tf-init
   terraform -chdir=deployments/environments/$ENV apply \
     -target=module.santa_api.module.rule_store.aws_iam_policy.events_write \
     --var lambda_api_zip="$PWD/build/package/api_deployment.zip" \
     --var lambda_authorizer_zip="$PWD/build/package/api_authorizer_deployment.zip" \
     --var lambda_ranker_zip="$PWD/build/package/ranker_deployment.zip" \
     --var lambda_fleet_zip="$PWD/build/package/fleet_deployment.zip"
   ```

   The plan updates `aws_iam_policy.events_write` in place and nothing else. The old eventupload code never writes `UnknownSeen#`, so the wider policy changes nothing until step 2.
2. **Deploy.** `make deploy`. The plan updates the Lambda code and creates the fleet job: its source object, function (256 MB, 900 s), log group, role with two policies, schedule, scheduler role and policy, and invoke config, 10 resources, plus the alarm `PREFIX_rudolph_fleet_job` when `alarm_sns_topic_arn` is set. It also adds the fleet role to the store key's policy statements that name Rudolph's Lambdas, which matters only when `store_kms_key_user_arns` is set. Nothing is replaced or destroyed.
3. **Run the fleet job once by hand**, rather than waiting up to a day for the schedule:

   ```sh
   aws lambda invoke --region "$REGION" --function-name "${PREFIX}_rudolph_fleet" /tmp/fleet-out.json
   aws logs tail "/aws/lambda/${PREFIX}_rudolph_fleet" --since 15m --region "$REGION"
   ```

   Success: `"StatusCode": 200` without `FunctionError`, and one log line starting `fleet: macs=<n>` with about the number of Macs you have. An `AccessDeniedException` here means the apply of step 2 did not finish.

When each figure becomes accurate (there is no backfill):

| Figure | Accurate |
|---|---|
| Fleet health lists and counts, every category | after the first run (step 3) |
| 7-day unknown and pending counts | 7 days after step 2 |
| 30-day counts and the readiness buckets | 30 days after step 2. Until then every Mac that is not already locked is `Unmeasured` with `Why` `observed`, so no Mac is called ready early. |

- **New variables.** If your environment directory was copied from `example` before this release, declare `lambda_fleet_zip`, `fleet_schedule_expression`, `fleet_schedule_timezone` and `fleet_alarm_days` in its `_variables.tf`, pass them in `main.tf` and add the last three to `config.auto.tfvars.json`, as `example` now does (`make plan` refuses the environment until you do, see step 17). The `Makefile` passes `lambda_fleet_zip`, which `make build` now produces as `build/package/fleet_deployment.zip`.
- **Deployer permissions.** The role that runs Terraform needs `iam:PassRole` on `arn:aws:iam::ACCOUNT_ID:role/rudolph/PREFIX_rudolph_fleet_role` (to Lambda) and `.../PREFIX_rudolph_fleet_scheduler_role` (to EventBridge Scheduler), as for the ranker in step 11; a statement on `role/rudolph/PREFIX_rudolph_*` already covers them.
- **The alarm.** `PREFIX_rudolph_fleet_job` fires after `fleet_alarm_days` UTC days (default 3) without a completed run, and only exists with `alarm_sns_topic_arn`. It can sit in `ALARM` or `INSUFFICIENT_DATA` until the first UTC day with a successful run has been counted. Nothing alarms on stale or incomplete Macs, and the job adds no custom metric or metric filter.
- **Sync cost is unchanged.** `FirstSeenDay` adds about 25 bytes to the sync state, which stays under 1 KB, so a steady-state sync still costs what `TestCOST2_SteadyStateSyncCapacityBudget` allows. Each unknown binary in an upload costs one more write unit.
- **Costs.** About US$0.75 a month for 1,000 Macs in monitor mode and about US$0.30 once most are in Lockdown, of which US$0.20 is the alarm; see [costs.md](costs.md#fleet-health-and-lockdown-readiness).
- **Silent Macs.** Readiness cannot see a configuration profile that turns unknown event upload off on the Mac. Consider `rudolph config update --global --disable-unknown-event-upload=false`, which Santa takes over the profile.
- **Holiday mode.** `rudolph fleet holiday --stale-days 60 --until 2027-02-01` (see [cli.md](cli.md#fleet-holiday)). CLI credentials restricted by partition key need `FleetHealth` added to their write statement (see [cli-iam.md](cli-iam.md#rule-administrator)).
- **rudolph-web** shows "not computed yet" until the pointer exists. Bump its Rudolph pin to this release for the fleet pages, `testdata/webcontract/fleet-rows.json` and `pkg/model/fleethealth`, which it uses to set and end a holiday; its role needs `FleetHealth` among the partitions it may write when editing is on.
- **Rolling back.** Disable or remove the schedule; the pages show the last snapshot's age, then "not computed" once its rows expire after 3 days. Markers expire on their own after 35 days.
