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

From the CLI, `rudolph rule allow|deny|silent|silent-gui|silent-tty|compiler` accept `--custom-msg`, `--custom-url`,
`--event-detail-button-label` and `--notification-app-name`, and `rudolph rule cel --expression <expr>` creates CEL rules.
CDHash rules must be given with `--identifier`.

Changing a rule's policy (`rudolph rule update`) keeps its other fields but clears its CEL expression, so a rule cannot
be updated *to* `CEL`: remove it and create it again with `rudolph rule cel`. Removing a machine-specific rule restores
the global rule for the same target, including its fields.

> **Deploy the API Lambda first.** Deploy the Rudolph version that supports them before creating `CDHASH` rules or rules
> with the `CEL`, `SILENT_GUI_BLOCKLIST` or `SILENT_TTY_BLOCKLIST` policies. Older Lambdas cannot read these values, so every
> rule download that reaches such a rule fails; for a global rule that is every host, not just the hosts it matches. For the same reason, do not roll the Lambda
> back to an older version once such rules exist: remove them first, or syncs break until you deploy again.


Whenever a new binary is executed, Santa’s system extension gathers information about the binary (via processes similar to `santactl fileinfo {BINARY}`) and looks up any relevant rules. It combines the rules to determine if a binary/application should be allowed/blocked to execute.

A sample output is provided below:
![Sample santactl fileinfo screenshot](./images/santa-fileinfo-sample-output-1.png)



## Importing or Exporting Rules
Rudolph comes with a handy CLI tool. One of the useful commands is to import/export rules to/from a csv or json file.
Files have the columns (or json keys) `identifier`, `type`, `policy`, `description` and, optionally, `custom_msg`,
`custom_url`, `event_detail_button_label`, `notification_app_name` and `cel_expr`; `type` and `policy` use the names above.

You will first need to build the command line utility:

```
make build
```

Once it is built, it will be available at `./rudolph`.

```
export ENV=YOURENV
./rudolph rules export -f /path/to/rules.csv
```

```
export ENV=YOURENV
./rudolph rules import -f /path/to/rules.csv
```

### Sample or Community Rules
We've provided a directory with ... _some_ sample rules [examples/sample-rules.csv](/examples/sample-rules.csv), but each team is largely going to have to figure this stuff out on their own.
