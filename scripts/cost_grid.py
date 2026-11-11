# /// script
# requires-python = ">=3.9"
# ///
"""Monthly cost estimate for Rudolph by fleet size, global rule count and sync interval.

Prints the Markdown tables in docs/costs.md, "Estimates by fleet size, rule count and sync interval". Every
assumption is stated there and in the constants below.

    uv run scripts/cost_grid.py
    uv run scripts/cost_grid.py --macs 1000 --rules 500
    uv run scripts/cost_grid.py --macs 800,3000 --intervals 10,30 --prices my-region.json

Options:

    --macs 500,2000,5000        fleet sizes, one table each
    --rules 100,1000,10000      global rule counts
    --intervals 1,10,30,60      Santa FullSyncInterval, in minutes
    --ranker-runs 720           ranker runs a month (720 hourly; about 240 for 11 runs a weekday)
    --prices FILE.json          override unit prices

The default prices are us-east-1 on-demand list prices in US dollars, excluding tax and without any free tier,
from the AWS price list API (https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/<service>/current/us-east-1/
index.json) on 2026-11-07. Other regions differ, often by 10 to 30%. To estimate for another region, write a JSON
object with any of these keys (the rest keep their defaults) and pass it with --prices:

    api_requests_per_million     API Gateway REST API requests, US$ per million. Either one number, or tiers as
                                 [[upper bound in millions of requests a month, price], ...] with null as the
                                 last upper bound. Default [[333, 3.50], [1000, 2.80], [20000, 2.38], [null, 1.51]].
    lambda_requests_per_million  Lambda requests, US$ per million. Default 0.20.
    lambda_gb_second             Lambda compute, arm64, US$ per GB-second. Default 0.0000133334.
    ddb_write_per_million        DynamoDB on-demand write request units, US$ per million. Default 0.625.
    ddb_read_per_million         DynamoDB on-demand read request units, US$ per million. Default 0.125.
    ddb_storage_gb_month         DynamoDB standard table storage, US$ per GB-month. Default 0.25.
    ddb_pitr_gb_month            DynamoDB point-in-time recovery, US$ per GB-month. Default 0.20.
    logs_ingest_gb               CloudWatch Logs standard ingestion, US$ per GB. Default 0.50.
    logs_storage_gb_month        CloudWatch Logs storage, US$ per GB-month. Default 0.03.
    data_out_gb                  Data transfer out to the internet, first 10 TB, US$ per GB. Default 0.09.
    kms_key_month                KMS customer managed key, US$ a month. Default 1.00.
    alarm_metric_month           CloudWatch alarm, standard resolution, US$ per alarm metric a month. Default 0.10.
    custom_metric_month          CloudWatch custom metric, US$ a month. Default 0.30.
    s3_month                     S3: the Lambda source bucket and its access logs, US$ a month. Default 0.05.

For example, {"api_requests_per_million": 4.25, "ddb_write_per_million": 0.80, "data_out_gb": 0.12}.
"""

import argparse
import json
import math
import sys

DEFAULT_PRICES = {
    "api_requests_per_million": [[333, 3.50], [1000, 2.80], [20000, 2.38], [None, 1.51]],
    "lambda_requests_per_million": 0.20,
    "lambda_gb_second": 0.0000133334,
    "ddb_write_per_million": 0.625,
    "ddb_read_per_million": 0.125,
    "ddb_storage_gb_month": 0.25,
    "ddb_pitr_gb_month": 0.20,
    "logs_ingest_gb": 0.50,
    "logs_storage_gb_month": 0.03,
    "data_out_gb": 0.09,
    "kms_key_month": 1.00,
    "alarm_metric_month": 0.10,
    "custom_metric_month": 0.30,
    "s3_month": 0.05,
}

DAYS = 30.42
GB = 1e9

