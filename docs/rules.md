# Composition of a “Rule”
Santa uses rules stored in the rules database to make decisions. Rudolph stores each rule with the following fields
(the numbers are how Rudolph stores the value in DynamoDB):

Identifier — What to match; its format depends on the rule type

## Type — What the identifier points to
Listed from most to least specific, which is also the order in which Santa applies them.
* 5 = CDHASH → The CDHash of a properly signed binary (40 lowercase hex characters)
* 1 = BINARY → The sha256 hash of a binary file
* 3 = SIGNINGID → The signing ID prefixed with the Team ID, or `platform` for Apple binaries (e.g. `EQHXZ8M8AV:com.google.Chrome`)
* 2 = CERTIFICATE → The sha256 hash of the code-signing certificate leaf (the first certificate or Apple developer certificate as used by Gatekeeper)
* 4 = TEAMID → The 10-character Apple Developer Team ID (e.g. `EQHXZ8M8AV`)

## Policy — What Santa does with a matching binary
* 1 = ALLOWLIST
* 2 = BLOCKLIST
* 3 = SILENT_BLOCKLIST → Block without GUI or TTY notifications
* 4 = REMOVE → Only sent to clients to delete a rule
* 5 = ALLOWLIST_COMPILER → Allow, and when transitive rules are enabled, allow the files this binary writes
* 7 = CEL → Allow or block by evaluating the rule's CEL expression (required)
* 8 = SILENT_GUI_BLOCKLIST → Block without GUI notifications; TTY notifications are still shown
* 9 = SILENT_TTY_BLOCKLIST → Block without TTY notifications; GUI notifications are still shown

`ALLOWLIST_TRANSITIVE` (6) is not a policy of the Santa sync protocol: transitive rules are created locally by Santa for
the files written by `ALLOWLIST_COMPILER` binaries. Rudolph rejects new rules with this policy and never sends rules that
still have it (from older versions of Rudolph) to clients; remove them with `rudolph rule remove`.

## Optional fields
* Custom Message (`custom_msg`) — Displayed to the end user instead of the default message whenever this rule blocks
* Custom URL (`custom_url`) — For block rules, opened from the block dialog instead of `EventDetailURL` (same format values)
* Event Detail Button Label (`event_detail_button_label`) — For block rules, the label of the block dialog's button
* Notification App Name (`notification_app_name`) — Santa notifies the user that this application is now allowed (not during clean syncs)
* CEL Expression (`cel_expr`) — The expression evaluated by `CEL` rules; only allowed on `CEL` rules

Every rule a Mac may hold is sent in its rule downloads, so these fields are bounded when a rule is created (global or
machine-specific, CLI or import); a longer value is refused with an error naming the field. The limits, in bytes of
UTF-8, are well above what Santa's block dialog and notifications can show:

| Field | Limit |
|---|---|
| `custom_msg` | 4096 |
| `custom_url` | 4096 |
| `event_detail_button_label` | 256 |
| `notification_app_name` | 256 |
| `cel_expr` | 16384 |

## Global and machine-specific rules
Every `rudolph rule` command that writes a rule (`allow`, `deny`, `silent`, `silent-gui`, `silent-tty`, `compiler`,
`cel`, `update`, `remove`) requires exactly one of `--global` (every Mac) or `--machine <machine-uuid>` (one Mac), or
`--group <group>` / `--ungrouped` for a device group (all but `update`; see [Device groups](groups.md)).
There is no default target: older versions applied the rule to the Mac running the CLI when both were left out.
`rudolph rule remove` takes the rule as `-t <rule-type> -i <identifier>` or as `<rule-name>`, the sort key `rudolph rules` prints (`Binary#`, `Cert#`, `TeamID#`, `SigningID#` or `CDHash#` and the identifier, e.g. `TeamID#EQHXZ8M8AV`). Every command and flag is listed in the [CLI reference](cli.md).

