# Audit: shortcomings vs Santa 2026.8

Protocol source of truth: [`testdata/santa-2026.8/sync_v1.proto`](../../testdata/santa-2026.8/sync_v1.proto)
(northpolesec/protos @ `68fc3ca`, the revision pinned by Santa `2026.8`), plus Santa's client code in the same folder.

How Santa 2026.8 talks to a sync server (from `SNTSyncStage.mm`):
- Requests are protobuf-JSON: fields with a `json_name` use it (`serial_num`), others are lowerCamelCase (`sipStatus`, `rulesHash`, `syncType`); enums are names (`"LOCKDOWN"`); default values are omitted.
- Request bodies are compressed with `Content-Encoding: deflate` unless `SyncClientContentEncoding` is set to `gzip`/`none`.
- Responses are parsed with `ignore_unknown_fields = true`; enum strings must match an enum name/alias exactly; a type mismatch (e.g. string where `repeated string` is expected) fails the whole stage.

### What changed in Santa 2026.8 (released 2026-09-16) that matters to Rudolph
- `sync/v1.proto`: only new `reserved` field numbers (holes shared with sync v2); no wire change for v1.
- **Empty path regexes now clear the setting.** In 2026.7 and earlier an empty `allowed_path_regex` / `blocked_path_regex` became a match-everything regex (per the 2026.8 source comment in `SNTConfigurator.mm`; not reproduced on a Mac). Rudolph always sends both fields, empty when unset, so older clients may have scope-allowed or scope-blocked every unmatched execution. Rudolph should require Santa ≥ 2026.8 fleet-wide. An absent field keeps the previous value, so the server must keep sending `""` to clear a regex (guard: `TestSync_PathRegexesCanBeSetAndCleared`).
- Clients log a warning and keep sending `request_clean_sync` until the server performs a clean sync; a client without synced state now requests CLEAN (not CLEAN_ALL), e.g. after `SyncBaseURL` changes. Rudolph honours requests (guard: `TestL8_…`).
- Truncated responses (200 plus transport error) now fail the stage instead of applying a partial rule set.
- Event uploads can now carry decision `BLOCK_BINARY_MISMATCH` (covered in the P6 test).

Severity: **High** = breaks syncing or enforcement for some hosts / silent wrong policy; **Med** = wrong behaviour with a workaround; **Low** = gap or polish.
Relevance to our needs: **R** rules (global + per-machine), **V** visibility, **$** cost, **Sec** security.