# Per sync, from the integration tests' measured capacity (COST2: 3 write units, 5.5 read units at steady state).
STEADY_WRITE_UNITS = 3
STEADY_READ_UNITS = 5.5
# Warm durations (assumed) and the configured memory of each handler: (GB, seconds).
PREFLIGHT = (0.256, 0.060)
RULEDOWNLOAD = (0.512, 0.060)
POSTFLIGHT = (0.512, 0.040)
EVENTUPLOAD = (0.256, 0.150)
LOG_BYTES_PER_REQUEST = 600  # router line, a handler line or two, START/END/REPORT, 26 bytes overhead per line
LOG_STORED_FRACTION = 0.15  # stored compressed (about 15% of ingested) for the 30 day retention
DATA_OUT_BYTES_PER_SYNC = 5000  # TLS handshake (certificate chain) and the three responses

# Clean syncs: periodic every 7 to 17 days (about 12 on average). Global rule pages hold the preflight batch_size
# (default 50) rules; each page Query reads about 1.6 read units for 50 small rules.
CLEAN_SYNCS_PER_MONTH = DAYS / 12
RULES_PER_PAGE = 50
READ_UNITS_PER_PAGE = 1.6
CLEAN_EXTRA_WRITE_UNITS = 2  # LastPageServedAt and the clean sync bookkeeping

# Events: Santa's default upload setting, monitor mode, about 50 a Mac a day, almost all ALLOW_UNKNOWN, which Santa
# uploads at the next sync; assume they fall within 8 hours of use a day.
EVENTS_PER_MAC_DAY = 50
EVENT_WRITE_UNITS = 6
EVENT_STORED_BYTES = 2700  # item about 600 bytes plus three indexes projecting every attribute
EVENT_ITEM_BYTES = 600
EVENT_DAYS_KEPT = 90
ACTIVE_MINUTES_PER_DAY = 480
INDEX_WRITES_PER_MAC_DAY = 2  # LastSeenDay in the two machine list indexes

# Ranker, per run (docs/events-table.md, "Top binaries"). Distinct binaries a day are a guess: about 2,000 at 1,000
# Macs and 50,000 at 100,000, i.e. growing with the fleet to the power 0.7. Each day counter is about 250 bytes,
# read at half a read unit per 4 KB for 90 days. About 300 listed binaries run on about a fifth of the Macs, with
# a 60 byte machine marker each: the unknown lists mostly repeat binaries already in the blocked and allowed ones.
# Snapshot rows (under 1 KB, one write unit) of the 20 lists grow from about 7,500 to the 10,000 cap. Up to 300
# global rule lookups (GetItem, half a read unit each) mark the unknown lists.
RANKER_DAYS = 90
RANKER_COUNTER_BYTES = 250
RANKER_LISTED_BINARIES = 300
RANKER_MARKER_SHARE = 0.2
RANKER_MARKER_BYTES = 60
RANKER_ROW_STORED_BYTES = 400
RANKER_ROWS_KEPT_DAYS = 8
RANKER_RULE_LOOKUPS = 300
RANKER_LAMBDA = (0.256, 10.0)

ALARM_METRICS = 33
CUSTOM_METRICS = 3
KMS_KEYS = 2


def parse_list(text: str) -> list:
    try:
        values = [int(v.replace("_", "")) for v in text.split(",") if v.strip()]
    except ValueError:
        raise argparse.ArgumentTypeError(f"expected comma-separated whole numbers, got {text!r}")
    if not values or any(v <= 0 for v in values):
        raise argparse.ArgumentTypeError(f"expected positive whole numbers, got {text!r}")
    return values


def load_prices(path) -> dict:
    prices = dict(DEFAULT_PRICES)
    if path is None:
        return prices
    with open(path, encoding="utf-8") as f:
        overrides = json.load(f)
    if not isinstance(overrides, dict):
        sys.exit(f"{path}: expected a JSON object of unit prices")
    unknown = sorted(set(overrides) - set(DEFAULT_PRICES))
    if unknown:
        sys.exit(f"{path}: unknown price keys {', '.join(unknown)}; known keys: {', '.join(DEFAULT_PRICES)}")
    prices.update(overrides)
    api = prices["api_requests_per_million"]
    if isinstance(api, (int, float)):
        prices["api_requests_per_million"] = [[None, api]]
    return prices