The machine UUID must be in its canonical form (8-4-4-4-12 hexadecimal digits); every `rudolph` command that takes `--machine` (`rule`, `rules`, `config`) refuses anything else, as the API does. It can be typed in any case. The server keeps a Mac's machine ID exactly as the Mac sends it, which is upper case for the hardware UUID but can be any case when Santa's `MachineID` setting is used, so the CLI looks the ID up in the case typed, in upper case and in lower case, and uses the first of these that a Mac has synced under; failing that, one with a machine configuration; failing that, one with machine rules (older CLIs stored the ID as typed); and otherwise upper case. The lookup is one DynamoDB BatchGetItem request, plus at most three one-item queries when nothing is known about the machine. The CLI warns when no Mac has ever synced under the ID it uses.

Machine-specific rules expire after 24 hours unless created with:
* `--expires <when>` — a duration from now (`48h`, `90m`) or an RFC 3339 time (`2026-12-31T18:00:00Z`); it must be in
  the future.
* `--permanent` — the rule never expires (it is stored without `ExpiresAfter`, the table's TTL attribute).

Both flags are refused on global rules (which never expire) and together.

From the CLI, `rudolph rule allow|deny|silent|silent-gui|silent-tty|compiler` accept `--custom-msg`, `--custom-url`,
`--event-detail-button-label` and `--notification-app-name`, and `rudolph rule cel --expression <expr>` creates CEL rules.
CDHash rules must be given with `--identifier`.

`rudolph rule update` changes only what it is given: the policy (`-p`), the expiry of a machine rule (`--expires`, `--permanent`), the four message and URL flags, and `--collect-allowed-events`. Changing a rule's policy keeps its other fields but clears its CEL expression, so a rule cannot be updated *to* `CEL`: remove it and create it again with `rudolph rule cel`. `-p remove` is refused as well: use `rudolph rule remove`. Without `--expires` or `--permanent`, a machine-specific rule keeps its expiry: a permanent rule stays permanent and an expiring rule still expires when it did. A rule that has already expired cannot be updated, including one that expires while the CLI waits for confirmation (the write is conditional on the rule not having expired); create it again with `--expires` or `--permanent`. `rudolph rule update --identifier` does not need Santa on the computer running the CLI; only `--filepath` runs `santactl fileinfo`.

Removing a machine-specific rule restores
the global rule for the same target, including its fields; so does its expiry. Without a global rule Santa can apply,
the Mac drops the rule (see [rule-pagination](./rule-pagination.md#machine-specific-rules)).

Rules stored by older versions with an identifier today's validation rejects (e.g. an upper-case SHA-256) can still be
removed with `rudolph rule remove`.

Commands that write rules (`rudolph rule ...`, `rudolph rules import`) first compare the local clock with DynamoDB's and
refuse when they differ by more than 2 minutes: rule changes are ordered by the writer's clock, and a change written
with a clock that is far behind can miss Macs that sync incrementally. Fix the clock, or pass `--force` (see
[rule-pagination](./rule-pagination.md#feed-cursor)).

> **Deploy the API Lambda first.** Deploy the Rudolph version that supports them before creating `CDHASH` rules or rules
> with the `CEL`, `SILENT_GUI_BLOCKLIST` or `SILENT_TTY_BLOCKLIST` policies. Older Lambdas cannot read these values, so every
> rule download that reaches such a rule fails; for a global rule that is every host, not just the hosts it matches. For the same reason, do not roll the Lambda
> back to an older version once such rules exist: remove them first, or syncs break until you deploy again.


Whenever a new binary is executed, Santa’s system extension gathers information about the binary (via processes similar to `santactl fileinfo {BINARY}`) and looks up any relevant rules. It combines the rules to determine if a binary/application should be allowed/blocked to execute.

A sample output is provided below:
![Sample santactl fileinfo screenshot](./images/santa-fileinfo-sample-output-1.png)



## Collecting the executions a rule allows

By default the [execution events table](events-table.md) keeps blocked executions and `ALLOW_UNKNOWN`, not executions allowed by a rule. To see what one rule allows for a while, flag it:

```sh
rudolph rule update -t teamid -i EQHXZ8M8AV --global --collect-allowed-events=true
# later
rudolph rule update -t teamid -i EQHXZ8M8AV --global --collect-allowed-events=false
```

For example, Google's team ID (`EQHXZ8M8AV`) is allowed fleet-wide, and you want to block Gemini, which Google signs. Flag the team ID rule: every execution it allows is stored with its signing ID (`EQHXZ8M8AV:com.google.GeminiMacOS` among them), so the web view lists the signing IDs the team ID covers. Block Gemini with a signing ID rule (`rudolph rule deny -t signingid -i EQHXZ8M8AV:com.google.GeminiMacOS --global`), which takes precedence over the team ID rule, then switch collection off again.

- Any binary, certificate, team ID, signing ID or CDHash rule can be flagged, global (`--global`) or machine-specific (`--machine <id>`, to watch one Mac only). An execution is stored when the global rule that allowed it is flagged, or the uploading Mac's own machine rule is.
- `--collect-allowed-events` may be given without `-p`, which then leaves the policy as it is; a policy update keeps the flag. `rudolph rule update` shows it as `CollectAllowedEvents`, and `rudolph rules` marks flagged rules.
- The flag (`CollectAllowedEvents` on the rule row) is never sent to Santa: setting it writes no rules feed row and changes neither what a sync downloads nor the Mac's rules hash.
- Santa uploads executions allowed by a rule only when `EnableAllEventUpload` is on. Turn it on for a few Macs (`rudolph config update --machine <id> --enable-all-event-upload`) or for the fleet (`--global`). The CLI warns when you flag a rule while it is off in the global configuration (it does not check machine configs). With it on, Santa uploads about ten times as many events; only those of flagged rules are stored, so the rest cost Lambda invocations (and the Firehose or Kinesis sink, if one is enabled), not DynamoDB: about US$0.30 a month for 1,000 Macs. See [Costs](events-table.md#costs).
- A warm eventupload Lambda keeps the list of flagged rules for up to 60 seconds, so a change takes up to a minute to apply. Turning collection off leaves the events already stored until they expire (90 days).
- Removing a rule (`rudolph rule remove`, or `REMOVE` in `rudolph rules import`), or creating it again with `rudolph rule ...`, switches collection off for it; rewriting it with `rudolph rules import` keeps it on. An expired machine rule collects nothing and cannot be flagged.

## Global rules and the policy index
Each global rule row (partition `GlobalRules`) also stores `GlobalRulePolicy`, a number equal to its `Policy`. Rudolph sets it when a global rule is created (`rudolph rule ... --global`, `rudolph rules import`) and keeps it in step when the policy is updated. Machine-specific rules and rules feed rows never carry it, so the `GlobalRulePolicy_SK` global secondary index (hash `GlobalRulePolicy`, range `SK`, every attribute projected) holds global rules only, in sort key order. The web view uses it to filter global rules by policy; Rudolph itself does not query it, and Santa never sees the attribute.

Global rules written before this attribute existed do not have it, so they are missing from the index until `rudolph rules backfill-policy-index` is run once (see [Upgrading an existing deployment](deploy.md#6-global-rule-policy-index)). The command adds `GlobalRulePolicy` to every global rule that lacks it, changes nothing else, does not touch the rules feed, prints how many rules it updated, and can safely be run again.

## Importing or Exporting Rules
Rudolph comes with a handy CLI tool. One of the useful commands is to import/export rules to/from a csv or json file.
Files have the columns (or json keys) `identifier`, `type`, `policy`, `description` and, optionally, `custom_msg`,
`custom_url`, `event_detail_button_label`, `notification_app_name` and `cel_expr`; `type` and `policy` use the names above. Export also writes `collect_allowed_events`, for information: import does not read it. Export writes the global rules only; machine and group rules are not exported. The format is CSV unless `--fileformat json` is given; import also takes it from the file's extension. See the [CLI reference](cli.md#rules-export-and-rules-import).

You will first need to build the command line utility:

```
make build
```

Once it is built, it will be available at `./rudolph`.

```
./rudolph --env YOURENV rules export -f /path/to/rules.csv
./rudolph --env YOURENV rules import -f /path/to/rules.csv
```

`--env` (or the `ENV` environment variable) names the deployment; see [Choosing the deployment](cli.md#choosing-the-deployment).

Import skips rules that are already stored with the same policy, custom message, custom URL, CEL expression, button label, notification app name and description (it reads each one first, strongly consistently), so importing an unchanged file again writes nothing and adds no rules feed row for Macs to download.

### Sample or Community Rules
We've provided a directory with ... _some_ sample rules [examples/sample-rules.csv](/examples/sample-rules.csv), but each team is largely going to have to figure this stuff out on their own.
