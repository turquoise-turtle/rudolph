# The `rudolph` command line

`rudolph` reads and writes the sync server's DynamoDB table directly: rules, configurations, device groups and what Macs report. It never talks to the sync API. Run `rudolph <command> --help` for every flag; this page is the overview.

Build it with `make build`; the binary is `./rudolph`.

## Choosing the deployment

Every command except `info` needs the table and region of a deployment (`info` uses one when it is configured):

- `--env <environment>` (or `--ENV`, or the `ENV` environment variable) names the deployment. Its configuration is read from the first of these files that exists, relative to the working directory: `deployments/environments/<env>/config.auto.tfvars.json` (where `scripts/new_env.sh` creates it, so run `rudolph` from the repo root), `terraform/deployments/<env>/config.auto.tfvars.json`, `terraform/environments/<env>/config.auto.tfvars.json`, `~/.rudolph-cli/<env>.json`, `./<env>.json`. The file needs `region` and `prefix`; the table is `<prefix>_rudolph_store`, or `dynamodb_table_name` when the file sets it (only the CLI reads that key: the deployed Lambdas always use `<prefix>_rudolph_store`, so set it only to point the CLI at another table on purpose). A file with an empty `region` is an error.
- `--region`, `--dynamodb_table` and `--prefix` override what the environment file says. A typed `--prefix` also sets the table to `<prefix>_rudolph_store` (ignoring the file's `dynamodb_table_name`) unless `--dynamodb_table` is typed too.
- Without an environment file, `--region` with `--dynamodb_table` (or with `--prefix`) is enough, and the `REGION` and `DYNAMODB_NAME` environment variables (the names the Lambdas use) stand in for them. This is the way in for CI and for DynamoDB Local.

AWS credentials come from the usual AWS SDK sources (`AWS_PROFILE`, environment variables, SSO).

## Conventions

- **Confirmation.** The commands that write rules or configurations (`rule`, `rules import`, `config set|update`), `group delete`, `machine forget` and `repair --delete` show what they are about to write and ask `Apply changes?`. The other `group` commands and `rules backfill-policy-index` write without asking. `yes` or `ok`, in any case, applies it. Any other answer, or no answer (empty stdin), changes nothing and exits non-zero, so scripts can tell a declined change from an applied one. `--yes` (`-y`, on `rule`, `rules import`, `config`, `group`, `machine` and `repair`) applies without asking.
- **Dry run.** `--dry-run` (on `rule`, `rules`, `config`, `group`, `machine` and `repair`) makes a command that writes do all its reading and checking as usual, then print what it would write (the rule, target, policy and expiry; the configuration changes; the group or membership change; what `machine forget` would delete; each rule an import would create, replace, remove or leave unchanged), followed by `Dry run: the changes shown are not written.`, and exit 0 without asking or writing. It wins over `--yes`, so a pasted command (such as one copied from rudolph-web) can be checked by adding `--dry-run`. Commands that only read accept it and ignore it.
- **Targets.** Commands that write rules (`rule allow|deny|silent|silent-gui|silent-tty|compiler|cel|update|remove`) need exactly one of `--global` (`-g`, every Mac), `--machine <machine-id>` (`-m`, one Mac), `--group <group>` or `--ungrouped` (a [device group](groups.md); not for `rule update`). `rules` and `config get|set|update` take at most one of `--global` and `--machine`; without either they apply to the Mac running the command, whose machine ID is read from `/Library/Preferences/com.northpolesec.santa.machine-mapping.plist`, or, when that file does not exist, from `/Library/Preferences/com.google.santa.machine-mapping.plist`, which Macs set up from the older example profile have.
- **Machine IDs** are UUIDs in the 8-4-4-4-12 form, typed in any case; see [rules.md](rules.md#global-and-machine-specific-rules) for how the stored spelling is found.
- **Shorthands** mean the same flag everywhere: `-f` file path, `-i` identifier, `-t` rule type, `-p` policy, `-m` machine, `-g` global, `-c` client mode, `-y` yes.
- Rule types: `binary` (`bin`), `certificate` (`cert`), `teamid`, `signingid`, `cdhash`. Policies: `allowlist`, `blocklist`, `silent_blocklist`, `silent_gui_blocklist`, `silent_tty_blocklist`, `cel`, `remove`.
- Commands that write rules check the local clock against DynamoDB's first (see [rules.md](rules.md)).
- **Times.** Rule expiries, temporary client mode ends and sync times are printed in local time first, then UTC in brackets, then relative to now, e.g. `Expires: 2026-11-09 20:21 JST (2026-11-09 11:21:42 UTC), in 2h 0m`; if your time zone is UTC, the time is shown once, e.g. `Expires: 2026-11-09 11:21:42 UTC, in 2h 0m`; an end that has passed shows as `expired 5m ago`, and a machine rule without an expiry as `never`. Local time is your computer's time zone; to use another, set `TZ` for the command, e.g. `TZ=America/New_York rudolph rule get …`. Audit summaries are stored, so they keep plain UTC.
- **Audit trail.** Every change a command writes is also recorded in the [audit trail](#the-audit-trail), after the write succeeds. A dry run or a declined prompt records nothing.

## Rules

```sh
rudolph rule <verb> (-f <file-path>|-i <identifier>) -t <rule-type> (-m <machine-id>|--global|--group <group>|--ungrouped) [--expires <duration-or-time>|--permanent]
```

| Verb | Policy |
|---|---|
| `allow` | `ALLOWLIST` |
| `deny` (`block`) | `BLOCKLIST` |
| `silent` | `SILENT_BLOCKLIST` |
| `silent-gui` | `SILENT_GUI_BLOCKLIST` |
| `silent-tty` | `SILENT_TTY_BLOCKLIST` |
| `compiler` (`allow-compiler`) | `ALLOWLIST_COMPILER` |
| `cel -e <expression>` | `CEL` |

- Exactly one of `-f`/`--filepath` and `-i`/`--identifier`. `-f` runs `santactl fileinfo` on the file (so needs Santa on the Mac running the command) and takes the SHA-256, certificate SHA-256, team ID or signing ID from it; CDHash rules need `-i`.
- Machine rules expire after 24 hours unless `--expires <when>` (a duration such as `48h`, or an RFC 3339 time) or `--permanent` is given; global and group rules never expire.
- `--custom-msg`, `--custom-url`, `--event-detail-button-label` and `--notification-app-name` set the optional fields Santa shows (see [rules.md](rules.md)).
- On success the command prints what was stored, e.g. `Added BLOCKLIST TEAMID rule EQHXZ8M8AV for every machine`.

### `rule update`

```sh
rudolph rule update (-f <file-path>|-i <identifier>) -t <rule-type> (-m <machine-id>|--global) [-p <policy>] [--expires <when>|--permanent] [--custom-msg <text>] [--custom-url <url>] [--event-detail-button-label <text>] [--notification-app-name <name>] [--collect-allowed-events=true|false]
```

Changes an existing rule; only what is given changes. Every flag is optional, but at least one change is needed, and an update that would leave the rule as it is changes nothing and says so (exit 0, so a command copied twice is harmless). A machine rule that is being removed (after `rule remove`, until the Mac's sync deletes it) cannot be updated: create it again instead. `-p remove` is refused (a `REMOVE` policy on a rule row never deletes it: use `rule remove`), and so is `-p cel` (create CEL rules with `rule cel`). An empty message or URL (`--custom-msg ""`) clears it. `--expires`/`--permanent` apply to machine rules only. The prompt shows the rule as stored, including its description, and as it will be. Group rules are changed by writing them again.

### `rule remove`

```sh
rudolph rule remove (-t <rule-type> -i <identifier>|<rule-name>) (-m <machine-id>|--global|--group <group>|--ungrouped)
```

`<rule-name>` is the rule's sort key as `rudolph rules` prints it: `Binary#`, `Cert#`, `TeamID#`, `SigningID#` or `CDHash#` followed by the identifier, e.g. `TeamID#EQHXZ8M8AV`. The command names the rule it removed, or the rule (and machine) it could not find.

### `rule get`

```sh
rudolph rule get (-f <file-path>|-i <identifier>) -t <rule-type> (-m <machine-id>|--global)
```

Shows one global or machine rule as stored: policy, description, expiry, messages, whether its allowed executions are collected, and whether a machine rule is being removed. A group's rules are shown by `rudolph group get <group>`.

### `rules`

```sh
rudolph rules [-m <machine-id>|--global] [--type <rule-type>] [--policy <policy>] [--limit <n>]
```

Lists every global rule, or every rule of a machine, with its description, expiry (machine rules) and whether its allowed executions are collected. `--type` and `--policy` filter; `--limit` stops after that many (default: all).

### `rules export` and `rules import`

```sh
rudolph rules export -f <file-path> [--fileformat csv|json]
rudolph rules import -f <file-path> [--fileformat csv|json] [--workers <n>]
```

`export` writes the **global** rules only (not machine or group rules) as CSV or JSON: `--fileformat`, or else JSON for a `.json` file and CSV otherwise; any other format is refused. `import` creates or replaces global rules from such a file, after showing how many rules it creates, replaces or removes and asking for confirmation (none with `--yes`); the format is `--fileformat`, or else the file's extension (`.csv`, `.json`). `--workers` (default 10) is how many rules are written at once. The columns are described in [rules.md](rules.md#importing-or-exporting-rules). `--filename`, the flag's former name, still works.

### `rules backfill-policy-index`

Run once after upgrading; see [rules.md](rules.md#global-rules-and-the-policy-index).

## Configuration

```sh
rudolph config get [-m <machine-id>|--global]
rudolph config set [-m <machine-id>|--global] -c <client-mode> [<setting flags>]
rudolph config update [-m <machine-id>|--global] [-c <client-mode>] [<setting flags>] [--clean-sync] [--inherit <setting>]...
rudolph config update -m <machine-id> -c <mode> (--for <duration>|--until <time>)
rudolph config update -m <machine-id> --end-temporary-mode
```

- `get` shows the global configuration, or the one a machine gets; for a machine, each setting is marked `(machine)` when the machine gets it from its own configuration and `(inherited)` when it follows the global one (a stored value that cannot be read is inherited, except remount flags, which are then not sent at all).
- `set` replaces the whole configuration; `-c` (`monitor`, `lockdown` or `standalone`) is required. Its old shorthands `-s` (`--batch-size`) and `-u` (`--upload-logs`) still work but are deprecated; `-f` no longer means `--full-sync-interval`.
- `update` changes only the settings given. Setting flags (both commands): `--allowed-paths`/`-a`, `--blocked-paths`/`-b` (repeatable regexes), `--batch-size`, `--bundles`, `--transitive-rules`, `--full-sync-interval`, `--upload-logs`, `--enable-all-event-upload`, `--disable-unknown-event-upload`, `--override-file-access-action`, `--block-usb-mount`, `--remount-usb-mode`, `--event-detail-url`, `--event-detail-text`.
- `--inherit <setting>` makes a machine follow the global value of a setting again; `--clean-sync` requests one clean sync of a machine; `--for`/`--until`/`--end-temporary-mode` manage a temporary client mode.
- A global client mode that blocks unknown executions needs `--confirm-global-lockdown` as well.

What each setting does, and how machine configurations inherit from the global one, is in [sync.md](sync.md) and [lockdown.md](lockdown.md).

## Device groups

```sh
rudolph group create <group> [--description <text>] [--no-reported]
rudolph group update <group> [--description <text>] [--no-reported|--accept-reported]
rudolph group delete <group> [--force]
rudolph group list
rudolph group get <group>
rudolph group add -m <machine-id> <group>
rudolph group remove -m <machine-id> <group>
rudolph group show -m <machine-id>
```

`delete` asks for confirmation. See [groups.md](groups.md).

## Finding machines

```sh
rudolph machines [--limit <n>]
rudolph machine show -m <machine-id>
rudolph machine forget -m <machine-id> [--yes|--dry-run]
rudolph lookup machine-ids [<prefix>] [--limit <n>]
rudolph lookup serial-number <serial-number> [--limit <n>]
rudolph info
```

`machines` lists the Macs that have synced, one per line ordered by serial number: machine ID, hostname, serial number, Santa version and the UTC day of the last sync. It reads the `DataType_SerialKey` index, as rudolph-web's machine list does (one Query, never a Scan; see [Sensor data row](sync.md#sensor-data-row)), so a Mac that reports no serial number is not listed (find it with `lookup machine-ids`). `--limit` defaults to 50 and must be at least 1; when there are more Macs, a last line says to raise it. With none, it prints `no machine found`.

`machine show` prints what Rudolph knows about one Mac:

- what it reported at its last preflight: hostname, serial number, primary user, model, macOS version and build, Santa version, the client mode it was in, and the number of rules it holds, in total and by type;
- when it last synced (local time, then UTC in brackets, then how long ago), when a sync last completed (postflight) and when its last clean sync started;
- a legacy client note when its Santa is older than 2026.8, the oldest release Rudolph fully supports (see [Supported Santa releases](sync.md#supported-santa-releases));
- the configuration Rudolph sends it: the client mode in effect now and its temporary client mode with its end (local time, then UTC in brackets, then the time remaining or how long since it ended), if any, and whether a clean sync is requested (`config get --machine` shows every setting);
- its assigned, reported and effective groups, as `group show` prints them.

The machine ID is found in any case, as for `--machine` elsewhere (see [Conventions](#conventions)). A Mac that never synced, or whose data expired (90 days after its last sync), is an error: `no machine with ID … has synced`.

`lookup` prints one machine ID per line (`no machine found` when there is none) from the data Macs send when they sync; `--limit` defaults to 10 and 5.

`info` prints the machine ID of the Mac running it. When a deployment is configured (`--env` or `ENV`, or `--region` with `--dynamodb_table`), it then prints the same block as `machine show` for this Mac: the configuration Rudolph sends it and, once it has synced, what it reported. Without a deployment it prints only the machine ID and a hint to set `ENV`, and exits 0; with an `ENV` whose configuration cannot be read, it warns and does the same. Once a deployment is configured, a failed read of it (for example expired AWS credentials) is an error: `info` prints the machine ID, then the error, and exits 1.

## Forgetting a retired Mac

```sh
rudolph machine forget -m <machine-id> [--yes|--dry-run]
```

Deletes everything Rudolph keeps in the store table for one Mac: its sensor data (what it reported at its last sync) and sync state, its configuration, its group membership item, its machine rules and their delivery tracking (every item of the `Machine#<id>`, `MachineRules#<id>` and `MachineRuleDelivery#<id>` partitions), and its entries in the index of rules whose allowed events are collected (see [events-table.md](events-table.md)). The machine ID is found in any case, as for `machine show`.

It first prints what it will delete, with the Mac's hostname and serial number and a count of each kind of item, then asks `Apply changes?` (`--yes` skips the question; `--dry-run` deletes nothing). It reads each partition with a paged Query, never a Scan, and deletes with batch writes. A Mac for which nothing is stored is an error. A run that fails part-way can be run again; it deletes what is left.

- **Groups.** The groups registered with `group create` stay, with their rules; only the Mac's membership goes. Its assigned groups are removed as `group remove` does, so the membership version that readers of group membership watch changes.
- **Global and group rules** are not changed.
- **Forget a Mac that has stopped syncing.** Remove its Santa profile (or retire it) first: a sync already under way while `forget` runs can write its sensor data, sync state or delivery rows again straight after the delete, and the Mac then reappears partly forgotten. Running `forget` again once it has stopped syncing cleans up.
- **Events** in the events table are not deleted: they expire on their own (see [Events from the test binaries](smoke-test.md#135-events-from-the-test-binaries-7-8) for when), and rudolph-web keeps showing them until then.
- **If the Mac syncs again**, it starts afresh, as a Mac Rudolph has never seen: no configuration of its own (it gets the global one), no machine rules and no assigned groups, and its first sync is a clean sync.

## The audit trail

```sh
rudolph audit [--since <duration-or-time>] [--limit <n>]
```

Every write the CLI makes also records an audit entry in the store table: the time, the actor, the action, the target and a summary. The CLI's actor is `cli:<OS user>@<host name>` (the user and host name of the computer running it; `$USER` or `unknown` when they cannot be read); rudolph-web will record its own changes with the actor `web:<Entra user>`. Entries are never overwritten and never expire. The layout is in [architecture.md](architecture.md#dynamodb).

| Command | Action | Target |
|---|---|---|
| `rule allow\|deny\|silent\|silent-gui\|silent-tty\|compiler\|cel` | `rule.add` | `global`, `machine:<machine id>`, `group:<group>` or `ungrouped` |
| `rule update` | `rule.update` | `global` or `machine:<machine id>` |
| `rule remove` | `rule.remove` | `global`, `machine:<machine id>`, `group:<group>` or `ungrouped` |
| `rules import` | `rules.import` (one entry for the import, with the counts) | `global` |
| `rules backfill-policy-index` | `rules.backfill-policy-index` | `global` |
| `config set` | `config.set` | `global` or `machine:<machine id>` |
| `config update` with `--for` or `--until` | `config.temporary-mode` | `machine:<machine id>` |
| `config update --end-temporary-mode` | `config.end-temporary-mode` | `machine:<machine id>` |
| any other `config update` | `config.update` | `global` or `machine:<machine id>` |
| `group create\|update\|delete` | `group.create`, `group.update`, `group.delete` | `group:<group>` |
| `group add\|remove` | `group.add`, `group.remove` | `machine:<machine id>` (the summary names the group) |
| `machine forget` | `machine.forget` | `machine:<machine id>` |
| `repair --delete` | `repair.delete` | `global` |

Machine IDs are the stored spelling (see [rules.md](rules.md#global-and-machine-specific-rules)). Summaries are for people: a rule's type, identifier and policy (`BINARY rule <sha256> BLOCKLIST, expires ...`), a temporary mode's client mode and end, the configuration settings changed, and so on.

The entry is recorded after the change is written, never instead of it. If recording it fails (for example because the credentials lack `dynamodb:PutItem` on the audit partitions; see [deploy.md](deploy.md#cli-permissions)), the command prints `[WARNING] the change was made but not audited ...` on stderr and exits non-zero. The change is not rolled back: record what was done by other means, fix the permission and carry on. A command that writes nothing records nothing: a dry run, a declined prompt, a `rule update` that finds the rule already as given, a `repair --delete` that finds no legacy items, a `rules backfill-policy-index` that updates no rule, or a `rules import` in which every rule was unchanged, skipped or failed. A command that changed something and then failed (an import, backfill or repair that stops part way, a `machine forget` that removed the group assignments and then could not delete the items, a `rule update` that changed the rule and then could not change event collection, a `group delete --force` that deleted rules and then not the group) records an entry whose summary ends with `(failed part-way: <error>)`, and still exits non-zero. `rules import` summarises as `<n> global rules written from <file> (<unchanged>, <skipped>, <failed>)`.

Configuration summaries list the settings changed (`ClientMode=LOCKDOWN, BatchSize=50`). URLs (`UploadLogsURL`, `EventDetailURL`) are cut to scheme, host and path, without user information, query string or fragment, since entries are kept forever.

`rudolph audit` lists the entries, newest first, with the time (local, then UTC), the actor, action, target and summary:

```
LOCAL TIME               TIME (UTC)            ACTOR                     ACTION                 TARGET                                        SUMMARY
2026-11-07 13:12:09 JST  2026-11-07T04:12:09Z  cli:admin@admin-mac       config.temporary-mode  machine:AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA  temporary client mode LOCKDOWN until 2026-11-07T05:12:09Z
2026-11-07 13:11:52 JST  2026-11-07T04:11:52Z  cli:admin@admin-mac       rule.add               global                                        BINARY rule abab...abab BLOCKLIST
```

- `--since` is how far back to look: a duration back from now (`7d`, `36h`, `90m`) or an RFC 3339 time (`2026-11-01T00:00:00+11:00`). Default `7d`. A time in the future is an error, and so is one more than 400 days back (the look-back limit, `audit.MaxLookback`): the error gives the limit in days.
- `--limit` is the most entries to show. Default `100`; when the limit is reached it says so.
- With no entries in that window it prints the start of the window, local time first: `No audit entries since 2026-11-06 13:12 JST (2026-11-06 04:12:09 UTC).`

It reads one partition per day back to `--since` with Query, never Scan, so a long `--since` costs one small read per day; the 400-day limit caps that at about 400 reads.

## Maintenance

```sh
rudolph repair [--delete [--yes|--dry-run]]
```

Scans the whole table for items written by early versions under `MachineInfo#` and `MachineConfig#`, which nothing reads any more. Without `--delete` it only lists them; with it, it deletes them after a confirmation (with `--dry-run` as well, it only lists them). A full scan reads every item, so it costs as much as the table is large.