def tiered(quantity_millions: float, tiers) -> float:
    cost, lower = 0.0, 0.0
    for upper, price in tiers:
        top = quantity_millions if upper is None else min(quantity_millions, upper)
        if top > lower:
            cost += (top - lower) * price
        if upper is None or quantity_millions <= upper:
            break
        lower = upper
    return cost


def eventupload_requests_per_mac_day(interval_minutes: int) -> float:
    syncs = ACTIVE_MINUTES_PER_DAY / interval_minutes
    return syncs * (1 - math.exp(-EVENTS_PER_MAC_DAY / syncs))


def lambda_cost(p: dict, memory_seconds) -> float:
    memory, seconds = memory_seconds
    return p["lambda_requests_per_million"] / 1e6 + memory * seconds * p["lambda_gb_second"]


def ranker(p: dict, macs: int, runs: float) -> float:
    binaries_a_day = 2000 * (macs / 1000) ** 0.7
    counter_reads = binaries_a_day * RANKER_DAYS * RANKER_COUNTER_BYTES / 4096 * 0.5
    marker_reads = RANKER_LISTED_BINARIES * macs * RANKER_MARKER_SHARE * RANKER_MARKER_BYTES / 4096 * 0.5
    rule_reads = RANKER_RULE_LOOKUPS * 0.5
    rows = min(10000, 7500 + 1250 * math.log10(max(macs, 1000) / 1000))
    stored_gb = rows * RANKER_ROW_STORED_BYTES * runs * RANKER_ROWS_KEPT_DAYS / DAYS / GB
    return (runs * ((counter_reads + marker_reads + rule_reads) * p["ddb_read_per_million"] / 1e6
                    + rows * p["ddb_write_per_million"] / 1e6 + lambda_cost(p, RANKER_LAMBDA))
            + stored_gb * p["ddb_storage_gb_month"])


def fixed_without_ranker(p: dict) -> float:
    return (KMS_KEYS * p["kms_key_month"] + ALARM_METRICS * p["alarm_metric_month"]
            + CUSTOM_METRICS * p["custom_metric_month"] + p["s3_month"])


def row(p: dict, rules: int, macs: int, interval: int, ranker_runs: float) -> dict:
    syncs = macs * DAYS * 1440 / interval
    clean = macs * CLEAN_SYNCS_PER_MONTH
    pages = rules // RULES_PER_PAGE + 1
    extra_pages = clean * (pages - 1)
    uploads = macs * DAYS * eventupload_requests_per_mac_day(interval)
    requests = 3 * syncs + extra_pages + uploads
    events = macs * DAYS * EVENTS_PER_MAC_DAY
    write, read = p["ddb_write_per_million"] / 1e6, p["ddb_read_per_million"] / 1e6

    lam = (syncs * (lambda_cost(p, PREFLIGHT) + lambda_cost(p, RULEDOWNLOAD) + lambda_cost(p, POSTFLIGHT))
           + extra_pages * lambda_cost(p, RULEDOWNLOAD) + uploads * lambda_cost(p, EVENTUPLOAD))
    api = (tiered(requests / 1e6, p["api_requests_per_million"])
           + syncs * DATA_OUT_BYTES_PER_SYNC / GB * p["data_out_gb"])
    ddb = (syncs * (STEADY_WRITE_UNITS * write + STEADY_READ_UNITS * read)
           + clean * (pages * READ_UNITS_PER_PAGE * read + CLEAN_EXTRA_WRITE_UNITS * write)
           + macs * DAYS * INDEX_WRITES_PER_MAC_DAY * write)
    log_gb = requests * LOG_BYTES_PER_REQUEST / GB
    logs = log_gb * (p["logs_ingest_gb"] + LOG_STORED_FRACTION * p["logs_storage_gb_month"])
    ev = (events * EVENT_WRITE_UNITS * write
          + macs * EVENTS_PER_MAC_DAY * EVENT_DAYS_KEPT
          * (EVENT_STORED_BYTES * p["ddb_storage_gb_month"] + EVENT_ITEM_BYTES * p["ddb_pitr_gb_month"]) / GB)
    fixed = fixed_without_ranker(p) + ranker(p, macs, ranker_runs)
    total = lam + api + ddb + logs + ev + fixed
    return dict(requests=requests, lam=lam, api=api, ddb=ddb, logs=logs, ev=ev, fixed=fixed, total=total)


