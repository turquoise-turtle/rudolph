# Audit: commits since 2024-01-01

Scope: every non-merge commit on `master` since 2024-01-01 (fork `turquoise-turtle/rudolph` == `airbnb/rudolph` @ `34f5ebb`).
Each diff was read in full. "Verdict" is about whether the change is correct *and* didn't break something else.
Suspicious/broken items map to shortcoming IDs in [shortcomings.md](shortcomings.md), which each get a test.

| Commit | PR | Claimed | Verdict | Findings |
|---|---|---|---|---|
| `5dfb57a` | #41 | Upgrade to 2022–24 schema/sync changes | **Suspicious** | Added `BlockUsbMount`, `RemountUsbMode`, `OverrideFileAccessAction` to the config model and update request, but no setter persists them and `ConstructPreflightResponse` never sends them (P4). `RemountUsbMode` is a `string`; the protocol field is `repeated string` so wiring it through as-is would break Santa's parser (P4). Error wrapping changes made config-update failures surface instead of being logged, exposing L3. |
| `3893706` | #43 | TF Lambda versioning, firehose | **Broken** | Added `santa_api/_providers.tf` pinning `hashicorp/aws = 5.15.0`, while `environments/example/versions.tf` requires `~> 3.76.1`. `terraform init` fails: *no available releases match the given constraints ~> 3.76.1, 5.15.0* (reproduced locally). D1. |
| `7c425c1` | #44 | TeamID / SigningID rule types | OK | DynamoDB encodings for `DataType`/`Policy`/`RuleType` are internally consistent (pre-#44 `DataType` unmarshal used a v1-SDK signature that the v2 SDK never called). Did not add `CDHASH`; keeps non-protocol `ALLOWLIST_TRANSITIVE` policy (P5). |
| `8ecc610` | #48 | Feed rule validation | OK | Validation has real, asserting table tests. SigningID regex accepts `platform:` and 10-char team prefixes. |
| `f5ec879` | #49 | Fix rule feeds/downloads | OK | Real fix: `for _, item := range *items` mutated copies; switched to pointer slices. Touched `determineCleanSyncByRuleCount` but left TeamID/SigningID/CDHash out (P3, upstream #57). |
| `5ab0543` | #53 | Santa > 2024.5 protocol fix | OK | Cursor is now a JSON-encoded *string*, matching `RuleDownloadRequest.cursor` / `RuleDownloadResponse.cursor` (`string`). Drops support for pre-2024.5 object cursors (acceptable: we target 2026.8). |
| `4105ae9` | #56 | KMS admin ARNs; fix symlink issues | **Broken** | Replaced `environments/example/main.tf` symlink with a copy whose module source is `github.com/airbnb/rudolph//deployments/terraform_modules/santa_api` — remote, unpinned. Deploying a fork applies upstream master, not the local code. `scripts/new_env.sh` still symlinks `variables.tf` while example now has `_variables.tf`; README still describes symlinks. D2. |
| `c904b50` | #58 | Fix symlink paths | OK (docs) | README link fix only. Docs remain inaccurate after #56. |
| `c65ad16` | #59 | `ErrRuleNotFound`, graceful Remove | **Suspicious** | `MachineRulesService.Remove`/`RemoveBySortKey` now return `nil` for rules that don't exist, so removing a typo'd identifier reports success. Other error paths in `RemoveMachineRule` still discard the cause (no `%w`). L10. |
| `f191fcc` | #60 | Skip empty Kinesis PutRecords | OK | Correct. Root cause of empty batches is that `audit_events` / `file_access_events`-only uploads are dropped by the parser (P6). |