## Protocol (P)
| ID | Sev | Need | Finding | Planned test (see Test map) |
|---|---|---|---|---|
| P1 | High | R | Deflate/gzip request bodies (Santa default) are not decompressed; JSON parse → 400 → sync aborts. API GW REST `minimum_compression_size` unset so the gateway doesn't decompress either. | `pkg/request` decode tests; handler contract tests with deflated fixtures |
| P2 | High | R | `ClientMode` only accepts `MONITOR`/`LOCKDOWN`; `STANDALONE` preflight → 400. | `pkg/types` + preflight contract test |
| P3 | High | R/$ | Clean-sync-by-rule-count ignores `teamid`/`signingid`/`cdhash` counts (upstream #57): such hosts clean sync every run. | `preflight/clean_sync_test.go` table |
| P4 | Med | R | Preflight response never sends `enable_all_event_upload`, `disable_unknown_event_upload`, `override_file_access_action`, `removable_media_policy`, `block_usb_mount`, `remount_usb_mode`, `event_detail_url/text`; `RemountUsbMode` modelled as string (proto: `repeated string`). | preflight contract test decodes response against proto shapes |
| P5 | Med | R | Rule model lacks `CDHASH` type; `CEL`, `SILENT_GUI_BLOCKLIST`, `SILENT_TTY_BLOCKLIST` policies; `custom_url`, `cel_expr`, `event_detail_button_label`, `notification_app_name`. Non-protocol `ALLOWLIST_TRANSITIVE` policy can be sent to clients. | `pkg/types` + ruledownload contract test |
| P6 | Med | V | Event upload drops `audit_events`, `file_access_events`, `entitlement_info`, `cs_flags`, `signing_status`, `secure_signing_time`, `signing_time`, `static_rule`. | eventupload round-trip test |
| P7 | Low | V | Preflight doesn't persist `hostname`, `model_identifier`, `sip_status`, `primary_user_groups`, `rules_hash`, `push_notification_sync`. | preflight sensor-data test |
| P8 | Low | R | Postflight ignores `sync_type` and `rules_hash`. | postflight contract test |

## Logic (L)
| ID | Sev | Need | Finding | Planned test (see Test map) |
|---|---|---|---|---|
| L1 | High | R | Global config update writes `BlockedPathRegex` into `AllowedPathRegex`. | `machineconfiguration` update test |
| L2 | Med | R | CLI `--allowed-paths/--blocked-paths` documented as comma-separated lists but passed raw, so Santa gets one literal regex (upstream #51). | CLI flag → config test |
| L3 | High | R | `config update` for a machine without an existing machine config: `UpdateItem` is conditional on the item existing → fails with `ConditionalCheckFailedException` (confirmed). The copied config keeps `DataType=GlobalConfig`. | DynamoDB Local store test |
| L4 | Med | R | Config setters swallow `PutItem` errors (return `nil`). | `machineconfiguration` set test |
| L5 | High | R | `UpdateGlobalRule` uses an unconditional transactional update: updating a non-existent rule creates a partial row (no RuleType/Identifier), which later breaks clean-sync marshalling: confirmed, every clean sync then fails with HTTP 502 (`unknown rule_type 0`). | DynamoDB Local store test |
| L6 | High | R | Machine rules expire via DynamoDB TTL only: clients never receive a `REMOVE`, and expired rows still being returned before TTL deletion are served. Temporary unblocks become long-lived. | DynamoDB Local + ruledownload test |
| L7 | Med | R | Machine rule removal race: a rule marked `DeleteOnNextSync` after the machine page was served is deleted by that sync's postflight, so the host never gets the `REMOVE`. | postflight/ruledownload sequencing test |
| ~~L8~~ | — | — | **Disproved by test:** preflight does leave `FeedSyncCursor` empty after a requested clean sync, but postflight overwrites it, so no second clean sync happens. Kept as a regression guard. | `TestL8_…` (passes) |
| L9 | Med | R | Postflight sets `FeedSyncCursor = now − 10 min` regardless of when rule download started; a slow sync (>10 min) misses feed rules. | postflight/syncstate test |
| L10 | Low | R | `MachineRulesService.Remove` reports success for non-existent rules (#59). The CLI uses the older `RuleRemovalService`, which still returns "no such rule", so there is no user-visible bug today. Decision: keep the service idempotent; the web view must say "not found". | machinerules service test |
| L11 | Low | R | Global config is cached for 1 h per Lambda instance; config changes can take up to an hour. | config cache test (configurable TTL) |
| L12 | Low | R | Global LOCKDOWN is blocked by a compile-time constant (`allowGlobalLockdown = false`). | config test (explicit opt-in) |
| L13 | Med | V | CLI machine lookup queries GSI `DataType_MachineID`, which Terraform never creates. | Terraform/code consistency test |

## Security (S)
| ID | Sev | Need | Finding | Planned test (see Test map) |
|---|---|---|---|---|
| S1 | High | Sec | Authorizer allows every request; no mTLS. Anyone with the URL can download all rules, spoof preflight/eventupload for any machine ID, or trigger postflight deletes. | authorizer tests (deny by default) + TF mTLS variable |
| S2 | Med | Sec/$ | `ApiRouter` and eventupload log full requests and responses (all rules, user names, paths). | handler logging test (no body in logs) |
| S3 | Low | Sec | Ruledownload skips machine-ID validation that other handlers do. | ruledownload handler test |

## Cost (C)
| ID | Sev | Need | Finding | Planned test (see Test map) |
|---|---|---|---|---|
| C1 | Med | $ | Authorizer result TTL 0: an extra Lambda invocation per request, for an authorizer that allows everything. | TF review |
| C2 | Med | $ | REST API (~$3.50/M) instead of HTTP API (~$1/M); six Lambdas built from one binary. | TF review |
| C3 | Med | $ | Measured: a sync with no changes performs 5 DynamoDB writes (2 PutItem, 3 UpdateItem) and 8 reads (3 GetItem, 5 Query). Writes cost 5× reads. Budget set to ≤3 writes, ≤5 reads, no Scans. | instrumented-client cost guard tests |
| C4 | Med | $ | No CloudWatch log retention anywhere; combined with S2 log volume. | TF review |

## Deployment (D)
| ID | Sev | Need | Finding | Planned test (see Test map) |
|---|---|---|---|---|
| D1 | High | — | Provider pin conflict (`~> 3.76.1` vs `5.15.0`) — `terraform init` fails (#43). | `make tf-check` (init + validate example env against local modules) |
| D2 | High | — | Example env sources the module from GitHub master, unpinned (#56); `new_env.sh`/README out of sync. | `make tf-check` asserts local source |
| D3 | Med | — | `aws_s3_bucket_acl "log-delivery-write"` on new buckets (ACLs disabled by default since 2023) — likely upstream #54. | verify on first dev `apply` |

## Test map

Unit tests run with `go test ./...`; integration tests (DynamoDB Local + terraform) with `make test-integration`.

| IDs | Test | Package |
|---|---|---|
| guards | `TestSync_PathRegexesCanBeSetAndCleared`, `TestSync_GlobalAndMachineSpecificRules`, `TestSync_IncrementalSyncDeliversNewAndRemovedGlobalRules`, `TestSync_MachineSpecificClientMode`, `TestSync_MachineRuleRemovalReachesHost`, `TestL8_…` — pass today; each verified to fail when the behaviour it guards is broken | `internal/handlers` (integration) |
| P1 | `TestP1_DefaultSantaCompressionCanSync`; `TestP1_APIGatewayDeliversCompressedBodiesIntact` | `internal/handlers` (integration); `deployments` |
| P2 | `TestP2_StandaloneModeHostCanSync`; `TestClientMode_Standalone_P2` | `internal/handlers` (integration); `pkg/types` |
| P3 | `TestP3_…`; `Test_determineCleanSyncByRuleCount_AllRuleTypes_P3` | `internal/handlers` (integration); `internal/handlers/preflight` |
| P4 | `TestP4_PreflightSendsConfiguredSantaSettings` | `internal/handlers` (integration) |
| P5 | `TestP5_NewerRuleTypesPoliciesAndFieldsReachHosts`, `TestP5_TransitivePolicyIsNeverSentToHosts`; `TestRuleType_CDHash_P5`, `TestPolicy_NewerPolicies_P5` | `internal/handlers` (integration); `pkg/types` |
| P6 | `TestEventUpload_ForwardsAllSanta2026_8EventData_P6` | `internal/handlers/eventupload` |
| P7, P8 | `TestP7_PreflightRecordsHostInventory`, `TestP8_PostflightRecordsSyncOutcome` | `internal/handlers` (integration) |
| L1, L4, L11 | `TestGlobalConfigUpdate_BlockedPathRegex_L1`, `TestConfigSetters_ReportWriteErrors_L4`, `TestCachedGlobalConfig_PicksUpChangesWithinAMinute_L11` | `pkg/model/machineconfiguration` |
| L2 | `TestCombinePathRegexes_L2` (red = does not compile until `combinePathRegexes` exists) | `internal/cli/config` |
| L3, L5, L6, L7, L9 | `TestL3_…`, `TestL5_…`, `TestL6_…` (×2), `TestL7_…`, `TestL9_…` | `internal/handlers` (integration) |
| L13, D1, D2, D3, C4 | `TestL13_…`, `TestD1_…` (static + `terraform init/validate`), `TestD2_…`, `TestD3_…`, `TestC4_…` | `deployments` |
| S2, C3 | `TestS2_SyncDoesNotLogPayloads`, `TestC3_SteadyStateSyncIsCheapOnDynamoDB` | `internal/handlers` (integration) |
| S3 | `TestRuledownload_RejectsInvalidMachineID_S3` | `internal/handlers/ruledownload` |
| L12 | `TestGlobalLockdown_RequiresConfirmation_L12` (red = does not compile until `ErrGlobalLockdownNotConfirmed`/`ConfirmGlobalLockdown` exist); `TestConfigCommands_HaveGlobalLockdownConfirmation_L12` | `pkg/model/machineconfiguration`; `internal/cli/config` |
| S1, C1 | `TestAuthorizer_*_S1` (red = does not compile until the authorizer takes `APIGatewayCustomAuthorizerRequestTypeRequest`); `TestS1_AuthorizerUsesTokenHeaderAndCaches`, `TestS1_OptionalMutualTLS` | `internal/handlers/authorizer`; `deployments` |
| L10 | reclassified (no CLI bug); covered in web view tests | — |
| C2 | decision: stay on REST API | — |

## Decisions (2026-09-17)
- **Auth (S1):** bearer token via Santa `SyncExtraHeaders`; authorizer checks SHA-256 digests from `SYNC_TOKEN_SHA256` (comma separated for rotation), fails closed; optional mTLS on the custom domain with the default endpoint disabled.
- **Global lockdown (L12):** allowed with explicit confirmation (`ConfirmGlobalLockdown()` / `--confirm-global-lockdown`).
- **Remove non-existent rule (L10):** service stays idempotent; CLI and web view report "not found".
- **API Gateway (C2):** stay on REST API.