def per_million_syncs(p: dict) -> dict:
    """Marginal cost of a million steady-state syncs, with no events and no clean syncs."""
    m = 1e6
    return dict(
        api=3 * m * p["api_requests_per_million"][0][1] / 1e6,
        data_out=m * DATA_OUT_BYTES_PER_SYNC / GB * p["data_out_gb"],
        ddb=m * (STEADY_WRITE_UNITS * p["ddb_write_per_million"] + STEADY_READ_UNITS * p["ddb_read_per_million"]) / 1e6,
        lam=m * (lambda_cost(p, PREFLIGHT) + lambda_cost(p, RULEDOWNLOAD) + lambda_cost(p, POSTFLIGHT)),
        logs=3 * m * LOG_BYTES_PER_REQUEST / GB * (p["logs_ingest_gb"] + LOG_STORED_FRACTION * p["logs_storage_gb_month"]),
    )


def money(x: float) -> str:
    if x >= 100:
        return f"{x:,.0f}"
    if x >= 10:
        return f"{x:.0f}"
    return f"{x:.1f}"


def requests_text(requests: float) -> str:
    millions = requests / 1e6
    return f"{millions:,.0f} M" if millions >= 100 else f"{millions:.1f} M"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--macs", type=parse_list, default=[500, 2000, 5000], help="fleet sizes (default 500,2000,5000)")
    parser.add_argument("--rules", type=parse_list, default=[100, 1000, 10000], help="global rule counts (default 100,1000,10000)")
    parser.add_argument("--intervals", type=parse_list, default=[1, 10, 30, 60], help="sync intervals in minutes (default 1,10,30,60)")
    parser.add_argument("--ranker-runs", type=float, default=720, help="ranker runs a month (default 720, hourly)")
    parser.add_argument("--prices", help="JSON file of unit prices to override (see the docstring for the keys)")
    args = parser.parse_args()
    p = load_prices(args.prices)

    for macs in args.macs:
        print(f"\n{macs:,} Macs, monitor mode, US$ a month:\n")
        print("| Rules | Interval | Requests/month | Lambda | API Gateway | DynamoDB | Logs | Events | Fixed | Total |")
        print("|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
        for interval in args.intervals:
            for rules in args.rules:
                r = row(p, rules, macs, interval, args.ranker_runs)
                print(f"| {rules:,} | {interval} min | {requests_text(r['requests'])} | {money(r['lam'])} | "
                      f"{money(r['api'])} | {money(r['ddb'])} | {money(r['logs'])} | {money(r['ev'])} | "
                      f"{money(r['fixed'])} | **{money(r['total'])}** |")

    s = per_million_syncs(p)
    print(f"\nPer million syncs (first API Gateway tier): US${sum(s.values()):.2f}: API Gateway requests "
          f"{s['api']:.2f}, data transfer out {s['data_out']:.2f}, DynamoDB {s['ddb']:.2f}, Lambda {s['lam']:.2f}, "
          f"CloudWatch Logs {s['logs']:.2f}.")

    weekday_runs = 11 * 5 * DAYS / 7
    print(f"\nFixed, US$ a month: {fixed_without_ranker(p):.2f} without the ranker. Ranker:\n")
    print(f"| Macs | Ranker, {args.ranker_runs:,.0f} runs | Ranker, 11 runs a weekday ({weekday_runs:.0f} runs) |")
    print("|---:|---:|---:|")
    for macs in args.macs:
        print(f"| {macs:,} | {ranker(p, macs, args.ranker_runs):.2f} | {ranker(p, macs, weekday_runs):.2f} |")

    for macs in args.macs:
        gb = macs * EVENTS_PER_MAC_DAY * EVENT_DAYS_KEPT * EVENT_STORED_BYTES / GB
        print(f"\nEvents table at {macs:,} Macs: about {gb:,.0f} GB stored (DynamoDB's always-free tier covers 25 GB).", end="")
    print()


if __name__ == "__main__":
    main()
