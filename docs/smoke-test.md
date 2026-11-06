# First real-AWS smoke test

This runbook takes an empty AWS dev account to a verified deployment of Rudolph and rudolph-web, with one or more test Macs syncing against it. Each step says what to run, what success looks like and what to capture. Run it in order; later steps rely on the rules, groups and events earlier steps create.

It is written for the first deployment of this release to a real account. Nothing in it has been run yet: where it predicts AWS behaviour that the tests cannot show (DynamoDB Local and the static Terraform checks stand in for AWS), the step says so and asks you to record what actually happens. Disagreements between the other docs and the code are listed at the end under [Doc issues found](#doc-issues-found).

Use a dev account with nothing else in it. Every command that writes to AWS is yours to run; none of them is safe against a production account.

## 0. Decisions and shell setup

### 0.1 Decide before you start

- **Region.** Any region with Lambda arm64 and API Gateway REST APIs. rudolph-web also needs a `us-east-1` provider alias even without CloudFront.
- **Route 53 hosted zone.** Rudolph always creates an ACM certificate and a custom domain, `<prefix>-rudolph.<zone>`, so the dev account needs a public hosted zone it can write records to (`use_existing_route53_zone = true`), or Terraform creates one (`false`; you then have to delegate it).
- **Names.** `ENV` (the environment directory) and `prefix` (every resource name) can be the same, e.g. `smoke`. `prefix` and `org` end up in S3 bucket names (`<prefix>-<org>-rudolph-source`) and a DNS name, so use lower-case letters, digits and dashes only; the bucket name must be globally unique.
- **Alarm notifications.** Whether to create an SNS topic with your email subscribed, so the alarm checks in step 11 reach you.
- **Afterwards.** Tear down after the test, or keep it running for a few days to measure costs (step 12 needs at least 2 to 3 days of Cost Explorer data).

### 0.2 Shell variables

Run every Rudolph command from the root of your Rudolph checkout: the `rudolph` CLI finds the environment's settings at `deployments/environments/$ENV/config.auto.tfvars.json`, relative to the working directory.

```sh
export AWS_PROFILE=rudolph-dev            # your dev account profile
export REGION=ap-southeast-2              # your region
export ENV=smoke
export PREFIX=smoke                       # = "prefix" in config.auto.tfvars.json
export ORG=yourorg                        # = "org"
export ZONE=dev.example.com               # = "route53_zone_name"
export HOST="${PREFIX}-rudolph.${ZONE}"
export STORE="${PREFIX}_rudolph_store"
export EVENTS="${PREFIX}_rudolph_events"
export SMOKE_DIR="$HOME/rudolph-smoke/$(date +%F)"
mkdir -p "$SMOKE_DIR"
```

Keep captures in `$SMOKE_DIR`, outside both repositories. They hold machine IDs, serial numbers, user names and file paths: summarise the results elsewhere, do not commit the raw captures. Never write the sync token, the Entra client secret or the cookie key into `$SMOKE_DIR`.

## 1. Prerequisites

### 1.1 Tools on your admin Mac

| Tool | Version | Check |
|---|---|---|
| Go | 1.25+ for Rudolph (`go.mod`), 1.26+ for rudolph-web | `go version` |
| Terraform | 1.5+ (tested with 1.16), provider `hashicorp/aws ~> 6.0` | `terraform version` |
| AWS CLI | v2 | `aws --version` |
| Xcode command line tools | for `lipo`, which `scripts/build.sh` uses for the universal CLI | `xcode-select -p` |
| `zip`, `openssl`, `perl`, `curl` | as shipped with macOS | |
| `jq` | optional, for reading JSON output | `jq --version` |

Capture: `{ go version; terraform version; aws --version; sw_vers; } > "$SMOKE_DIR/01-tools.txt"`.

### 1.2 AWS profile

```sh
aws sts get-caller-identity --region "$REGION" | tee "$SMOKE_DIR/01-identity.json"
```

Success: the account ID is the dev account. The role needs to create IAM roles and policies, Lambda, API Gateway, DynamoDB, KMS keys, S3 buckets, ACM certificates, Route 53 records, CloudWatch alarms, log groups and metric filters, EventBridge rules and SSM parameters.

### 1.3 Terraform state bucket

The S3 backend bucket must exist before `terraform init` (both repositories can share it with different keys). Create it with your usual tooling, private, versioned and encrypted. Capture its name and the KMS key you use for it, if any.

### 1.4 Test Macs

- **Main test Mac:** Santa 2026.9 from https://github.com/northpolesec/santa/releases, with its system extension and Full Disk Access approved (MDM: `configs/santa-sysext.mobileconfig` and `configs/santa-tcc.mobileconfig`; on an unmanaged test Mac, approve them in System Settings). Use a Mac you can put into lockdown for half an hour, not your daily machine.
- **Optional:** a second Mac on Santa 2026.8, and a macOS 13 Mac on Santa 2025.12 (the only release supported there, with the limits in [configuring Santa](configuring-santa.md)).
- On each, `santactl version` and `santactl status` work.

Capture: `santactl version` and `sw_vers` from each Mac.

## 2. Create the Rudolph environment and build

### 2.1 Create the environment directory

```sh
make init-env
```

Success: `deployments/environments/$ENV/` exists with `main.tf`, `_variables.tf`, `_backend.tf`, `_outputs.tf`, `config.auto.tfvars.json`, `versions.tf` and a `.gitignore` of `*` (the directory never reaches Git).

### 2.2 Create the sync token

```sh
TOKEN="$(openssl rand -base64 32)"
printf '%s' "$TOKEN" | shasum -a 256 | cut -d ' ' -f 1     # the digest for sync_token_sha256
```

Store `$TOKEN` in your password manager now; it goes into the Santa profile in step 4 and the `curl` checks in step 3. Only the digest goes into Terraform.

### 2.3 Edit `_backend.tf`

Set `bucket`, `key` (e.g. `rudolph-smoke.tfstate`), `region`, `kms_key_id` (or remove it and `encrypt` if you do not use one) and the `region` of the `provider "aws"` block, which must equal `$REGION`. Keep the `default_tags` (`Name = Rudolph`): Cost Explorer uses it in step 12.

### 2.4 Edit `config.auto.tfvars.json`

```json
{
  "aws_account_id": "123456789012",
  "org": "yourorg",
  "prefix": "smoke",
  "stage_name": "dev",
  "region": "ap-southeast-2",
  "eventupload_handler": "NONE",
  "eventupload_kinesis_name": "",
  "eventupload_autocreate_policies": false,
  "route53_zone_name": "dev.example.com",
  "use_existing_route53_zone": true,
  "eventupload_output_lambda_name": "",
  "allowed_inbound_ips": [],
  "kms_key_administrators_arns": [],
  "sync_token_sha256": ["<digest from 2.2>"],
  "mtls_truststore_uri": "",
  "mtls_truststore_version": "",
  "disable_execute_api_endpoint": false,
  "log_retention_in_days": 30,
  "api_throttling_rate_limit": 2000,
  "api_throttling_burst_limit": 2000,
  "alarm_sns_topic_arn": "",
  "api_4xx_alarm_min_requests": 10
}
```

- `eventupload_handler` `NONE`: no Firehose or Kinesis sink; the execution events table is filled anyway. The `event_sink_failures` alarm and filter are then not created.
- `api_4xx_alarm_min_requests` `10` instead of 100, so that one test Mac and a few `curl` calls can fire `api_4xx` in step 11.
- `alarm_sns_topic_arn`: the topic from 0.1, or empty (the alarms then only change state in CloudWatch).
- `enable_s3_logging` is not in the file and defaults to `true`, which creates the access-log bucket that audit item D3 is about (step 11.2). Leave it on for this test.

### 2.5 Expose the module outputs you will need

The example `_outputs.tf` prints only `sync_base_url` and `events_dynamodb_table_arn`. Append these to the environment's own `_outputs.tf` (gitignored, so this changes nothing in the repository):

```hcl
output "events_dynamodb_table_name" {
  value = module.santa_api.events_dynamodb_table_name
}

output "store_kms_key_arn" {
  value = module.santa_api.store_kms_key_arn
}

output "api_gateway_id" {
  value = module.santa_api.api_gateway_id
}

output "raw_url" {
  value = module.santa_api.raw_url
}
```

### 2.6 Test and build

```sh
make test 2>&1 | tee "$SMOKE_DIR/02-test.txt" | tail -5
make build 2>&1 | tee "$SMOKE_DIR/02-build.txt" | tail -12
ls -l build/package/ ./rudolph
```

Success: tests pass; `build/package/` holds `api_deployment.zip`, `api_authorizer_deployment.zip` and `ranker_deployment.zip`; `./rudolph` is a symlink to the universal macOS CLI. `make deploy` would run the same test and build steps; doing them separately lets you look at the plan before applying.

Put the CLI on your `PATH` for this shell, so the commands rudolph-web offers to copy (`ENV=smoke rudolph ...`) paste as they are:

```sh
export PATH="$PWD:$PATH"
rudolph --help | head -5
```

## 3. Deploy Rudolph

### 3.1 Init

```sh
make tf-init 2>&1 | tee "$SMOKE_DIR/03-init.txt" | tail -5
```

Success: `Terraform has been successfully initialized!`, provider `hashicorp/aws` 6.x installed. If it fails with `AccessControlListNotSupported` on the state bucket, remove the `acl = "private"` line from `_backend.tf` and run it again (record this).

### 3.2 Plan

```sh
make plan 2>&1 | tee "$SMOKE_DIR/03-plan.txt" | tail -3
grep -E '# module.santa_api.*will be created' "$SMOKE_DIR/03-plan.txt" | sed 's/ will be created//' | sort > "$SMOKE_DIR/03-plan-resources.txt"
wc -l < "$SMOKE_DIR/03-plan-resources.txt"
```

Success: `Plan: N to add, 0 to change, 0 to destroy.` Record N. Check that the plan holds, by name:

- **DynamoDB** (`module.rule_store`): `aws_dynamodb_table.store` named `smoke_rudolph_store`, `PAY_PER_REQUEST`, TTL on `ExpiresAfter`, point-in-time recovery on, server-side encryption with the store KMS key, and five global secondary indexes: `SerialNum_DataType_MachineID`, `DataType_MachineID`, `DataType_SerialKey`, `DataType_HostnameKey`, `GlobalRulePolicy_SK`. `aws_dynamodb_table.events` named `smoke_rudolph_events`, same settings, with three indexes: `Fleet_TS`, `Decision_TS`, `TeamID_TS`. The IAM policy `smoke_rudolph_events_write_policy` and its attachment to the eventupload role.
- **KMS:** three keys, all with rotation on: the tables' key (alias `alias/smoke-santa-rules-tables-sse`), the Lambda source bucket's key and the access-log bucket's key.
- **Lambda:** `smoke_rudolph_health`, `_xsrf`, `_preflight`, `_eventupload`, `_ruledownload`, `_postflight` and `_authorizer`, each `provided.al2` on `arm64` with an alias named `dev` and a log group with 30 days' retention; `smoke_rudolph_ranker` (256 MB, 300 s timeout) with its own role and policy, log group and `aws_lambda_function_event_invoke_config` with 0 retries. The eventupload function's environment has `EVENTS_DYNAMODB_NAME` and `DYNAMODB_NAME`.
- **EventBridge:** rule `smoke_rudolph_ranker_hourly` with `rate(1 hour)`, its target and the Lambda permission for `events.amazonaws.com`.
- **API Gateway:** REST API `smoke_rudolph` with `binary_media_types = ["application/json"]` and `api_key_source = "AUTHORIZER"`; the stage `dev`; a deployment; `aws_api_gateway_method_settings.throttling` (`*/*`, 2000/2000, metrics and logging off); a `REQUEST` authorizer with identity source `method.request.header.Authorization`; `GET /health` and `POST /{preflight,eventupload,ruledownload,postflight,xsrf}/{machine_id}`; the custom domain `smoke-rudolph.<zone>` with `TLS_1_2`, its base path mapping, the ACM certificate and its DNS validation, and two Route 53 records.
- **S3:** `smoke-<org>-rudolph-source` (versioning, SSE-KMS, SSL-only policy, access logging) with three objects (API, authoriser and ranker zips); `smoke-<org>-rudolph-source-logging` with `aws_s3_bucket_ownership_controls` (`BucketOwnerPreferred`) and `aws_s3_bucket_acl` (`log-delivery-write`).
- **Alarms:** six, `smoke_rudolph_api_5xx`, `_api_4xx`, `_request_too_large`, `_lambda_errors`, `_lambda_timeouts`, `_dynamodb_throttles`; metric filters `*_request_too_large` on the six handler log groups and `*_timeouts` on all eight log groups.

And that it does **not** hold: anything Firehose or Kinesis, `event_sink_failures`, or an `aws_route53_zone` (with `use_existing_route53_zone = true`).

Capture: the plan file, N, and any warnings at the end of the plan (deprecations such as `aws_s3_bucket_object` are expected to be harmless; record their text).

### 3.3 Apply

```sh
make justdeploy 2>&1 | tee "$SMOKE_DIR/03-apply.txt"
```

Review the plan it prints, type `yes`. The ACM validation waits for DNS; allow 5 to 15 minutes.

Success: `Apply complete! Resources: N added, 0 changed, 0 destroyed.` and outputs including `sync_base_url = "https://smoke-rudolph.<zone>/"`.

Capture: the full apply output, its duration, and the outputs:

```sh
terraform -chdir=deployments/environments/$ENV output | tee "$SMOKE_DIR/03-outputs.txt"
```

Watch for, and record if they happen: an error creating Lambda functions with runtime `provided.al2` (see [Doc issues found](#doc-issues-found)), `AccessControlListNotSupported` on `aws_s3_bucket_acl.s3_logging` (audit D3), or the custom domain timing out on certificate validation.

### 3.4 First requests

```sh
curl -s -o /dev/null -w '%{http_code}\n' "https://$HOST/health"                                   # 401: no token
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $TOKEN" "https://$HOST/health"            # {"status":"healthy"} 200
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer wrong" "https://$HOST/health"             # 401
```

Success: `401`, then `{"status":"healthy"}` with `200`, then `401`. The first is answered by API Gateway without invoking the authoriser; the third invokes it, and the authoriser answers by returning the `Unauthorized` error. Note the time of the third request: step 11.3 checks whether it fired `lambda_errors`.

Capture: the three outputs, and the log lines:

```sh
aws logs tail "/aws/lambda/${PREFIX}_rudolph_health" --since 10m --region "$REGION" | tee "$SMOKE_DIR/03-health-log.txt"
```

Expect `GET /health machine=- status=200 duration=...` and the Lambda `REPORT` line; no request headers, no token.

### 3.5 Compressed request bodies and the direction of `binary_media_types`

`binary_media_types = ["application/json"]` makes API Gateway hand every `application/json` request body to Lambda base64 encoded, which is what lets Santa's deflate-compressed bodies through. Responses are not base64 encoded by the handlers, so they must still arrive as plain JSON. This step checks both directions before a Mac depends on them. It writes the sensor data row of a made-up machine, which shows up in rudolph-web as `smoke-curl` (it expires after 90 days, or with the teardown).

```sh
CURL_MID="$(uuidgen)"; echo "$CURL_MID" > "$SMOKE_DIR/03-curl-machine-id.txt"
BODY='{"serial_num":"SMOKECURL01","hostname":"smoke-curl","santa_version":"2026.9.0","os_version":"15.0","os_build":"24A335","client_mode":"MONITOR"}'

# 1. zlib ("deflate"), Santa's default
printf '%s' "$BODY" | perl -MCompress::Zlib -0777 -ne 'print compress($_)' |
  curl -s -w '\n%{http_code} %{content_type}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Content-Encoding: deflate' \
    -H 'Accept: application/json' --data-binary @- | tee "$SMOKE_DIR/03-preflight-deflate.txt"

# 2. gzip
printf '%s' "$BODY" | gzip -c |
  curl -s -w '\n%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Content-Encoding: gzip' \
    --data-binary @- | tee "$SMOKE_DIR/03-preflight-gzip.txt"

# 3. uncompressed, with Accept: */*
curl -s -w '\n%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Accept: */*' \
  --data-binary "$BODY" | tee "$SMOKE_DIR/03-preflight-plain.txt"

# 4. wrong media type
curl -s -o /dev/null -w '%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/plain' --data-binary "$BODY"
```

Success:

1. to 3. `200`, and a body that starts with `{` and is readable JSON (not base64 such as `eyJ...`), containing `"client_mode":"MONITOR"`, `"sync_type":"clean"` (this machine reports no rules, so every preflight is a clean sync), and `"allowed_path_regex":""` and `"blocked_path_regex":""` (Santa 2026.9 gets empty strings).
4. `415`.

If 1. answers 400, request bodies are not reaching the handler intact; if 1. to 3. answer 200 with a base64 body, the response direction is wrong and Santa will fail to parse every response. Either one stops the test: record it and go to [If something fails](#if-something-fails).

Capture: the four results and the preflight log lines (`aws logs tail "/aws/lambda/${PREFIX}_rudolph_preflight" --since 10m --region "$REGION"`).

## 4. rudolph-web

Deploying the web view now lets every later step check its pages as well. Its repository is separate; run these commands in your rudolph-web checkout.

### 4.1 Entra ID app registration

In Microsoft Entra admin centre → App registrations → New registration (details in rudolph-web's `docs/deploy.md`):

1. Single tenant. Platform Web. Leave the redirect URI for now (the Function URL exists only after the first apply), or add `http://localhost:8080/auth/callback`.
2. Certificates & secrets → New client secret. Copy the value into your password manager; it is shown once.
3. App roles → Create app role, twice, allowed member types Users/Groups, values exactly `Rudolph.Viewer` and `Rudolph.Admin`.
4. Enterprise applications → the app → Properties → Assignment required: Yes. Users and groups: assign `Rudolph.Viewer` to one test account and `Rudolph.Admin` to another (or to yourself, changing it between sign-ins; role changes apply at the next sign-in).

Capture: tenant ID, client ID, the two role values as created (screenshot), which account holds which role.

### 4.2 Environment

```sh
cd /path/to/rudolph-web
export ENV=smoke                    # the rudolph-web environment; may differ from Rudolph's
make init-env
```

Edit `deployments/environments/smoke/_backend.tf` (state bucket, a different key such as `rudolph-web-smoke.tfstate`, region; keep the `aws.us_east_1` provider alias) and `config.auto.tfvars.json`:

```json
{
  "aws_account_id": "123456789012",
  "region": "ap-southeast-2",
  "prefix": "smoke",
  "tenant_id": "<tenant GUID>",
  "client_id": "<application (client) ID>",
  "base_url": "",
  "post_logout_redirect_url": "",
  "session_ttl": "1h",
  "cli_env": "smoke",
  "create_ssm_parameters": false,
  "ssm_kms_key_arn": "",
  "dynamodb_table_name": "smoke_rudolph_store",
  "dynamodb_kms_key_arn": "",
  "events_dynamodb_table_name": "smoke_rudolph_events",
  "lambda_memory_size": 256,
  "lambda_timeout_seconds": 10,
  "log_retention_in_days": 30,
  "enable_cloudfront": false,
  "domain_name": "",
  "acm_certificate_arn": "",
  "route53_zone_name": "",
  "use_existing_route53_zone": true,
  "cloudfront_price_class": "PriceClass_100",
  "tags": {}
}
```

- `cli_env` is the **Rudolph** environment name, so the pages' commands start with `ENV=smoke rudolph`.
- `dynamodb_kms_key_arn` is left empty on purpose: Rudolph's key policy lets any principal in the account use the key through DynamoDB, so the web role should need no KMS grant. Step 4.5 checks this; if the pages fail with a KMS `AccessDenied`, set it to the `store_kms_key_arn` output from 3.3 and apply again.

### 4.3 SSM parameters

The names default to `/<web prefix>/rudolph-web/cookie-key` and `/<web prefix>/rudolph-web/client-secret`.

```sh
aws ssm put-parameter --region "$REGION" --name "/smoke/rudolph-web/cookie-key" \
  --type SecureString --value "$(openssl rand -base64 32)"
aws ssm put-parameter --region "$REGION" --name "/smoke/rudolph-web/client-secret" \
  --type SecureString --value "$(cat)"     # paste the Entra client secret, then ctrl-D
```

Success: both commands print a `Version` of 1.

### 4.4 Deploy (two applies without CloudFront)

```sh
make plan 2>&1 | tee "$SMOKE_DIR/04-web-plan.txt" | tail -3
make deploy 2>&1 | tee "$SMOKE_DIR/04-web-apply-1.txt" | tail -12
```

The plan should create `smoke_rudolph_web` (arm64, `provided.al2023`, 256 MB, 10 s), its Function URL (`authorization_type = NONE`), its log group, and a role whose inline policy has `WriteOwnLogs`, `ReadSecrets`, `DecryptSecrets`, `ReadRudolphStore` and `ReadRudolphEvents`, with `dynamodb:GetItem` and `dynamodb:Query` only, on the two tables and their `index/*`. No `dynamodb:Scan`, no write action.

After the first apply: put the `function_url` output, **without the trailing slash**, into `base_url`, register `<function_url>auth/callback` (the `redirect_url` output after the second apply) as a Web redirect URI on the app registration, and run `make deploy` again (`04-web-apply-2.txt`). Until the second apply every request answers 500 and the log says `config: RUDOLPH_WEB_REDIRECT_URL is required`; that is expected.

Capture: plan, both apply outputs, `url` and `redirect_url` outputs.

### 4.5 First sign-in

Open the `url` output in a private window, sign in as the **viewer** account.

Success: redirected to Entra, back to the machines page, which lists `smoke-curl` (serial `SMOKECURL01`). No 500 or 503.

Capture: screenshot; and the function's log:

```sh
aws logs tail "/aws/lambda/smoke_rudolph_web" --since 15m --region "$REGION" | tee "$SMOKE_DIR/04-web-log.txt"
```

The whole page walk-through is step 10, once there is data to show. Back in the Rudolph checkout, restore `ENV`:

```sh
cd /path/to/rudolph && export ENV=smoke
```

## 5. Santa configuration profile and first sync

### 5.1 Machine ID

On the main test Mac:

```sh
MID="$(uuidgen)"; echo "$MID"
sudo defaults write /Library/Preferences/com.google.santa.machine-mapping MachineUUID -string "$MID"
```

The file name is not a preference domain, so it keeps its `com.google.santa` name; `rudolph info` reads it (step 5.3). Record `$MID` in `$SMOKE_DIR/05-machine-ids.txt` on your admin Mac and `export MID=...` there too.

### 5.2 Configuration profile

Write this outside both repositories (it holds the token), fill in the values, and install it: through your MDM scoped to the test Mac, or on an unmanaged test Mac by opening it and approving it in System Settings → General → Device Management.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key>
      <string>com.northpolesec.santa</string>
      <key>PayloadIdentifier</key>
      <string>com.northpolesec.santa.smoke</string>
      <key>PayloadUUID</key>
      <string>REPLACE-WITH-uuidgen-1</string>
      <key>PayloadVersion</key>
      <integer>1</integer>
      <key>ClientMode</key>
      <integer>1</integer>
      <key>SyncBaseURL</key>
      <string>https://smoke-rudolph.dev.example.com/</string>
      <key>SyncExtraHeaders</key>
      <dict>
        <key>Authorization</key>
        <string>Bearer REPLACE-WITH-TOKEN</string>
      </dict>
      <key>MachineIDPlist</key>
      <string>/Library/Preferences/com.google.santa.machine-mapping.plist</string>
      <key>MachineIDKey</key>
      <string>MachineUUID</string>
      <key>MachineOwnerGroups</key>
      <array>
        <string>smoke</string>
      </array>
    </dict>
  </array>
  <key>PayloadDisplayName</key>
  <string>Santa (Rudolph smoke test)</string>
  <key>PayloadIdentifier</key>
  <string>com.example.santa.rudolph-smoke</string>
  <key>PayloadType</key>
  <string>Configuration</string>
  <key>PayloadUUID</key>
  <string>REPLACE-WITH-uuidgen-2</string>
  <key>PayloadVersion</key>
  <integer>1</integer>
</dict>
</plist>
```

- `SyncBaseURL` is the `sync_base_url` output, with its trailing slash.
- `SyncExtraHeaders` carries the token: exactly `Bearer ` followed by the token whose digest is in `sync_token_sha256`.
- `MachineOwnerGroups` reports the group `smoke`, used in step 6.4. Rudolph ignores it until the group is registered.
- `ClientMode` `1` starts the Mac in monitor mode.
- Leave out `SyncClientContentEncoding` (deflate, checked in 3.5) and `SyncEnableProtoTransfer`.
- **`SyncEnableCleanSyncEventUpload`** (the exact key Santa reads; some Rudolph docs call it `EnableCleanSyncEventUpload`) is left out. Santa uploads no events during a clean sync unless it is true, and a Mac that holds no rules is clean synced on every sync. Set it to `<true/>` only for a fleet that will run without any rules; this test adds rules in step 6, after which syncs become incremental and events are uploaded.

The payload above uses the `com.northpolesec.santa` domain that Santa 2026.8 and newer read; `configs/santa-configuration.mobileconfig` in this repository still uses `com.google.santa` and has no `SyncExtraHeaders`, so do not start from it.

Check on the Mac:

```sh
santactl status
```

Success: the sync server shown is `https://smoke-rudolph.<zone>/`, mode Monitor. Capture: `santactl status --json > ~/05-status-before.json` (copy to `$SMOKE_DIR`).

### 5.3 First sync: an empty deployment

```sh
sudo santactl sync --debug 2>&1 | tee ~/05-sync-1.txt
sudo santactl sync --debug 2>&1 | tee ~/05-sync-2.txt
```

Success: both end in a successful sync with no HTTP errors (no 401, 400, 415 or 5xx). Rudolph has no rules yet, so each sync is a **clean** sync (the Mac holds no rules) and its rule download carries one rule: a `REMOVE` of the placeholder identifier `0000…0000` (64 zeros), which is how Rudolph makes Santa 2026.8+ clear its rules on a clean sync with nothing to send. Santa may therefore report one rule received rather than none. The Mac still holds 0 rules afterwards, so the second sync is clean again.

On the admin Mac:

```sh
rudolph info                                # only on the test Mac itself: prints its machine ID
rudolph lookup machine-ids | tee "$SMOKE_DIR/05-lookup.txt"
rudolph lookup serial-number "<test Mac serial>"
rudolph config get --machine "$MID" | tee "$SMOKE_DIR/05-config-get.txt"
```

Success: `$MID` and the curl machine are listed; the serial lookup finds `$MID`; `config get` shows built-in or inherited settings (there is no global configuration yet).

In the router logs, one sync is a preflight, possibly an eventupload, one or more ruledownload pages and a postflight:

```sh
for f in preflight eventupload ruledownload postflight; do
  aws logs tail "/aws/lambda/${PREFIX}_rudolph_$f" --since 15m --region "$REGION" --format short | grep "machine=$MID"
done | tee "$SMOKE_DIR/05-router-lines.txt"
```

Capture: both sync outputs (redact nothing but the token, which Santa does not print), the router lines, and in rudolph-web the machine page of `$MID` (last sync, client mode `MONITOR`, Santa version, "Sync health", last sync type `CLEAN`).

### 5.4 Global configuration

```sh
rudolph config get --global
rudolph config set --global -c monitor            # answer yes
rudolph config get --global | tee "$SMOKE_DIR/05-global-config.txt"
```

Success: the first shows no stored global configuration; after `set`, client mode monitor with the default batch size and regexes. It applies at the Mac's next sync.

## 6. Rules reaching the Mac

Make two harmless test binaries on the test Mac (Go is enough; `go build` signs them ad hoc on Apple silicon):

```sh
mkdir -p ~/smoke-bins && cd ~/smoke-bins
for n in blocked unknown grouped machine; do
  mkdir -p "$n" && printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("rudolph smoke: %s") }\n' "$n" > "$n/main.go"
  (cd "$n" && go mod init "smoke/$n" >/dev/null 2>&1; go build -o "../smoke-$n" .)
done
shasum -a 256 smoke-* | tee ~/06-hashes.txt
```

Without Go on the test Mac, build them on the admin Mac (`GOOS=darwin GOARCH=arm64`) and copy them over. Export the hashes on the admin Mac as `SHA_BLOCKED`, `SHA_UNKNOWN`, `SHA_GROUPED`, `SHA_MACHINE`.

### 6.1 Global rule, clean sync, then incremental

```sh
rudolph rule deny -t binary -i "$SHA_BLOCKED" --global --custom-msg "Rudolph smoke test: global block"
rudolph rules --global | tee "$SMOKE_DIR/06-global-rules.txt"
```

On the test Mac:

```sh
sudo santactl sync --debug 2>&1 | tee ~/06-sync-3.txt     # clean: the Mac still holds 0 rules
sudo santactl sync --debug 2>&1 | tee ~/06-sync-4.txt     # incremental (normal)
santactl rule --check --path ~/smoke-bins/smoke-blocked
santactl fileinfo ~/smoke-bins/smoke-blocked
./smoke-bins/smoke-blocked; echo "exit $?"
```

Success: sync 3 is clean and delivers the blocklist rule; sync 4 is normal and delivers no rule. `santactl rule --check` reports a blocklist binary rule; `fileinfo` shows the SHA-256 and its rule as blocked. Running the binary is blocked, with the custom message in Santa's dialog. In rudolph-web, the machine page's last sync type goes `CLEAN` then `NORMAL`.

### 6.2 Machine rule

```sh
rudolph rule allow -t binary -i "$SHA_MACHINE" --machine "$MID" --expires 2h
rudolph rules --machine "$MID"
```

Sync on the Mac, then `santactl rule --check --path ~/smoke-bins/smoke-machine`. Success: an allowlist binary rule; `rudolph rules --machine` shows its expiry about 2 hours ahead. Optional: `rudolph rule remove -t binary -i "$SHA_MACHINE" --machine "$MID"`, sync, and the rule is gone from the Mac.

### 6.3 Temporary client mode

```sh
rudolph config update --machine "$MID" -c lockdown --for 30m      # asks for confirmation: yes
rudolph config get --machine "$MID" | tee "$SMOKE_DIR/06-temp-mode.txt"
```

On the Mac: `sudo santactl sync`, then `santactl status` shows mode Lockdown. Run `~/smoke-bins/smoke-unknown`: blocked as unknown (this creates the `BLOCK_UNKNOWN` event used in step 7). Then end it:

```sh
rudolph config update --machine "$MID" --end-temporary-mode
```

Sync again; `santactl status` shows Monitor. Success: `config get` shows the temporary mode and its end in local time and UTC while it runs; rudolph-web's machine page shows it under "Sync health". Capture both `config get` outputs and the two `santactl status` outputs.

### 6.4 Group rule

```sh
rudolph group create smoke --description "Rudolph smoke test"
rudolph rule deny -t binary -i "$SHA_GROUPED" --group smoke --custom-msg "Rudolph smoke test: group block"
```

Sync the Mac (it reports `smoke` through `MachineOwnerGroups`; the server caches the group list for up to 60 seconds, so wait a minute after `group create`), then:

```sh
rudolph group show --machine "$MID" | tee "$SMOKE_DIR/06-group-show.txt"
rudolph group get smoke
```

On the Mac: `santactl rule --check --path ~/smoke-bins/smoke-grouped`, and run it. Success: `group show` lists `smoke` as a reported group that counts and as effective; the Mac gets the group rule without a clean sync; the binary is blocked with the group message. Optional: `rudolph group update smoke --no-reported`, sync, and the rule leaves the Mac (the Mac is now ungrouped); `--accept-reported` brings it back.

Optional, on the 2026.8 and macOS 13 (2025.12) Macs: repeat 5.1 to 5.3 and 6.1. On 2025.12, also add `rudolph rule silent-gui -t binary -i "$SHA_UNKNOWN" --global` and check `santactl rule --check` there shows a silent blocklist rule (Rudolph sends `SILENT_GUI_BLOCKLIST` as `SILENT_BLOCKLIST` to releases before 2026.6), and that rudolph-web marks the Mac "legacy client". Remove the rule afterwards.

## 7. Events: blocked execution to events table to rudolph-web

After 6.1 and 6.3 the Mac holds `BLOCK_BINARY` and `BLOCK_UNKNOWN` events. Run `~/smoke-bins/smoke-unknown` once more in monitor mode (an `ALLOW_UNKNOWN` event), then sync so they are uploaded:

```sh
sudo santactl sync --debug 2>&1 | tee ~/07-sync-events.txt
```

On the admin Mac:

```sh
aws logs tail "/aws/lambda/${PREFIX}_rudolph_eventupload" --since 10m --region "$REGION" | tee "$SMOKE_DIR/07-eventupload-log.txt"
aws dynamodb query --region "$REGION" --table-name "$EVENTS" \
  --key-condition-expression 'PK = :pk' --expression-attribute-values "{\":pk\":{\"S\":\"Exec#$MID\"}}" \
  --projection-expression 'SK, Decision, FileName, FileSHA256, SigningID, TeamID' | tee "$SMOKE_DIR/07-events.json"
aws dynamodb query --region "$REGION" --table-name "$EVENTS" \
  --key-condition-expression 'PK = :pk' --expression-attribute-values "{\":pk\":{\"S\":\"BinaryDay#$(date -u +%F)\"}}" | tee "$SMOKE_DIR/07-day-counters.json"
```

Success: `POST /eventupload/{machine_id} machine=$MID status=200`; items with `Decision` `BLOCK_BINARY` (smoke-blocked), `BLOCK_UNKNOWN` and `ALLOW_UNKNOWN` (smoke-unknown), and the group block; a day counter per binary with `Blocked` or `Allowed` counts; no `dropped` line in the log. In rudolph-web: `/events` lists them newest first; `/events?decision=BLOCK_BINARY` and `/events?machine=<MID>` filter them.

If no events arrive: check that the sync was normal, not clean (events are not uploaded during a clean sync), and look for a 500 in the eventupload log (see [If something fails](#if-something-fails)).

## 8. Collecting allowed executions per rule (Google team ID)

This checks the per-rule collection path end to end: one Mac uploading every execution, one flagged rule, and only that rule's executions stored.

```sh
rudolph config update --machine "$MID" --enable-all-event-upload
rudolph rule allow -t teamid -i EQHXZ8M8AV --global
rudolph rule update -t teamid -i EQHXZ8M8AV --global --collect-allowed-events=true
rudolph rule get -t teamid -i EQHXZ8M8AV --global | tee "$SMOKE_DIR/08-rule-get.txt"
```

Expect `rule update` to warn that `EnableAllEventUpload` is off in the global configuration (it is on for this Mac only, which is the point).

On the Mac: sync (now with `enable_all_event_upload: true` in the preflight response), confirm `santactl fileinfo /Applications/Gemini.app` (or Google Chrome if Gemini is not installed) shows team ID `EQHXZ8M8AV` and record its signing ID; quit and launch Gemini and Chrome a few times (Santa only uploads decisions it did not answer from its cache; `sudo santactl flushcache` clears that cache if your Santa has it), wait at least a minute (eventupload caches the collection index for up to 60 seconds in a warm Lambda), then sync again.

Success:

- rudolph-web `/events?team_id=EQHXZ8M8AV` lists `ALLOW_TEAMID` events with signing IDs such as `EQHXZ8M8AV:com.google.Chrome` and Gemini's own; capture the list of distinct signing IDs.
- Other allowed executions this Mac uploaded (Apple-signed apps, other vendors) are **not** in the table: only `BLOCK_*`, `ALLOW_UNKNOWN` and the flagged rule's executions are stored.
- Optionally, the groups doc example: `rudolph rule deny -t signingid -i "<Gemini signing ID>" --group smoke --custom-msg "Gemini is not allowed for smoke"`, sync, launch Gemini: blocked (a signing ID rule beats a team ID rule), while Chrome still runs.

Capture: the eventupload log lines (status and duration of these larger uploads), the count of `ALLOW_TEAMID` items, and the count of eventupload requests from this Mac while the setting is on (step 12.1 uses it).

Turn it off again (the stored events cost money; uploads with every execution cost Lambda time):

```sh
rudolph rule update -t teamid -i EQHXZ8M8AV --global --collect-allowed-events=false
rudolph config update --machine "$MID" --inherit enable-all-event-upload
```

## 9. Ranker and the policy index backfill

### 9.1 The ranker's first run

Either wait for the hourly schedule, or invoke it once:

```sh
aws lambda invoke --region "$REGION" --function-name "${PREFIX}_rudolph_ranker" "$SMOKE_DIR/09-ranker-out.json"
aws logs tail "/aws/lambda/${PREFIX}_rudolph_ranker" --since 15m --region "$REGION" | tee "$SMOKE_DIR/09-ranker-log.txt"
aws dynamodb get-item --region "$REGION" --table-name "$EVENTS" \
  --key '{"PK":{"S":"Top#Blocked#7"},"SK":{"S":"Current"}}' | tee "$SMOKE_DIR/09-pointer.json"
```

Success: the invoke answers `"StatusCode": 200` without `FunctionError`; the log has only `START`, `END` and `REPORT` lines (the ranker logs nothing on success); the pointer has `Snapshot` and `GeneratedAt`. rudolph-web `/events/top-blocked?days=1` lists smoke-blocked, smoke-grouped and smoke-unknown with counts and `Machines` 1; `/events/top-allowed` lists smoke-unknown and, after step 8, Google binaries. Then check the next scheduled run happened: after the next full hour, the ranker log has a second `REPORT` and the page shows a newer generation time.

Capture: `REPORT` lines (duration, max memory used), screenshots of both lists for 1 and 7 days.

### 9.2 `rules backfill-policy-index`

```sh
rudolph rules backfill-policy-index --dry-run
rudolph rules backfill-policy-index | tee "$SMOKE_DIR/09-backfill.txt"
rudolph rules backfill-policy-index
```

Success: `Would update 0 global rules`, then `Updated 0 global rules` twice. On a new deployment every global rule is written with `GlobalRulePolicy` already; anything other than 0 means a rule was written without it, which would be a bug to record. In rudolph-web, `/rules?policy=BLOCKLIST` lists the smoke-blocked rule and `/rules?policy=ALLOWLIST&type=TEAMID` the Google team ID rule.

## 10. rudolph-web page walk-through

Signed in as the **viewer**:

| Page | Check |
|---|---|
| `/` | redirects to or shows the machines page |
| `/machines` | the test Macs and `smoke-curl`, last seen as a UTC date |
| `/machines?serial=<first 3 characters>` and `?hostname=<prefix>` | prefix search, case-insensitive |
| `/machines?serial=a&hostname=b` | 400, "Filter by serial number or by hostname, not both." |
| `/machines/<MID>` | last sync, client mode, rule counts, machine rules (if 6.2 kept it), groups (assigned, reported, effective), Sync health, a "Commands" disclosure |
| `/rules`, `?type=BINARY`, `?policy=BLOCKLIST`, both | global rules filtered |
| `/groups`, `/groups/smoke`, and "Macs in no group" | registered group with description and "reported counts", its rule |
| `/events`, `?machine=<MID>`, `?decision=BLOCK_UNKNOWN`, `?team_id=EQHXZ8M8AV` | newest first, one filter at a time; two filters together answer 400 |
| `/events/top-blocked?days=1`, `7`, `30`, `90`; `/events/top-allowed` likewise | generation time shown, no "more than 3 hours ago" warning |
| any list with `?cursor=garbage` | 400 |
| `/admin` | 403 for the viewer |
| Log out (button) | back to Entra's sign-out, then signed out |

Copy one command from an event row (e.g. block a binary) and one from the machine page, paste each with `--dry-run` appended in the Rudolph checkout: each prints what it would write and `Dry run: the changes shown are not written.`

Sign in as the **admin**: `/admin` answers 200 (a placeholder). Sign in with an account that has neither role (if you have one): every page 403.

Capture: a screenshot per page, the dry-run outputs, and `aws logs tail /aws/lambda/smoke_rudolph_web --since 30m` (no secrets, cookies or tokens in it).

## 11. Checks the audit left for the first deploy

### 11.1 Compressed request bodies and `binary_media_types`

Done in 3.5 with `curl`, and by every Santa sync since (Santa compresses with deflate by default). Record the result here as well. Optionally set `SyncClientContentEncoding` to `gzip` in the profile for one sync, then back.

### 11.2 Audit D3: the S3 ACL

Audit item D3 expected `aws_s3_bucket_acl "log-delivery-write"` to fail on new buckets, whose ACLs are disabled by default. The code now sets no ACL on the source bucket and sets `BucketOwnerPreferred` ownership controls before the ACL on the access-log bucket. Check:

```sh
SRC="${PREFIX}-${ORG}-rudolph-source"
aws s3api get-bucket-ownership-controls --bucket "$SRC-logging"
aws s3api get-bucket-acl --bucket "$SRC-logging"
aws s3api get-bucket-logging --bucket "$SRC"
aws s3api get-bucket-encryption --bucket "$SRC-logging"
aws s3api get-public-access-block --bucket "$SRC"
```

Success: the apply in 3.3 had no ACL error; ownership `BucketOwnerPreferred`; the ACL grants the log delivery group write; the source bucket logs to `$SRC-logging` with prefix `$SRC/`.

Then, a few hours after the apply, look for access logs:

```sh
aws s3 ls "s3://$SRC-logging/$SRC/" | head
```

Record whether any arrived. The logging bucket's default encryption is SSE-KMS with a customer managed key, and S3 server access logging is documented by AWS to deliver only to destination buckets with SSE-S3 default encryption; if no logs arrive after a day, that is the likely cause (see [Doc issues found](#doc-issues-found)).

### 11.3 Alarm firing paths

List them first: `aws cloudwatch describe-alarms --alarm-name-prefix "${PREFIX}_rudolph_" --region "$REGION" --query 'MetricAlarms[].[AlarmName,StateValue]' --output table`. Expect six, all `OK` or `INSUFFICIENT_DATA`.

- **`lambda_errors` from a wrong token.** The request in 3.4 with `Bearer wrong` made the authoriser return its `Unauthorized` error, which Lambda counts as a function error. Check the authoriser's `Errors` metric for that minute and whether `lambda_errors` went to `ALARM`. If it did, every scanner request with a made-up bearer token fires this alarm; record it.
- **`request_too_large`**, for real: a body that decompresses to more than 6 MiB.

  ```sh
  head -c 7000000 /dev/zero | tr '\0' ' ' | perl -MCompress::Zlib -0777 -ne 'print compress($_)' |
    curl -s -w '\n%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
      -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Content-Encoding: deflate' --data-binary @-
  ```

  Success: `413`; the preflight log has `status=413`; within 5 to 10 minutes `smoke_rudolph_request_too_large` is `ALARM` (and notifies the SNS topic, if set), and returns to `OK` later.
- **`api_4xx`**, for real: more than 5% of at least 10 requests in each of two consecutive 5-minute periods answered 4xx. Requests without a token are answered 401 by API Gateway without invoking Lambda:

  ```sh
  for i in $(seq 1 30); do curl -s -o /dev/null "https://$HOST/health"; sleep 20; done
  ```

  Success: after about 10 minutes `smoke_rudolph_api_4xx` is `ALARM`.
- **`api_5xx`, `lambda_timeouts`, `dynamodb_throttles`**: there is no safe way to cause them. Check that each notifies by setting its state by hand; CloudWatch puts it back at the next evaluation:

  ```sh
  aws cloudwatch set-alarm-state --region "$REGION" --alarm-name "${PREFIX}_rudolph_api_5xx" \
    --state-value ALARM --state-reason "smoke test"
  ```

Capture: the alarm history of each (`aws cloudwatch describe-alarm-history --alarm-name ... --history-item-type StateUpdate`), and whether the SNS emails arrived.

### 11.4 Logs carry no secrets or payloads

```sh
for g in authorizer health preflight eventupload ruledownload postflight xsrf ranker; do
  aws logs filter-log-events --region "$REGION" --log-group-name "/aws/lambda/${PREFIX}_rudolph_$g" \
    --filter-pattern '"Bearer"' --query 'events[].message' --output text
done
```

Success: no output. Also search for the test Mac's user name and `smoke-blocked`: they must not appear (the router logs route, machine, status and duration only).

## 12. Measurements

Leave the test Mac syncing normally (every 600 seconds by default) for at least a few hours, ideally a few days, while you use it. Record each figure next to the number the docs assume.

### 12.1 Events per Mac per day

eventupload logs no per-upload event count (only the router line with status and duration), so count uploads in the logs and stored events in the table.

Uploads per Mac per day, in CloudWatch Logs Insights on `/aws/lambda/smoke_rudolph_eventupload`:

```
parse @message /POST \S+ machine=(?<machine>\S+) status=(?<status>\d+) duration=(?<dur>\S+)/
| filter ispresent(machine)
| stats count() as uploads by machine, status, bin(1d)
```

Stored events per Mac for one UTC day (the sort key starts with the execution time in microseconds, 20 digits):

```sh
DAY=2026-11-10
A=$(printf '%020d' $(( $(date -j -u -f '%Y-%m-%d %H:%M:%S' "$DAY 00:00:00" +%s) * 1000000 )))
B=$(printf '%020d' $(( $(date -j -u -f '%Y-%m-%d %H:%M:%S' "$DAY 23:59:59" +%s) * 1000000 + 999999 )))
aws dynamodb query --region "$REGION" --table-name "$EVENTS" --select COUNT \
  --key-condition-expression 'PK = :pk AND SK BETWEEN :a AND :b' \
  --expression-attribute-values "{\":pk\":{\"S\":\"Exec#$MID\"},\":a\":{\"S\":\"$A\"},\":b\":{\"S\":\"$B~\"}}"
```

Compare with: rudolph-web `docs/data-layer-design.md` ("Event store options": about 50 events per Mac per day by default in monitor mode, about 5 in lockdown, about 500 with `EnableAllEventUpload`) and [events-table.md](events-table.md#costs). With the default upload setting nearly every uploaded event is stored, so stored events per day stand in for uploaded ones; with `EnableAllEventUpload` (step 8) only the number of uploads, each of at most `batch_size` (50) events, is visible.

### 12.2 DynamoDB consumed capacity per sync

Pick a quiet window of a few hours in which only the test Mac syncs and nobody runs `rudolph` or rudolph-web. Count its syncs (preflights) in Logs Insights on the preflight log group (`filter @message like /status=200/ | stats count()`), then:

```sh
S=2026-11-10T00:00:00Z; E=2026-11-10T06:00:00Z
for m in ConsumedReadCapacityUnits ConsumedWriteCapacityUnits; do
  aws cloudwatch get-metric-statistics --region "$REGION" --namespace AWS/DynamoDB --metric-name "$m" \
    --dimensions Name=TableName,Value="$STORE" --start-time "$S" --end-time "$E" --period 21600 --statistics Sum
done
```

Index writes are reported separately: repeat the write metric with `Name=GlobalSecondaryIndexName,Value=DataType_SerialKey` (and the other indexes) added to the dimensions. Divide by the number of syncs.

Compare with: audit C3 in `docs/audit/shortcomings.md` (budget for a steady-state sync with no changes: at most 3 writes and 5 reads, no scans) and [sync.md](sync.md#sensor-data-row) (a steady-state preflight writes nothing to the machine list indexes; about one index write per Mac per day when `LastSeenDay` changes).

### 12.3 Lambda durations and memory

Logs Insights over all `/aws/lambda/smoke_rudolph_*` groups:

```
filter @type = "REPORT"
| stats count() as invocations, avg(@duration) as avgMs, pct(@duration, 95) as p95Ms, max(@duration) as maxMs,
        max(@maxMemoryUsed / 1048576) as maxMemMB, count(@initDuration) as coldStarts by @log
```

Compare with the configured memory (preflight and eventupload 256 MB, ruledownload and postflight 512 MB, ranker 256 MB) and the eventupload timeout (10 s, of which sinks and the events table get the deadline minus 1.5 s: [sync.md](sync.md#delivery-to-the-event-sink)). Also record API Gateway `Latency` and `IntegrationLatency` for the stage.

### 12.4 Costs

1. In Billing → Cost allocation tags, activate the `Name` tag (it applies from activation onwards, after up to 24 hours).
2. After 2 to 3 days, Cost Explorer, daily, grouped by service, filtered to the dev account (and to `Name = Rudolph` once active).

Compare with [costs.md](costs.md) (a floor of about US$0.10 a day for one Mac; the alarms about US$2.50 a month), the ranker estimate in [events-table.md](events-table.md#top-binaries) (about US$2 a month at 1,000 Macs, so cents here), and expect fixed items costs.md does not list: three KMS customer managed keys (US$1 a month each), the hosted zone, and point-in-time recovery on both tables.

Capture: a CSV export of the Cost Explorer view.

## 13. Teardown or keep

**Keep** (for more measurements): set the test Mac back to how it should stay (`rudolph rule remove` the smoke rules if they get in the way), and note in your records what is still running and why. Consider an AWS Budget on the dev account.

**Tear down:**

```sh
# rudolph-web checkout
export ENV=smoke && make destroy
aws ssm delete-parameter --region "$REGION" --name /smoke/rudolph-web/cookie-key
aws ssm delete-parameter --region "$REGION" --name /smoke/rudolph-web/client-secret

# Rudolph checkout
export ENV=smoke && make destroy
```

- `make destroy` deletes both DynamoDB tables (all rules, configurations and events) and empties and deletes the S3 buckets (`force_destroy`). KMS keys are scheduled for deletion, not deleted at once.
- Left behind: the state bucket and its objects, the hosted zone (when it existed before), the Entra app registration and its secret, the SNS topic, and the Santa profile on the test Macs. Remove the profile; Santa keeps its last rules and mode until reconfigured.
- Afterwards: `aws resourcegroupstaggingapi get-resources --tag-filters Key=Name,Values=Rudolph --region "$REGION"` lists what still carries the tag (deleted resources can linger in it for a while).
- Revoke the sync token: it is only valid while a deployment holds its digest, but delete it from the password manager so it is never reused.

Capture: both destroy outputs.

## If something fails

| Symptom | Look at |
|---|---|
| `make plan` or `apply` errors | the Terraform output in `$SMOKE_DIR`; for a Lambda runtime error, see [Doc issues found](#doc-issues-found); for an ACL error, step 11.2 |
| 401 on every sync | `SyncExtraHeaders` value (exactly `Bearer <token>`), the digest in `sync_token_sha256` (`printf '%s'`, not `echo`); `/aws/lambda/smoke_rudolph_authorizer` |
| 400 on preflight | `/aws/lambda/smoke_rudolph_preflight`: `preflight: refusing request: ...` names the field over its limit; a machine ID that is not a UUID is logged as `machine=invalid` |
| 415 | the request's `Content-Type` is not `application/json` |
| 413 | the body decompresses to more than 6 MiB; the `request_too_large` alarm |
| 500 from any handler | the handler's log group: the line before `status=500` is `internal server error: "..."` with the cause |
| 502 or 504 | the handler timed out or crashed: `Task timed out` or a panic in its log group; `lambda_timeouts` |
| Sync succeeds but no events | the sync was clean (no events then), or eventupload answered 500 (events table write failed: IAM `smoke_rudolph_events_write_policy`, throttling) |
| Allowed executions not stored | `EnableAllEventUpload` not on for the Mac, rule not flagged (`rudolph rule get`), or the 60-second cache; eventupload log lines starting `eventupload: could not read` |
| Top lists "not computed yet" | ranker log group; `aws lambda invoke` it by hand (9.1); its role may only touch `BinaryDay#`, `BinarySeen#`, `TopSnapshot#` and `Top#` |
| rudolph-web 500 on every page | `/aws/lambda/smoke_rudolph_web`: `config: ... is required` (second apply missing), SSM parameter errors |
| rudolph-web 503 | `RUDOLPH_WEB_DYNAMODB_TABLE` or `RUDOLPH_WEB_EVENTS_TABLE` not set on the function |
| rudolph-web 500 on data pages | a KMS `AccessDenied` in its log: set `dynamodb_kms_key_arn` (step 4.2); a missing index: Rudolph deployed without it |
| Santa side | `sudo santactl sync --debug`; `santactl status`; `log show --predicate 'process == "santad" OR process == "santasyncservice"' --last 15m` |

Request counts by route and status, in Logs Insights over the handler log groups:

```
parse @message /(?<method>[A-Z]+) (?<route>\S+) machine=(?<machine>\S+) status=(?<status>\d+)/
| filter ispresent(status)
| stats count() by route, status
```

## Doc issues found

Found while writing this runbook, by comparing the docs with the code and with the vendored Santa sources in `testdata/`. Not fixed here.

1. **`EnableCleanSyncEventUpload` is not Santa's key.** [sync.md](sync.md) (and comments in `internal/testutil/fakesanta`) tell admins to set `EnableCleanSyncEventUpload` in the profile. Santa 2025.12, 2026.8 and 2026.9 read `SyncEnableCleanSyncEventUpload` (`kSyncEnableCleanSyncEventUpload` in `SNTConfigurator.mm`); the documented key does nothing.
2. **The example Santa profile is out of date.** `configs/santa-configuration.mobileconfig`, which [configuring Santa](configuring-santa.md) points to as the example, sets its preferences under `com.google.santa`, while the same page says Santa 2026.8 reads `com.northpolesec.santa` (confirmed in `SNTConfigurator.mm`), and it has no `SyncExtraHeaders`, without which every sync is refused with 401.
3. **"Received 0 rules" on the first sync.** [configuring Santa](configuring-santa.md) says a working first sync receives 0 rules; on a deployment with no rules, the clean sync sends one rule, the `REMOVE` of the placeholder identifier (`internal/handlers/ruledownload/clean_sync.go`). Verify what Santa prints in 5.3 and correct the doc.
4. **No event count in the eventupload log.** rudolph-web's `docs/data-layer-design.md` says to measure events per Mac per day by counting `events` per upload in the Lambda logs. eventupload logs only the router line (route, machine, status, duration) and `dropped` lines; step 12.1 counts uploads in the logs and stored events in the table instead. Measuring uploaded events with `EnableAllEventUpload` on would need a log line or metric that does not exist.
5. **Module outputs not exposed by the example environment.** rudolph-web's `docs/deploy.md` refers to Rudolph's `events_dynamodb_table_name` output and the table's KMS key; the `santa_api` module has `events_dynamodb_table_name`, `store_kms_key_arn` and `api_gateway_id` outputs, but `deployments/environments/example/_outputs.tf` exports only `sync_base_url` and `events_dynamodb_table_arn` (step 2.5 adds them locally).
6. **`lambda_errors` likely fires on a wrong bearer token.** The authoriser rejects a token that does not match by returning the `Unauthorized` error (`internal/handlers/authorizer/authorizer.go`), which Lambda counts as a function error; [deploy.md](deploy.md#alarms) describes `lambda_errors` as firing when a function "returned an error" and only mentions rejected tokens under `api_4xx`. To be confirmed in 11.3; if it fires, scanners with a made-up token page whoever receives the alarms.
7. **Audit D3 is out of date, and access logging may still not work.** [shortcomings.md](audit/shortcomings.md) D3 describes an ACL on new buckets; the code has since removed the source bucket's ACL and added ownership controls before the logging bucket's ACL. The remaining risk is different: the logging bucket's default encryption is SSE-KMS with a customer managed key, which AWS documents as unsupported for server access log destinations, so logs may never be delivered. To be confirmed in 11.2.
8. **`provided.al2` runtime.** Rudolph's Lambdas (`modules/lambda/api-handler/lambda.tf`, `ranker.tf`) use `provided.al2`, while rudolph-web uses `provided.al2023`. Amazon Linux 2 reached end of support on 30 June 2026; if AWS has started blocking creation of `provided.al2` functions, the apply in 3.3 fails. No doc mentions the runtime or this risk.
9. **The costs floor omits fixed monthly items.** [costs.md](costs.md) gives a floor of about US$0.10 a day for one Mac and lists the alarms (about US$2.50 a month), but not the three KMS customer managed keys (US$1 a month each), the hosted zone or point-in-time recovery; the real floor is likely higher. Step 12.4 measures it.
10. **Example environment placeholders.** `deployments/environments/example/config.auto.tfvars.json` gives `eventupload_handler` as `[KINESIS|FIREHOSE]` with `eventupload_autocreate_policies: true`, although `NONE` is valid and is the simplest start, and it leaves out `authorizer_result_ttl_in_seconds`, which the example's README shows.
