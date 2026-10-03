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
The API stage is throttled (`api_throttling_rate_limit`, default 100 requests per second, burst 200), so a runaway
client or a flood of requests gets HTTP 429 from API Gateway instead of running up Lambda and DynamoDB charges.

The alarms are kept few and cheap: six alarms on standard metrics, three of them fed by metric filters on the Lambda
logs (filters are free; each extracted metric is one custom metric). Expect a few US dollars a month in total,
independent of fleet size. Per-method API Gateway metrics and execution logging stay off, as they are billed per
metric and per log line.

## Costs of Rules
Due to Rudolph's sync logic, we have not observed noticable cost nor performance degradatation with very large numbers of
rules.


# Examining Costs
You can use AWS's billing dashboard to explore Rudolph-related costs. All Rudolph resources should be tagged with the
`Name = Rudolph` tag.
