# The `rudolph` command line

`rudolph` reads and writes the sync server's DynamoDB table directly: rules, configurations, device groups and what Macs report. It never talks to the sync API. Run `rudolph <command> --help` for every flag; this page is the overview.

Build it with `make build`; the binary is `./rudolph`.

## Choosing the deployment

Every command except `info` needs the table and region of a deployment:

- `--env <environment>` (or `--ENV`, or the `ENV` environment variable) names the deployment. Its configuration is read from the first of these files that exists, relative to the working directory: `deployments/environments/<env>/config.auto.tfvars.json` (where `scripts/new_env.sh` creates it, so run `rudolph` from the repo root), `terraform/deployments/<env>/config.auto.tfvars.json`, `terraform/environments/<env>/config.auto.tfvars.json`, `~/.rudolph-cli/<env>.json`, `./<env>.json`. The file needs `region` and `prefix`; the table is `<prefix>_rudolph_store`, or `dynamodb_table_name` when the file sets it. A file with an empty `region` is an error.
- `--region`, `--dynamodb_table` and `--prefix` override what the environment file says.
- Without an environment file, `--region` and `--dynamodb_table` are enough, and the `REGION` and `DYNAMODB_NAME` environment variables (the names the Lambdas use) stand in for them. This is the way in for CI and for DynamoDB Local.

AWS credentials come from the usual AWS SDK sources (`AWS_PROFILE`, environment variables, SSO).

## Conventions

- **Confirmation.** Every command that writes shows what it is about to write and asks `Apply changes?`. `yes` or `ok`, in any case, applies it. Any other answer, or no answer (empty stdin), changes nothing and exits non-zero, so scripts can tell a declined change from an applied one. `--yes` (`-y`, on `rule`, `config`, `group` and `repair`) applies without asking.
- **Targets.** Commands that write rules (`rule allow|deny|silent|silent-gui|silent-tty|compiler|cel|update|remove`) need exactly one of `--global` (`-g`, every Mac), `--machine <machine-id>` (`-m`, one Mac), `--group <group>` or `--ungrouped` (a [device group](groups.md); not for `rule update`). `rules` and `config get|set|update` take at most one of `--global` and `--machine`; without either they apply to the Mac running the command, whose machine ID is read from `/Library/Preferences/com.google.santa.machine-mapping.plist`.
- **Machine IDs** are UUIDs in the 8-4-4-4-12 form, typed in any case; see [rules.md](rules.md#global-and-machine-specific-rules) for how the stored spelling is found.
- **Shorthands** mean the same flag everywhere: `-f` file path, `-i` identifier, `-t` rule type, `-p` policy, `-m` machine, `-g` global, `-c` client mode, `-y` yes.
- Rule types: `binary` (`bin`), `certificate` (`cert`), `teamid`, `signingid`, `cdhash`. Policies: `allowlist`, `blocklist`, `silent_blocklist`, `silent_gui_blocklist`, `silent_tty_blocklist`, `cel`, `remove`.
- Commands that write rules check the local clock against DynamoDB's first (see [rules.md](rules.md)).

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

Changes an existing rule; only what is given changes. Every flag is optional, but at least one change is needed, and an update that would leave the rule as it is is refused. `-p remove` is refused (a `REMOVE` policy on a rule row never deletes it: use `rule remove`), and so is `-p cel` (create CEL rules with `rule cel`). An empty message or URL (`--custom-msg ""`) clears it. `--expires`/`--permanent` apply to machine rules only. The prompt shows the rule as stored, including its description, and as it will be. Group rules are changed by writing them again.

### `rule remove`

```sh
rudolph rule remove (-t <rule-type> -i <identifier>|<rule-name>) (-m <machine-id>|--global|--group <group>|--ungrouped)
```

`<rule-name>` is the rule's sort key as `rudolph rules` prints it: `Binary#`, `Cert#`, `TeamID#`, `SigningID#` or `CDHash#` followed by the identifier, e.g. `TeamID#EQHXZ8M8AV`. The command names the rule it removed, or the rule (and machine) it could not find.

### `rule get`

```sh
rudolph rule get (-f <file-path>|-i <identifier>) -t <rule-type> (-m <machine-id>|--global)
```

Shows one global or machine rule as stored: policy, description, expiry, messages and whether its allowed executions are collected. A group's rules are shown by `rudolph group get <group>`.

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

`export` writes the **global** rules only (not machine or group rules) as CSV (default) or JSON, and refuses any other format. `import` creates or replaces global rules from such a file; the format is `--fileformat`, or else the file's extension (`.csv`, `.json`). `--workers` (default 10) is how many rules are written at once. The columns are described in [rules.md](rules.md#importing-or-exporting-rules). `--filename`, the flag's former name, still works.

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

- `get` shows the global configuration, or the one a machine gets; for a machine, each setting is marked `(machine)` when the machine's own configuration sets it and `(inherited)` when it follows the global one.
- `set` replaces the whole configuration; `-c` (`monitor`, `lockdown` or `standalone`) is required.
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
rudolph lookup machine-ids [<prefix>] [--limit <n>]
rudolph lookup serial-number <serial-number> [--limit <n>]
rudolph info
```

`lookup` prints one machine ID per line (`no machine found` when there is none) from the data Macs send when they sync; `--limit` defaults to 10 and 5. `info` prints the machine ID of the Mac running it and needs no environment.

## Maintenance

```sh
rudolph repair [--delete [--yes]]
```

Scans the whole table for items written by early versions under `MachineInfo#` and `MachineConfig#`, which nothing reads any more. Without `--delete` it only lists them; with it, it deletes them after a confirmation. A full scan reads every item, so it costs as much as the table is large.
