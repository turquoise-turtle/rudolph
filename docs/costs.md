# Costs
So, what does Rudolph cost?

## Minimum Costs
Suppose you have a fleet with only 1 machine, sync'ing once every 10 minutes.

We've observed the cost "floor" of Rudolph to be approximately $0.10 per day, plus whatever costs associated with your
domain name registration (typically like $0.50 a month and $20 a year or something).

That floor leaves out fixed monthly items, which do not grow with the fleet:

* KMS customer managed keys, US$1 a month each: two, the DynamoDB tables' key and the Lambda source bucket's key. When Terraform creates the Firehose event sink (`eventupload_handler` `FIREHOSE` with `eventupload_autocreate_policies`), its bucket's key adds one more. The access-log buckets use SSE-S3 and need no key.
* The Route 53 hosted zone, US$0.50 a month, when Terraform creates it (`use_existing_route53_zone` false).
* Point-in-time recovery on the DynamoDB tables, charged per GB stored: tiny for a small fleet.

## Costs At Scale
AWS costs scale roughly linearly with the number of syncs, which is the number of Macs online times their sync rate (Santa's `FullSyncInterval`). The number of rules hardly matters (see [Costs of Rules](#costs-of-rules)).

The original Rudolph project reported, for about 1,000 rules and 10,000 Santa clients syncing every 60 seconds, about US$50 a day (lower on weekends), or around US$900 a month. That was observed on a fleet of laptops, many of them asleep or offline at any time, with an earlier version of Rudolph. At today's per-sync cost (about US$16 per million syncs at us-east-1 list prices, see below) the same fleet would cost about US$6,300 a month if every Mac stayed online around the clock; scale that by the share of the time your Macs are actually online and syncing.

Santa's default sync interval is 10 minutes, 10 times less frequent than above, so the sync-driven costs are about 90% lower. A fleet 90% smaller (1,000 Macs instead of 10,000) likewise costs about 90% less, apart from the fixed monthly items.

## Estimates by fleet size, rule count and sync interval
Monthly estimates for 500, 2,000 and 5,000 Macs with 100, 1,000 or 10,000 global rules, syncing every 1, 10, 30 or 60 minutes. They follow how Rudolph behaves today, from its code and its integration tests; `scripts/cost_grid.py` reproduces the tables (`uv run scripts/cost_grid.py`), so change an assumption there and paste the output back here. This is an estimate: round it further rather than trusting the last digit.

Santa accepts a `FullSyncInterval` of 60 seconds or more (`kMinimumFullSyncInterval` in `testdata/santa-2026.9/SNTSyncConstants.mm`) and uses 600 seconds (10 minutes) by default; Rudolph refuses a configured interval below 60 seconds. So 1 minute is possible, but it is the minimum.

### Your own region and fleet
Prices differ between regions, often by 10 to 30% above us-east-1. To estimate for your own fleet, pass its size, rule counts and sync intervals, and a JSON file with your region's unit prices from the [AWS pricing pages](https://aws.amazon.com/pricing/) or the AWS price list API; any price you leave out keeps its us-east-1 default, and the script's docstring lists the keys:

```sh
cat > prices.json <<'EOF'
{"api_requests_per_million": 4.25, "ddb_write_per_million": 0.80, "ddb_read_per_million": 0.16, "data_out_gb": 0.12}
EOF
uv run scripts/cost_grid.py --macs 800,1200 --rules 300 --intervals 10,30 --prices prices.json
```

The figures in that example file are placeholders, not any region's prices. `--ranker-runs` sets how often the ranker runs a month (default 720, hourly; see [Ranker schedule](deploy.md#ranker-schedule)).

Assumptions:

* **Region and prices.** us-east-1 on-demand list prices from the AWS price list API on 2026-11-07, **without** the free tier (see the note after the tables), in US dollars and excluding tax.
* **Every Mac is online and syncing around the clock.** This is the upper bound. A fleet that is only online 50 hours a week (office hours) syncs about 30% as often: scale every column except Events and Fixed by the share of the time your Macs are online.
* **A sync with no changes** is 3 requests: preflight, one ruledownload page (the feed of rule changes since the Mac's last completed syncs, with the machine and group rules at the end of that page) and postflight. Eventupload is only called when there are events (Santa skips it otherwise). Santa only calls `/xsrf` after a 403 and never calls `/health`, so neither is counted.
* **DynamoDB per sync**, as measured by the integration tests (`TestCOST2_SteadyStateSyncCapacityBudget`): 3 write units and 5.5 read units for a Mac with 1,000 global and 100 machine rules; `TestC3_SteadyStateSyncIsCheapOnDynamoDB` checks it makes no `Scan`. An incremental sync reads only feed rows written since its cursor (10 minutes before the older of the Mac's last two completed syncs), so it does not depend on the number of global rules. Each Mac also costs the two machine list indexes about 2 writes a day (`LastSeenDay`).
* **Rule changes**: a few a day (say 5). A changed rule reaches each Mac in the feed page its next syncs read anyway, a row of about 150 bytes inside a query already counted, so rule changes add no requests and next to no capacity.
* **Clean syncs** happen periodically, every 7 to 17 days depending on the machine ID (about 2.5 a Mac a month), plus a Mac's first sync, a clean sync requested with `rudolph config update --machine <uuid> --clean-sync`, a Mac reporting no rules and a Santa upgrade across 2026.6 (rare; not counted). A clean sync sends every global rule in pages of the preflight's `batch_size` (default 50): about 3, 21 and 201 ruledownload pages for 100, 1,000 and 10,000 rules, each page about 1.6 read units, plus about 2 more write units for its bookkeeping. This is the only place the rule count shows: at 5,000 Macs it adds about US$1 a month for 1,000 rules and about US$12 for 10,000.
* **Lambda** (arm64, billed per millisecond): warm durations of about 60 ms for preflight (256 MB), 60 ms per ruledownload page (512 MB), 40 ms for postflight (512 MB) and 150 ms for eventupload (256 MB). Cold starts add little at these request rates.
* **Authoriser**: the whole fleet shares one sync token, and API Gateway caches the authoriser's decision per token for `authorizer_result_ttl_in_seconds` (300), so it runs roughly once per 5 minutes for the whole fleet (about 9,000 invocations a month, under US$0.01), however many Macs sync.
* **Logs**: about 600 bytes of CloudWatch Logs per Lambda invocation (Rudolph's request line and a handler line or two, Lambda's START/END/REPORT lines, and CloudWatch's 26 bytes per log event), kept 30 days (`log_retention_in_days`). API Gateway execution logging is off.
* **Data transfer out** to the Macs: about 5 KB a sync (TLS handshake with the certificate chain, and the three responses), counted in the API Gateway column.
* **Events**: Santa's default upload setting in **monitor mode**, about 50 events a Mac a day (rudolph-web's `docs/data-layer-design.md`; replace it with your fleet's real figure from the eventupload logs). They do not depend on the sync interval: about 6 write units each (event item, its three index entries, its share of the day counter and machine marker) and, kept 90 days, about 2.7 KB of table storage each (the item and three indexes that project every attribute) plus point-in-time recovery of the item. Santa uploads blocked executions straight away but monitor mode's `ALLOW_UNKNOWN` executions at the next sync, so with events spread over 8 hours of use a day, the number of eventupload requests does depend on the interval (about 47, 31, 15 and 8 a Mac a day at 1, 10, 30 and 60 minutes). In **lockdown mode** (about 5 events a Mac a day) the Events column is about a tenth of the figure shown.
* **Fixed** (see [Fixed monthly costs](#fixed-monthly-costs)): US$10.10, US$10.90 and US$11.90 for 500, 2,000 and 5,000 Macs, with the ranker running hourly.

Unit prices used (us-east-1, on-demand, list):

| Item | Price (US$) |
|---|---|
| API Gateway REST API requests | 3.50 per million (first 333 million a month), 2.80 (next 667 million), 2.38 (next 19 billion) |
| Lambda requests | 0.20 per million |
| Lambda compute, arm64 | 0.0000133334 per GB-second |
| DynamoDB on-demand writes / reads | 0.625 / 0.125 per million request units |
| DynamoDB storage / point-in-time recovery | 0.25 / 0.20 per GB-month |
| CloudWatch Logs ingestion / storage | 0.50 per GB / 0.03 per GB-month |
| CloudWatch alarms / custom metrics | 0.10 per alarm metric / 0.30 per metric a month |
| Data transfer out to the internet | 0.09 per GB (first 10 TB) |
| KMS customer managed key | 1.00 per key a month |
| Route 53 hosted zone | 0.50 a month |

500 Macs, monitor mode, US$ a month:

| Rules | Interval | Requests/month | Lambda | API Gateway | DynamoDB | Logs | Events | Fixed | Total |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 100 | 1 min | 66.4 M | 33 | 242 | 56 | 20 | 4.6 | 10 | **366** |
| 1,000 | 1 min | 66.5 M | 33 | 242 | 56 | 20 | 4.6 | 10 | **367** |
| 10,000 | 1 min | 66.7 M | 33 | 243 | 56 | 20 | 4.6 | 10 | **368** |
| 100 | 10 min | 7.0 M | 3.6 | 26 | 5.6 | 2.1 | 4.6 | 10 | **52** |
| 1,000 | 10 min | 7.1 M | 3.6 | 26 | 5.6 | 2.1 | 4.6 | 10 | **52** |
| 10,000 | 10 min | 7.3 M | 3.7 | 27 | 5.7 | 2.2 | 4.6 | 10 | **53** |
| 100 | 30 min | 2.4 M | 1.3 | 8.8 | 1.9 | 0.7 | 4.6 | 10 | **27** |
| 1,000 | 30 min | 2.4 M | 1.3 | 8.9 | 1.9 | 0.7 | 4.6 | 10 | **28** |
| 10,000 | 30 min | 2.7 M | 1.4 | 9.7 | 1.9 | 0.8 | 4.6 | 10 | **29** |
| 100 | 60 min | 1.2 M | 0.6 | 4.4 | 1.0 | 0.4 | 4.6 | 10 | **21** |
| 1,000 | 60 min | 1.2 M | 0.6 | 4.5 | 1.0 | 0.4 | 4.6 | 10 | **21** |
| 10,000 | 60 min | 1.5 M | 0.8 | 5.3 | 1.0 | 0.4 | 4.6 | 10 | **22** |

2,000 Macs, monitor mode, US$ a month:

| Rules | Interval | Requests/month | Lambda | API Gateway | DynamoDB | Logs | Events | Fixed | Total |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 100 | 1 min | 266 M | 132 | 969 | 225 | 80 | 19 | 11 | **1,436** |
| 1,000 | 1 min | 266 M | 132 | 970 | 225 | 80 | 19 | 11 | **1,437** |
| 10,000 | 1 min | 267 M | 133 | 973 | 225 | 81 | 19 | 11 | **1,441** |
| 100 | 10 min | 28.2 M | 14 | 103 | 23 | 8.5 | 19 | 11 | **177** |
| 1,000 | 10 min | 28.3 M | 14 | 103 | 23 | 8.6 | 19 | 11 | **178** |
| 10,000 | 10 min | 29.2 M | 15 | 106 | 23 | 8.8 | 19 | 11 | **182** |
| 100 | 30 min | 9.7 M | 5.0 | 35 | 7.6 | 2.9 | 19 | 11 | **80** |
| 1,000 | 30 min | 9.8 M | 5.1 | 36 | 7.6 | 3.0 | 19 | 11 | **81** |
| 10,000 | 30 min | 10.7 M | 5.6 | 39 | 7.8 | 3.2 | 19 | 11 | **85** |
| 100 | 60 min | 4.9 M | 2.5 | 18 | 3.8 | 1.5 | 19 | 11 | **55** |
| 1,000 | 60 min | 5.0 M | 2.6 | 18 | 3.8 | 1.5 | 19 | 11 | **55** |
| 10,000 | 60 min | 5.9 M | 3.1 | 21 | 4.0 | 1.8 | 19 | 11 | **60** |

5,000 Macs, monitor mode, US$ a month:

| Rules | Interval | Requests/month | Lambda | API Gateway | DynamoDB | Logs | Events | Fixed | Total |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 100 | 1 min | 664 M | 331 | 2,192 | 561 | 201 | 46 | 12 | **3,344** |
| 1,000 | 1 min | 665 M | 331 | 2,192 | 562 | 201 | 46 | 12 | **3,344** |
| 10,000 | 1 min | 667 M | 332 | 2,199 | 562 | 202 | 46 | 12 | **3,353** |
| 100 | 10 min | 70.5 M | 36 | 256 | 56 | 21 | 46 | 12 | **428** |
| 1,000 | 10 min | 70.7 M | 36 | 257 | 56 | 21 | 46 | 12 | **429** |
| 10,000 | 10 min | 73.0 M | 37 | 265 | 57 | 22 | 46 | 12 | **440** |
| 100 | 30 min | 24.3 M | 13 | 88 | 19 | 7.3 | 46 | 12 | **185** |
| 1,000 | 30 min | 24.5 M | 13 | 89 | 19 | 7.4 | 46 | 12 | **186** |
| 10,000 | 30 min | 26.8 M | 14 | 97 | 19 | 8.1 | 46 | 12 | **197** |
| 100 | 60 min | 12.2 M | 6.3 | 44 | 9.6 | 3.7 | 46 | 12 | **122** |
| 1,000 | 60 min | 12.4 M | 6.4 | 45 | 9.6 | 3.8 | 46 | 12 | **123** |
| 10,000 | 60 min | 14.7 M | 7.8 | 53 | 10 | 4.4 | 46 | 12 | **134** |

Per sync this is about US$16 per million syncs: API Gateway about US$11 (US$10.50 for the 3 requests, about US$0.45 for data transfer out), DynamoDB about US$2.60, Lambda about US$1.50 and CloudWatch Logs about US$0.90. Above 333 million requests a month (about 2,500 Macs syncing every minute), API Gateway's lower tiers bring the per-sync cost down a little.

The free tier is left out above. AWS's always-free allowances (1 million Lambda requests and 400,000 GB-seconds a month, 25 GB of DynamoDB storage, 5 GB of CloudWatch Logs, 10 alarms and 10 custom metrics, 100 GB of data transfer out) take a few dollars to about US$25 off the monthly total, if no other workload in the account uses them; the events table's storage (about 6, 24 and 61 GB at 500, 2,000 and 5,000 Macs) fits in the 25 GB up to about 2,000 Macs. API Gateway's free tier lasts only the account's first 12 months.

Takeaways:

* API Gateway's per-request price is about two thirds of every sync-driven figure, followed by DynamoDB; at intervals of 30 minutes and more, the events table and the fixed items become a large share of a small bill.
* The sync interval matters far more than the rule count: 1-minute syncs cost about 8 times as much as Santa's 10-minute default and 20 to 30 times as much as 60-minute syncs, while 1,000 rules instead of 100 change the total by about a dollar or less, because only the periodic clean syncs page through the global rules. Even 10,000 rules add only a few per cent at a 10-minute interval.
* For most fleets, keep Santa's default of 10 minutes (about US$52, US$180 and US$430 a month for 500, 2,000 and 5,000 Macs if every Mac is always online, much less in practice), or 30 minutes (about US$27, US$80 and US$185) if a rule change or temporary mode may take up to half an hour to arrive; `santactl sync` applies a change at once on a Mac that needs it. Avoid 1 minute: it is Santa's minimum and costs about US$365, US$1,435 and US$3,345 a month for little gain.

## Fixed monthly costs
These do not depend on the sync interval, and apart from the ranker not on the number of Macs (us-east-1 list prices; `scripts/cost_grid.py` prints the ranker figures for other fleet sizes and `--ranker-runs`):

| Item | US$ a month |
|---|---|
| 2 KMS customer managed keys (DynamoDB tables, Lambda source bucket); a Firehose sink created by Terraform adds a third | 2.00 |
| CloudWatch alarms: 33 alarm metrics and 3 custom metrics (see [Throttling and Alarms](#throttling-and-alarms)); 3.80 without an event sink | 4.20 |
| Ranker, hourly (`rate(1 hour)`, 720 runs): 500 / 2,000 / 5,000 Macs in monitor mode | about 3.90 / 4.60 / 5.70 |
| Ranker on weekdays in working hours only (for example `cron(0 8-18 ? * MON-FRI *)`: 11 runs a weekday, about 240 a month): 500 / 2,000 / 5,000 Macs | about 1.30 / 1.55 / 1.90 |
| S3: the Lambda source bucket and its access logs | under 0.05 |
| EventBridge schedule, SSM standard parameters, the cost budget | free |
| Route 53 hosted zone, only when Terraform creates it (`use_existing_route53_zone` false), so not counted above; alias queries to API Gateway are free | 0.50 |

With the ranker restricted to working hours, the Fixed column above drops by about US$2.60 (500 Macs), US$3.10 (2,000 Macs) or US$3.80 (5,000 Macs).

## rudolph-web
The web view (a Lambda Function URL, optionally behind CloudFront) has no idle cost: it is billed per request only. Assuming light admin use, a few admins and about 200 page views a working day (about 5,000 requests a month, each a Lambda invocation of a few hundred milliseconds at 256 MB and a few DynamoDB queries), it costs under US$0.10 a month in Lambda, DynamoDB reads and logs. CloudFront adds about US$0.01 per 10,000 HTTPS requests and US$0.085 per GB to viewers in North America and Europe (`PriceClass_100`, the example's setting, serves viewers elsewhere from North American and European edges at those prices): a few cents a month at this use, and within CloudFront's always-free 1 TB and 10 million requests a month.

## Authorizer and Logs
API Gateway caches the authorizer's decision per token for `authorizer_result_ttl_in_seconds` (default 300 seconds),
so a sync normally costs no authorizer Lambda invocation beyond the first one in each cache window, instead of one per
request.

CloudWatch log groups keep logs for `log_retention_in_days` (default 30) instead of forever.

## Throttling and Alarms
The API is throttled per route (`api_throttling_rate_limit`, default 2,000 requests per second on each of the 6 routes, burst 2,000), so a runaway client or a flood of requests gets HTTP 429 from API Gateway instead of running up Lambda and DynamoDB charges. The defaults leave room for the normal load of about 100,000 Macs; see [deploy](./deploy.md#0d-check-the-api-throttle) for why the throttle must stay above the fleet's peak.

The alarms are kept few and cheap: ten alarms, or nine when no event sink is configured (see `monitoring.tf` and [deploy](./deploy.md#alarms)).

* Seven alarm on metrics AWS publishes for free: `api_5xx`, `api_4xx`, `lambda_errors` and the four DynamoDB throttle alarms (`dynamodb_store_read_throttles`, `dynamodb_store_write_throttles`, `dynamodb_events_read_throttles` and `dynamodb_events_write_throttles`).
* Three alarm on metrics that metric filters extract from the Lambda logs: `request_too_large`, `lambda_timeouts` and `event_sink_failures`. The filters are free; each extracted metric is billed as one custom metric (3 in all, US$0.30 each a month).
* Alarms are billed per metric they evaluate (US$0.10 each a month): 33 in all, as `lambda_errors` sums one metric per Rudolph function (7: the ranker included, the authoriser left out), the DynamoDB throttle alarms sum each table with each of its global secondary indexes, whose throttles DynamoDB reports separately (20: the rule store and its five indexes, and the events table and its three, once for reads and once for writes) and `api_4xx` divides 4xx responses by requests (2).

That is about US$4.20 a month in total at us-east-1 prices (US$3.80 without an event sink) (the same in many regions), independent of fleet size. Per-method API Gateway metrics and execution logging stay off, as they are billed per metric and per log line.

## Budgets
The optional monthly cost budget (`monthly_budget_usd`, see [deploy](./deploy.md#cost-budget)) is a plain cost budget with email notifications and no budget actions. AWS's [Budgets pricing page](https://aws.amazon.com/aws-cost-management/aws-budgets/pricing/) says that monitoring budgets and receiving their notifications is free of charge; only budgets with actions (which change IAM permissions, service control policies or resources when a threshold is reached) are charged, after the first two in an account, per budget per day, and Budget Reports are charged per report delivered. Rudolph's budget uses neither. Check that page for current prices.

The budget only sees costs tagged `Name = Rudolph` once `Name` is activated as a cost allocation tag (see below), and it only alerts: it never stops spending.

## Execution Events Table
Blocked and unknown executions, and those allowed by rules flagged for collection, are stored in a DynamoDB table for the web view. Its cost grows with the number of events Santa uploads (with Santa's default upload setting, about US$9 a month per 1,000 Macs in monitor mode at us-east-1 list prices, or about US$6 when its storage fits in the free tier, and about a tenth of that in lockdown mode; see [Estimates](#estimates-by-fleet-size-rule-count-and-sync-interval)); see [Execution events table](./events-table.md#costs).

The ranker, which builds the web view's top-blocked, top-allowed and top-unknown lists, reads 90 days of day counters, the machine markers of up to 2,000 binaries (usually a few hundred) and up to about 300 global rules per run, and writes up to 10,000 snapshot rows: about US$4 a month for 1,000 Macs and about US$21 a month for 100,000 Macs (up to about US$150 if the listed binaries all run on every Mac) at us-east-1 prices, mostly reading the day counters; other regions differ, and running it only in working hours on weekdays (11 runs a weekday) cuts it by about two thirds (see [Fixed monthly costs](#fixed-monthly-costs)). See [Top binaries](./events-table.md#top-binaries).

## Costs of Rules
The number of global rules barely affects cost. A sync with no changes reads only the rules feed since the Mac's last completed syncs, not the global rules, so it costs the same with 100 rules or 80,000. Only clean syncs (a Mac's first sync, then every 7 to 17 days) page through every global rule, in pages of the preflight's `batch_size` (default 50); for 1,000 rules that is about 20 extra small requests per Mac every couple of weeks, about a dollar a month for 5,000 Macs (see [Estimates](#estimates-by-fleet-size-rule-count-and-sync-interval)). Machine and group rules are read on every sync (about 150 KB, 20 read units at eventual consistency, for 1,000 of them on one Mac), so a Mac with very many machine rules does cost more per sync.


## Group members
The member rows that `rudolph group members` lists (`GroupMembers#<group>`/`<machine id>`, about 150 bytes each, one per Mac and group it is assigned or reports) cost nothing on a steady-state sync: preflight only touches them when a Mac's reported groups change. Such a change, or a `rudolph group add|remove`, reads the Mac's groups item once, strongly consistent (one read unit), and writes it with the row of each group joined or left in one transaction. Transactional writes cost double, so that is about two extra write units per group joined or left, plus one for the groups item itself. `rudolph group members --rebuild` scans the whole table once (one read unit per 4 KB at strong consistency, so a few dollars at most even for a large table) and writes only the rows that are missing or wrong.

# Examining Costs
You can use AWS's billing dashboard to explore Rudolph-related costs. All Rudolph resources should be tagged with the
`Name = Rudolph` tag.

To filter on that tag in Cost Explorer and Budgets, activate `Name` as a cost allocation tag in the Billing and Cost Management console (Cost allocation tags). It takes up to 24 hours and applies to costs from activation onwards. In an account that also holds other resources, always filter on the tag; without it, Cost Explorer shows the whole account.
