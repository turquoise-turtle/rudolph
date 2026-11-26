# Fleet health and Lockdown readiness

Once a day a Lambda of its own, the fleet job (`PREFIX_rudolph_fleet`, code in `cmd/fleet` and `internal/fleet`), walks every Mac and answers two questions for the web view (rudolph-web):

- **Which Macs need attention?** Six health lists: Macs that have stopped syncing, that start syncs without finishing them, that are not in the client mode Rudolph intends, whose rules did not apply, that have SIP reduced, or that run an old Santa.
- **Which Macs are ready for Lockdown?** For each Mac, how many unknown binaries it ran in the last 7 and 30 days, and how many of those no global rule covers yet; and per device group, how many Macs are ready, nearly ready, far off, not measurable or already locked.

The job writes its answers to the events table as a snapshot. A page view reads one pointer item and one bounded `Query`, never the fleet. Rudolph's sync never reads any of it. The item layouts are in [Events table](events-table.md#fleet-health-and-readiness-rows).

The figures are as old as the last run: up to a day with the default schedule. The pointer's `GeneratedAt` says when that was.

## Health categories

A Mac can be in several categories. The pointer's `Attention` counts the Macs in at least one.

| Category | A Mac is listed when | Listed in order of |
|---|---|---|
| `Stale` | its last completed sync is more than the stale threshold ago (10 days unless changed, see [Holiday mode](#the-stale-threshold-and-holiday-mode)), or there is none on record (`Never`). Exactly the threshold is not stale. | last completed sync, oldest first; Macs that never completed one before the others |
| `Incomplete` | its current sync has no postflight, its preflight is at least 30 minutes old and not older than the stale threshold: the Mac reaches Rudolph but does not finish. | preflight time, oldest first |
| `Mode` | the client mode the Mac last reported differs from the one Rudolph intends for it now, and the Mac is not `Stale`. | machine ID |
| `Rules` | its last sync completed but applied fewer rules than it received (`Reason` `NotApplied`), or its rules hash differs from what most Macs with the same rules report (`Reason` `Outlier`). | machine ID |
| `SIP` | it reports System Integrity Protection flags other than 0. `Flags` is the value in hex, for example `0x7f`. A Mac that reports none, or 0, has SIP fully on. | machine ID |
| `Version` | its Santa release is below the minimum Santa version: `2026.9` unless changed with `rudolph fleet min-santa-version`, which can also turn the list `off` (see [The minimum Santa version](#the-minimum-santa-version)). Releases compare as numbers, so 2026.10 is newer than 2026.9. | release, oldest first |

Details that matter when reading a list:

- **Last completed sync** is the postflight time of the Mac's current sync or, when that sync has no postflight, the start of the newest earlier sync that completed.
- **A new Mac** is `Stale` with `Never` until its first sync completes, usually seconds after it starts. A Mac whose first sync started in the minutes before a run shows up in that run's list.
- **`Incomplete` points at the server, `Stale` at the Mac.** Macs that are switched off go stale; Macs that keep reaching preflight without finishing usually mean a failing rule download or postflight. A sync that started less than 30 minutes before the run is simply in progress.
- **Intended mode** is resolved the way preflight resolves it: a temporary client mode while it lasts, the Mac's own, the strictest of its groups' (`LOCKDOWN` over `STANDALONE` over `MONITOR`), the global configuration's, and `MONITOR` when there is no global configuration. `Source` says which one decided: `temporary`, `own`, `group:<name>`, `global` or `default`.
- **A mode change takes one sync to arrive**, so for one sync interval after you change a mode the `Mode` list shows the Macs that have not synced since. It is a list of a rollout in progress as much as of faults.
- **A stored mode that is not a client mode** (a value edited by hand) is skipped, and the next setting in the order decides. Preflight sends no mode at all for such a value, so the Mac keeps the mode it has; the list then shows it against the mode the rest of its configuration asks for, which is how you find the broken setting.
- **Version unknown.** A Mac with an empty or unparseable Santa version is counted in `VersionUnknown` and not listed. It is counted while the minimum Santa version is `off` too: it says the Mac's data is poor, not that its release is old.
- **Macs without a serial number or hostname** are walked and listed like any other; their rows leave those fields out.

### Rules hash outliers

Rudolph does not compute the hash Santa should report, so the job compares Macs with each other. Macs whose sync state has the same digest of machine and group rules (`MachineRulesTracked`) are sent the same rules, so after a completed sync they should report the same hash.

- Only Macs whose current sync completed in the last 24 hours and reported a hash are compared. Macs whose digest is empty or marked dirty are left out.
- Only Macs that have synced since the last global rule change are compared. Every global rule that is added, changed or removed writes a row to the rules feed (the store table's `RulesFeed` partition), and the job reads the newest one, once a run. A Mac takes part when its sync completed more than 10 minutes after that row's time. A Mac that last synced before it has not had the change yet, and one that synced within the 10 minutes may or may not have been sent it, so neither is counted or listed.
- When the rules feed has no row (a fresh install, or no global rule change in the 90 days a feed row lives), every Mac of the first bullet takes part.
- In each set of at least three Macs that take part, the majority hash is the one more than half of them report. A two-all split has no majority.
- A Mac that takes part is an outlier when its hash differs from the majority's. `ExpectedHash` is the majority's.
- A Mac already listed as `NotApplied` is not listed a second time.

Limits of this check, which make `Outlier` a prompt to look, not proof:

- A Mac with rules nobody shares cannot be judged. `NotApplied` still covers it.
- **After a global rule change the check starts again.** Until at least three Macs with the same rules have completed a sync more than 10 minutes after the change, none of them is judged, so on a fleet whose global rules change every day, the Macs that sync rarely are seldom compared. A Mac that is ahead is no longer listed as one that is wrong: Macs that have not synced since the change are not counted towards the majority.
- **Only global rule changes are seen.** The rules feed records global rules. It does not need to record machine and group rules: `MachineRulesTracked` is a digest of the ones a Mac was sent, so a Mac that receives a change to those moves to another set rather than being compared with the Macs still waiting for it.
- **A feed row edited by hand.** If the newest `RulesFeed` row's sort key does not start with a time, the job cannot tell when the rules last changed. It skips the outlier check for that run and logs `rules hash outliers not checked in this run`, rather than comparing every Mac and listing the ones that are up to date. `NotApplied` is unaffected.
- Transitive rules, which Santa creates locally, may change a Mac's hash. This has not been tested on a real Mac.

## The stale threshold and holiday mode

One item of the store table holds the threshold, and the [minimum Santa version](#the-minimum-santa-version): PK `FleetHealth`, SK `Config`.

| Attribute | Meaning |
|---|---|
| `StaleDays` | the normal threshold in days; 10 when absent |
| `HolidayStaleDays` | the threshold while a holiday lasts |
| `HolidayUntil` | the holiday's last day, a UTC date (`YYYY-MM-DD`), inclusive |
| `MinSantaVersion` | the release below which a Mac is listed under `Version`, such as `2026.10`, or `off`; `2026.9` when absent |

Over school holidays Macs are simply off, and a 10-day threshold would list most of the fleet. Holiday mode raises the threshold until a date:

```sh
./rudolph fleet holiday --stale-days 60 --until 2027-02-01
./rudolph fleet holiday --end
```

- The job reads the item on every run. While today (UTC) is on or before `HolidayUntil`, the threshold is `HolidayStaleDays`; the day after, the normal threshold applies again without anyone ending the holiday.
- The threshold in force decides `Stale`, the upper bound of `Incomplete`, and the `stale` reason of readiness. The pointer records it: `StaleDays` (the threshold the run used), `Holiday` and, while a holiday is on, `HolidayUntil`.
- A holiday is refused when its stale days are outside 1 to 365 or below the normal threshold, or when its last day is not a date, is in the past, or is more than 366 days ahead. See [`rudolph fleet holiday`](cli.md#fleet-holiday).
- A change takes effect at the job's next run. To see it sooner, invoke the function by hand (see [Running it by hand](#running-it-by-hand)).
- The CLI does not set `StaleDays`. rudolph-web writes the item through the same package, `pkg/model/fleethealth`, which holds the validation and the "threshold in force" rule for both.
- A value in the item that cannot be used (edited by hand to a string, or out of range) is ignored and the default applies, so a bad edit never stops the job.

## The minimum Santa version

A Mac is listed under `Version` when its Santa release is below the minimum Santa version, the `MinSantaVersion` attribute of the same item. With none stored the minimum is `2026.9`.

```sh
./rudolph fleet show
./rudolph fleet min-santa-version 2026.10
./rudolph fleet min-santa-version off
./rudolph fleet min-santa-version --default
```

- **A release** such as `2026.10` or `2026.10.1`, at most 32 characters. Only the year and month are compared, so `2026.10.1` lists the same Macs as `2026.10`. A minimum below `2026.9` is allowed.
- **`off`** turns the list off: no Mac is listed under `Version`, its count is 0, and no Mac counts towards `Attention` for its release. The other five lists and `VersionUnknown` are unchanged.
- **`--default`** removes the stored value, so `2026.9` applies again.
- The job reads the item on every run, so a change takes effect at the next run with no deploy. To see it sooner, invoke the function by hand (see [Running it by hand](#running-it-by-hand)). The pointer's `MinSantaVersion` records the minimum the run used, or `off`, so a reader knows which minimum the lists on show were built with.
- A stored value that cannot be used (edited by hand to something that is neither a release nor `off`) is ignored and `2026.9` applies. `rudolph fleet show` then reports the default, and `rudolph fleet min-santa-version --default` removes the value.
- The environment variable `FLEET_MIN_SANTA_VERSION`, which set the minimum before, is ignored: a function that still has it logs one line at start saying so (see [upgrade step 32](deploy.md#32-editable-minimum-santa-version)).
- The CLI and rudolph-web validate and write the value through the same package, `pkg/model/fleethealth`, whose `DefaultMinSantaVersion` is the one default. See [`rudolph fleet min-santa-version`](cli.md#fleet-min-santa-version).

## Lockdown readiness

A Mac in Lockdown blocks every binary no rule allows. Before moving a Mac there, you want to know what it would block. In monitor mode Santa uploads an event for every unknown binary a Mac runs, and from those Rudolph keeps one marker per Mac and binary.

### What is counted

- **The marker.** For every binary with a stored `BLOCK_UNKNOWN` or `ALLOW_UNKNOWN` execution in an upload, eventupload writes `UnknownSeen#<machine ID>` / `<file SHA-256>` with `LastDay`, the UTC date of the upload. The date is Rudolph's, not the Mac's, so a Mac cannot backdate a binary out of the window; a Mac that was offline and uploads old executions has them counted as recent, which errs towards "not ready".
- **`Unknown30`, `Unknown7`**: the Mac's markers whose `LastDay` is within the last 30 and 7 days, today included.
- **`Pending30`, `Pending7`**: those of them no global rule covers now, by SHA-256, signing ID or team ID. Without this a Mac would stay unready for 30 days after you wrote the rule. Certificate and CDHash rules cannot be matched (a marker holds neither), and group and machine rules are not counted, so pending can overstate but not understate.
- **`Capped`**: the job reads at most 1,001 of a Mac's markers, with one `Query`. A Mac with more is shown as "1,000+" and is never ready, whatever was read.

### Buckets

Each Mac is in exactly one bucket, the first that matches:

| Bucket | Name | A Mac is in it when |
|---|---|---|
| `4` | Locked | Rudolph already intends `LOCKDOWN` or `STANDALONE` for it. Its markers are not read, so its row has no unknown counts. |
| `3` | Unmeasured | nothing pending would mean nothing, for the reason in `Why` (below). |
| `2` | Many | 10 or more pending in 30 days, or capped. |
| `1` | Few | 1 to 9 pending in 30 days. |
| `0` | Ready | none pending in 30 days. |

A Mac that uploads nothing looks exactly like a Mac that ran nothing unknown. `Unmeasured` holds every case in which that silence is known to be possible:

| `Why` | The Mac |
|---|---|
| `stale` | is in the `Stale` list |
| `incomplete` | is in the `Incomplete` list |
| `upload-off` | has `DisableUnknownEventUpload` true, in its own configuration or inherited from the global one |
| `no-rules` | reports no rules at all: such a Mac gets a clean sync on every sync, and Santa uploads no events during one |
| `observed` | was first seen fewer than 30 days ago (`ObservedDays`), or has no first-seen date yet |

One cause is not visible to Rudolph: a configuration profile that sets `DisableUnknownEventUpload` on the Mac while Rudolph sends nothing. Storing `rudolph config update --global --disable-unknown-event-upload=false` closes it, as a value from the sync server beats the profile.

Each Mac has one row in the fleet's list and one in the list of each of its effective groups: the registered groups it is assigned to or, where the group accepts reported membership, reports. A Mac in none is under `rudolph:ungrouped`. Within a bucket, rows sort by fewest pending first. The pointer's `Readiness` holds the five counts and their total for `All`, every registered group and `rudolph:ungrouped`.

### Warm-up

There is no backfill. Readiness starts counting on the day the release is deployed:

| Figure | Accurate |
|---|---|
| Health lists and counts | after the job's first run |
| 7-day unknown and pending counts | 7 days after the deploy |
| 30-day counts and the buckets | 30 days after the deploy |

- `FirstSeenDay` on a Mac's sync state is the UTC date of its first sync on record. A Mac that was syncing before the deploy is first seen on the day of its first sync after it.
- Until a Mac has been observed for 30 days it is `Unmeasured` with `Why` `observed`, however few binaries it has run. So for the first 30 days after the deploy every Mac that is not locked is `Unmeasured`, and nothing can be called ready early.
- A Mac whose sync state expired (no sync for 90 days) starts again from the day it returns.

### What a holder of the sync token can do

The sync token is shared by the fleet, and whoever holds it can already write sensor data and events for any machine ID.

- They can make any Mac look unhealthy or unready, by forging its preflight or uploading invented hashes. They cannot make a Mac look ready by adding data: counts only go up, and a capped Mac is never ready.
- They can make a Mac they control look ready by not uploading. The `Unmeasured` gates cover the known ways; watch a Mac's events for its first days in Lockdown.
- Invented machine IDs inflate `Macs` and cost the job about four read units and three write units each a run, until their sensor data expires after 90 days. Above `FLEET_MAX_MACS` (250,000) the run fails without writing anything and the job's alarm fires.
- A Mac's markers cost the job nothing beyond the 1,001 it reads.

## How a run works

1. It reads the stale threshold, the group registry and the global configuration (three `GetItem` on the store table).
2. It lists every Mac by paging the store table's `DataType_MachineID` index for sensor data rows. It never scans. Above `FLEET_MAX_MACS` it fails before writing anything.
3. For each Mac, 32 at a time, it reads the Mac's configuration, sensor data, groups and sync state with one `Query` of the Mac's partition and, unless the Mac is Locked, its unknown markers with one `Query` limited to 1,001 rows. It then writes the Mac's rows with `BatchWriteItem`.
4. The global rules' sort keys are read once, when the first Mac has an unknown binary to check. A fleet that is all in Lockdown never reads them.
5. After the walk it reads the newest row of the rules feed (one `Query` of `RulesFeed`, newest first, limited to one row), settles the rules hash outliers among the Macs that synced since, and writes their rows.
6. It writes the pointer, on the condition that the stored pointer is absent or names an older snapshot. Snapshot IDs sort by time.

- **A reader never sees half a snapshot**: every row is written before the pointer.
- **A failed run changes nothing**: the pointer stays on the previous snapshot, and the function returns the error, which Lambda counts in `Errors`. The next day's run starts again from scratch, so failed runs are not retried.
- **Overlapping runs** (a schedule delivered twice, or a run by hand) are harmless: the one that started earlier cannot move the pointer back.
- **Rows expire 3 days after they are written.** A reader paging an older snapshot keeps finding its rows after a newer run. If the job stops, the pointer outlives its rows and the web view says the lists have not been computed since `GeneratedAt`.
- **Rows enter no index.** They have no `TS`, `Fleet` or `Decision`, so each costs one write unit.
- **Writes are paced** at 1,000 rows a second, so a large fleet's rows are not throttled in the one partition every Mac has a row in, and the job does not take the events table's capacity from eventupload.

The job's code shares Rudolph's own logic rather than restating it: the intended mode comes from `pkg/model/machineconfiguration` (`IntendedFromStored`, `IntendedClientMode`) and `pkg/model/groups` (`MachineClientMode`, `Effective`), the same functions preflight and rule download use; marker fields follow `pkg/counterfields`; rule keys come from `rules.CoveringSortKeys`, which the ranker uses too; and the snapshot mechanics (`internal/snapshot`) are the ranker's.

### Permissions

The job has its own role, `PREFIX_rudolph_fleet_role`, with every statement limited by partition key and no `Scan`:

| Resource | Actions | Partitions |
|---|---|---|
| store table index `DataType_MachineID` | `Query` | `SensorData` |
| store table | `Query` | `Machine#*`, `GlobalRules`, `RulesFeed` |
| store table | `GetItem` | `GroupRegistry`, `GlobalConfig`, `FleetHealth` |
| events table | `Query` | `UnknownSeen#*` |
| events table | `PutItem`, `BatchWriteItem` | `FleetSnapshot#*`, `FleetPointer` |
| its own log group | `logs:CreateLogStream`, `logs:PutLogEvents` | |

It has no KMS statement: the store key's policy lets the account's principals use the key through DynamoDB, and names this role among Rudolph's Lambdas when `store_kms_key_user_arns` narrows it. The ranker's role is unchanged, and eventupload gains only `UnknownSeen#*`.

## Schedule and alarm

An EventBridge Scheduler schedule, `PREFIX_rudolph_fleet`, runs the job at `fleet_schedule_expression` in `fleet_schedule_timezone`: once a day in UTC by default. See [Fleet job schedule](deploy.md#fleet-job-schedule).

The job has one alarm, `PREFIX_rudolph_fleet_job`: no successful run for `fleet_alarm_days` days in a row (default 3, from 1 to 7).

- It is built from Lambda's own `Invocations` and `Errors` for the function, summed per UTC day: a day counts as successful when invocations minus errors is at least 1. Failures, timeouts and running out of memory all count as errors, and a day without any invocation counts as a day without success.
- It notifies `alarm_sns_topic_arn` when it fires and when it recovers, and **only exists when that topic is set**, unlike Rudolph's other alarms.
- There are no alarms on stale or incomplete Macs, no metric filters and no custom metrics. The job is left out of `lambda_errors` and `lambda_timeouts`, so one failed night is silent: the lists are a day older and the web view shows their age.
- It counts UTC days and assumes at least one run a day. With a schedule that skips days, such as weekdays only, set `fleet_alarm_days` above the longest gap or it fires every weekend.

### Running it by hand

```sh
aws lambda invoke --region "$REGION" --function-name "${PREFIX}_rudolph_fleet" /tmp/fleet-out.json
aws logs tail "/aws/lambda/${PREFIX}_rudolph_fleet" --since 15m --region "$REGION"
```

A successful run logs one line, for example:

```
fleet: macs=1000 rules=1200 attention=41 stale=30 incomplete=2 ready=310 locked=120 stale_days=10 holiday=false min_santa_version=2026.9 seconds=4.2
```

## Limits

- **Time.** The function has the Lambda maximum of 900 seconds. A run is about two reads and one write call a Mac, 32 Macs at a time, with rows paced at 1,000 a second: a few seconds for 1,000 Macs, about 5 minutes for 100,000. `FLEET_MAX_MACS` (default 250,000) is where a run stops fitting; past it the walk would have to be split across invocations, which is not built.
- **Memory.** 256 MB. A run holds every machine ID (about 60 bytes each), the global rules' sort keys (about 10 MB for 80,000 rules) and, for the hash check, a sample of about 200 bytes for each Mac that completed a sync in the last day. Rows are written as the walk goes and never held. Compare `macs=` and `rules=` in the log line with `Max Memory Used` on the `REPORT` line, and raise `memory_size` in `fleet.tf` if that nears the limit. This has not been measured at 100,000 Macs.
- **A Mac's partition is read whole.** A Mac that reports hundreds of long directory groups has a large reported-groups item, which costs that Mac up to 8 read units instead of half of one.
- **Group and machine rules are not counted** towards pending, and neither are certificate or CDHash rules, so a fleet that allows software mostly through those will look less ready than it is.

## Cost

About US$0.75 a month for 1,000 Macs in monitor mode and about US$0.30 once most are in Lockdown, at us-east-1 list prices; US$0.20 of each is the alarm, which needs an SNS topic. The breakdown and other fleet sizes are in [costs.md](costs.md#fleet-health-and-lockdown-readiness).

- The markers are most of it in monitor mode: one write unit per unknown binary per upload.
- The job costs about 3.5 read units and 2.5 write units a Mac a run in monitor mode, and about half a read unit and the same writes for a Mac Rudolph intends to lock, because its markers are not read. Both the marker writes and the job's reads fall as Macs move to Lockdown.
- The schedule is the lever for the job's share: hourly runs would cost 24 times as much for lists that change slowly.
