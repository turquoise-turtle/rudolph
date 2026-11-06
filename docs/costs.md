# Costs
So, what does Rudolph cost?

## Minimum Costs
Suppose you have a fleet with only 1 machine, sync'ing once every 10 minutes.

We've observed the cost "floor" of Rudolph to be approximately $0.10 per day, plus whatever costs associated with your
domain name registration (typically like $0.50 a month and $20 a year or something).

## Costs At Scale
AWS costs **_roughly_** scale linearly to your number of clients * their sync rate.

Consider this production load:
* ~1000 rules
* ~10,000 Santa clients
* Sync interval every 60 seconds

Expected Amazon Web Services costs will be in the $50/day range (excluding tax, lower on weekends). Amortized, we estimate
costs of around $900/month, or around $12,000 a year.

The cost directly correlates with how many syncs occur, which is roughly the number of client * the sync rate. Santa
recommends a **_default_** sync rate of 10 minutes, which is 10 times less frequent than above. In this case, your
monthly costs will be about ~90% lower.

If you have 90% fewer clients (e.g. 1,000 clients instead of 10,000), you can expect your costs to be ~90% lower as well.

## Authorizer and Logs
API Gateway caches the authorizer's decision per token for `authorizer_result_ttl_in_seconds` (default 300 seconds),
so a sync normally costs no authorizer Lambda invocation beyond the first one in each cache window, instead of one per
request.

CloudWatch log groups keep logs for `log_retention_in_days` (default 30) instead of forever.

## Throttling and Alarms
The API is throttled per route (`api_throttling_rate_limit`, default 2,000 requests per second on each of the 6 routes, burst 2,000), so a runaway client or a flood of requests gets HTTP 429 from API Gateway instead of running up Lambda and DynamoDB charges. The defaults leave room for the normal load of about 100,000 Macs; see [deploy](./deploy.md#0d-check-the-api-throttle) for why the throttle must stay above the fleet's peak.

The alarms are kept few and cheap: seven alarms, or six when no event sink is configured (see `monitoring.tf` and [deploy](./deploy.md#alarms)).

* Four alarm on metrics AWS publishes for free: `api_5xx`, `api_4xx`, `lambda_errors` and `dynamodb_throttles`.
* Three alarm on metrics that metric filters extract from the Lambda logs: `request_too_large`, `lambda_timeouts` and `event_sink_failures`. The filters are free; each extracted metric is billed as one custom metric (3 in all, US$0.30 each a month).
* Alarms are billed per metric they evaluate (US$0.10 each a month): 16 in all, as `lambda_errors` sums one metric per Rudolph function (8, the ranker included), `dynamodb_throttles` adds reads and writes (2) and `api_4xx` divides 4xx responses by requests (2).

At us-east-1 prices that is about US$2.50 a month in total, independent of fleet size. Per-method API Gateway metrics and execution logging stay off, as they are billed per metric and per log line.

## Execution Events Table
Blocked and unknown executions, and those allowed by rules flagged for collection, are stored in a DynamoDB table for the web view. Its cost grows with the number of events Santa uploads (with Santa's default upload setting, a few US dollars a month for 1,000 Macs in monitor mode); see [Execution events table](./events-table.md#costs).

The hourly ranker, which builds the web view's top-blocked and top-allowed lists, reads 90 days of day counters and the machine markers of up to 800 binaries per run and writes up to 4,000 snapshot rows: about US$2 a month for 1,000 Macs and about US$19 a month for 100,000 Macs (up to about US$70 if the listed binaries all run on every Mac), mostly reading the day counters. See [Top binaries](./events-table.md#top-binaries).

## Costs of Rules
Due to Rudolph's sync logic, we have not observed noticable cost nor performance degradatation with very large numbers of
rules.


# Examining Costs
You can use AWS's billing dashboard to explore Rudolph-related costs. All Rudolph resources should be tagged with the
`Name = Rudolph` tag.
