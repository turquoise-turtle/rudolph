# The Sync Process
Santa synching is documented here: https://santa.readthedocs.io/en/latest/introduction/syncing-overview/ 

The sync process requires a SyncBaseURL to be configured in the Configuration Profile. This sync server must be an API server that responds to the following 5 endpoints:

* POST /preflight/{MACHINE_ID}
* POST /eventupload/{MACHINE_ID}
* POST /logupload/{MACHINE_ID} - if enabled, as controlled via MDM profiles
* POST /ruledownload/{MACHINE_ID}
* POST /postflight/{MACHINE_ID}

Note: “MACHINE_ID” is configured in the Configuration Profile.

This flow of API calls allows the Santa sensor and the sync server to synchronize their rules and desired configurations. This process is complex, and is expanded upon below. 

## Synchronization Process
One key point about syncing: All API endpoints are HTTP POST by design: they are not intended to be idempotent. The Rudolph server maintains most of the state in each sync transaction and holds a significant amount of control over what happens during a sync. 

Additionally, API endpoints only write synchronization state back to the DynamoDB and API endpoints have no functionality to write/inject rules back into the DynamoDB table. 

### XSRF - CSRF
** This endpoint is not enabled in the deployed version of Rudolph, as it is not clear how this feature improves security **

If implemented, this API endpoint generates a token which is linked to the client sync session. This token will be sent in the header of each POST request to the sync server and will validate, on the server end, that this matches the current session XSRF token.

A simple API endpoint has been created that will always return status 200. 

Rudolph is a RestAPI for the Santa agent to synchronize rulesets from Rudolph. In this case, clients do not need to authenticate nor are cookies present during this synchronization session and the Santa agent manages states during the sync. XSRF/CSRF is not a vulnerable vector due to the way Santa agents communicate with Rudolph.

### Preflight
#### URL - HTTP POST /preflight/{machine_uuid}
This endpoint allows the Santa Sensor to upload its current configuration and sensor data, and allows the Rudolph server to send down desired configuration as well as instructions on whether to clean sync.
#### Reconfiguring the Sensor
    - The Rudolph server’s preflight response overrides many configuration parameters of the Santa sensor when it is received. It can change:
    - The sensor’s MODE (Monitor/lockdown)
    - The number of logs/events uploaded in each POST request
    - Regex-based DENY and ALLOW rules (`allowed_path_regex`, `blocked_path_regex`)
    - Which events are uploaded (`enable_all_event_upload`, `disable_unknown_event_upload`)
    - The File Access Authorization override (`override_file_access_action`: `NONE`, `AUDIT_ONLY` or `DISABLE`)
    - USB mass storage mounting (`block_usb_mount`, `remount_usb_mode`)
    - The button of block notifications (`event_detail_url`, `event_detail_text`)

Which configuration a machine gets:
- A machine without a machine-specific configuration gets the global configuration (or built-in defaults when there is no global configuration).
- A machine-specific configuration overrides only the settings stored on it, and every other setting follows the current global configuration, including later global changes. This holds for every setting: client mode, path regexes, batch size, bundles, transitive rules, full sync interval, upload logs URL, event upload, file access action, USB mount blocking and remount flags, event detail URL/text. Settings configured nowhere are left out of the response, and Santa keeps its current value for them. The USB settings are inherited separately: a machine that only configures `--block-usb-mount` still gets the global remount flags; pass `--remount-usb-mode ""` to configure no remount flags.
- `rudolph config update --machine <uuid>` stores exactly the settings it is given on the machine-specific configuration (creating it if needed). A given setting is stored even when it equals the current global value, which is how a machine is pinned to a setting before a fleet-wide change (e.g. `config update --machine <uuid> -c monitor` exempts it from a later global lockdown, while it keeps following every other global setting).
- `rudolph config update --machine <uuid> --inherit <setting>` removes one setting from the machine-specific configuration, so that the machine follows the global value of it again; its other settings are kept. The confirmation prompt shows the global value the machine will follow, and warns when that is a global client mode that blocks unknown executions. The setting is named as its flag is (`client-mode`, `allowed-paths`, `blocked-paths`, `batch-size`, `bundles`, `transitive-rules`, `full-sync-interval`, `upload-logs`, `enable-all-event-upload`, `disable-unknown-event-upload`, `override-file-access-action`, `block-usb-mount`, `remount-usb-mode`, `event-detail-url`, `event-detail-text`), and the flag can be repeated. It is refused with `--global`, and together with a flag that sets the same setting.
- `rudolph config set --machine <uuid>` writes a complete machine-specific configuration: the client mode, path regexes, batch size, bundles and transitive rules are always stored, so the machine no longer follows the global values of these. The full sync interval and the upload logs URL are only stored when they are set (a full sync interval other than 0, an upload logs URL other than ""); otherwise the machine follows the global values of them, like the optional settings it does not set. Machine-specific configurations written by earlier versions of Rudolph hold the same attributes and behave the same way. This changes what a machine whose row lacks them gets: earlier versions sent it no upload logs URL and no full sync interval (Santa kept its current values), Rudolph now sends the global ones.
- A machine-specific configuration row can therefore hold any subset of the settings, or none (e.g. a row created by `config update --machine <uuid> --clean-sync`). Tools that read the rows, such as rudolph-web, must treat every missing setting of a machine row as inherited from the global row.

Stored values are checked when they are written: `config set` / `config update` (and the model) refuse a client mode other than monitor, lockdown or standalone, a batch size outside 1 to 500, a full sync interval below 60 seconds or above 4294967295, an unknown file access action, and more than 32 remount flags, with an error naming the setting. A value that does not fit (edited by hand in DynamoDB or written by other tooling) never fails the preflight: it is left out of the response (client mode, full sync interval, file access action, remount flags) or replaced by the default (batch size 50), and logged; the other settings are still sent. When the remount flags are left out (too many, or not a list of strings), `block_usb_mount: true` is left out as well: Santa would otherwise block USB mass storage outright instead of remounting it with the intended flags, so the Mac keeps its current USB behaviour until the configuration is fixed.

A clean sync of one machine is requested with `rudolph config update --machine <uuid> --clean-sync`, which changes no other setting (or with `config set --machine <uuid> ... --clean-sync`, which also replaces the machine's configuration). Because `config set --machine` replaces the whole configuration, running it without `--clean-sync` cancels a requested clean sync that has not been performed yet, which for a Mac that is offline can stay pending for weeks; it warns when it does. Every sync of that machine is a clean sync until one is confirmed, and the flag is then cleared, so it applies once:
- The postflight of the clean sync clears the flag. A clean sync that does not complete (e.g. its rule download failed, its response was lost, or santad failed to apply it) is therefore performed again by the next sync, until one completes.
- Otherwise, the clean sync is confirmed by the same evidence the delivery tracking uses (see [rule-pagination](./rule-pagination.md#macs-whose-postflight-never-arrives)): when the `rules_hash` in the next preflight differs from the one the clean sync's preflight reported, the Mac applied it, and that preflight clears the flag.
- A Mac whose postflights do not arrive may never confirm it: when it already held exactly the rules a clean sync sends, applying one does not change its hash. A served last page is no proof (the response may have been lost), so a clean sync whose last page was served but whose hash did not change is repeated, and after 3 such clean syncs the request is given up (and logged): either the Mac already held those rules, or all 3 responses were lost. A clean sync that never had its last page served is not counted, so a requested clean sync that did not happen is never forgotten, and one that did repeats at most 3 times. This only applies to a Mac that was already missing postflights when the request's first clean sync started; it is judged once, because every failed attempt adds a sync without postflight. A Mac whose postflights arrived until then gets a postflight for a clean sync it applies, so only that or a changed hash confirms its request; if its postflights stop arriving during the request and its hash does not change, it keeps clean syncing until the request is cancelled.
- Each request is identified (`CleanSyncRequest` on the configuration row), so a request made while an earlier clean sync of the machine is still running is not cleared by that clean sync. The identifier is removed whenever the flag is cleared, so a flag set to true again without one (by hand in DynamoDB, or by other tooling) is a new request. A flag without identifier (also one set by an earlier version of Rudolph) is handled the same way, except that a clean sync whose postflight arrived counts as its end: if the flag is still set after it, the next sync is another clean sync. `config update --machine <uuid> --clean-sync=false` cancels a request.

The flag is refused for the global configuration, where it would make the whole fleet clean sync at once. A global flag stored by an earlier version of Rudolph, which accepted `config set --global --clean-sync`, is ignored (`config get --global` shows it as ignored); remove it with `rudolph config update --global --clean-sync=false`.

#### Temporary client modes
A machine can be put into another client mode for a limited time, e.g. monitor mode for a developer who needs an afternoon of unblocked builds on a lockdown fleet, or lockdown on a monitor fleet while a Mac is investigated:

```
rudolph config update --machine <machine-uuid> -c monitor --for 2h
rudolph config update --machine <machine-uuid> -c lockdown --until 2026-11-07T17:00:00+11:00
rudolph config update --machine <machine-uuid> --end-temporary-mode
```

- `--for` takes a Go duration (`90m`, `2h`, `48h`), `--until` an RFC 3339 time. One of them is required with `-c`, and the end must be in the future and at most 30 days ahead. They are refused with `--global`, and together with any other setting (run a separate `config update` for those).
- Monitor, lockdown and standalone can all be temporary. A temporary lockdown or standalone mode asks for the same confirmation as setting that mode on the machine, because Santa then blocks unknown binaries on that Mac; declining it is an error and stores nothing. A temporary monitor mode, and `--end-temporary-mode`, are applied without asking.
- The temporary mode never changes the machine's own `ClientMode`. Until it ends, preflight sends it instead of the mode the Mac would otherwise get (its own or the inherited global one); afterwards that mode is sent again, with nothing to clean up. A Mac without a machine-specific configuration gets a row holding only the temporary mode, so it keeps inheriting every other setting (`--end-temporary-mode` leaves such an empty row, which inherits everything, like no row).
- Both the switch and the return take effect at the Mac's next sync, so up to its sync interval (`FullSyncInterval`, 600 seconds by default) after the command or the end; run `santactl sync` on the Mac to switch at once. The end is checked at every preflight; the machine configuration is never cached, so no Lambda instance keeps sending an ended temporary mode.
- `config get --machine <uuid>` shows the temporary mode and its end, in local time and in UTC, or that it has ended and is ignored. `--end-temporary-mode` shows the mode the Mac returns to.
- `config set --machine <uuid>` replaces the machine's configuration but keeps a temporary mode that has not ended (it says so before asking for confirmation): it is about the machine's usual settings, and the exception should still end when it was meant to. The `ClientMode` it sets applies once the temporary mode ends. An ended temporary mode is dropped. `config update --machine <uuid> -c <mode>` without `--for` or `--until` likewise changes only the machine's own mode.

The row stores the temporary mode in two attributes, which tools writing the rows, such as rudolph-web, can set and remove as well:

| Attribute | Type | Meaning |
|---|---|---|
| `TemporaryClientMode` | Number | `1` monitor, `2` lockdown, `3` standalone, as `ClientMode` |
| `TemporaryClientModeExpiresAt` | Number | Unix seconds at which the temporary mode ends |

They only ever appear on a machine's row (`PK` `Machine#<uuid>`, `SK` `Config`); on the global row they are ignored. A temporary mode applies only while both are present, the mode is a known one, and the current time is before `TemporaryClientModeExpiresAt`. Values that cannot be decoded (a string instead of a number, an unknown mode, a missing expiry) are ignored like any other mistyped setting: the Mac gets its usual mode. Ended attributes may stay on the row; readers must compare the expiry with the current time rather than treat their presence as an active temporary mode. To end one, remove both attributes.

The path regexes are always sent, because Santa 2026.8 only clears a regex when it receives an empty string; Rudolph requires Santa 2026.8 or later.

Settings are managed with `rudolph config set` / `rudolph config update`. Remount flags (`--remount-usb-mode`) are only accepted when USB mounts are blocked (`--block-usb-mount`, or inherited from the global configuration), because Santa ignores them otherwise.

A path regex flag can be repeated; each value is one regex (commas included). Santa matches a path regex from the start of the path (it prepends `^` when missing), and several regexes are combined into `^(?:(?:regex1)|(?:regex2))`, which matches a path if any of them matches from the start:

```
rudolph config update --global --blocked-paths '^/Users/[^/]+/Downloads/' --blocked-paths '/tmp/'
rudolph config update --machine <machine-uuid> --override-file-access-action audit_only --event-detail-url 'https://rudolph.example.com/blocked/%file_sha%'
```

A **single** regex is sent unchanged, so Santa anchors only what comes before its first top-level `|`: `--allowed-paths '/tmp/|/private/'` reaches Santa as `^/tmp/|/private/` and allows `/Users/bob/private/evil`. The CLI warns about this; wrap the alternation in a group (`'^(?:/tmp/|/private/)'`) or pass each alternative as its own flag, which anchors all of them.

The CLI validates regexes with Go's RE2 syntax, which differs from the ICU syntax Santa uses: for example RE2 has no lookahead (`(?!...)`). Pass `--skip-regex-validation` to set such a regex. Check it carefully, because nothing else will:
- Santa silently ignores a path regex that is not valid ICU, leaving the setting unset on the host.
- Combining several regexes wraps each one in `(?:...)`, which does not capture, but capture groups are numbered across the whole combined regex: a backreference such as `\1` in the second regex then refers to a group of the first one. Use only one regex per flag value, or no backreferences.
- In a regex with the ICU comment flag `(?x)`, a `#` comment runs to the end of the line. As the combined regex is one line, such a comment swallows the regexes after it. Avoid `(?x)` in combined regexes.

More detailed documentation: https://santa.readthedocs.io/en/latest/deployment/configuration/#sync-server-provided-configuration

Client Requesting Clean Sync
The Santa sensor can “request” for a sync process to be “clean”. This shows up as a POST BODY parameter of request_clean_sync: true but has no further meaning unless the server respects this (see below).

NOTE: You can force this using: santactl sync --clean

Server Requesting Clean Sync
The Rudolph server can command the Santa sensor to destroy all of its local rules and re-download all rules from scratch. This is known as a “clean sync” and the server can mandate this by sending back clean_sync: true in the preflight response. Doing so will cause the client to clear its rules database immediately, prior to doing ruledownload.

NOTE: Even if the Client requests a clean sync, the server is not obligated to respect it.

Rudolph also clean syncs a Mac periodically (every 7 to 17 days, depending on its machine ID). Only a clean sync that
completed counts: when the preflight that decided one is sent again (Santa retries a request whose response it did
not get) or the sync fails, the next sync is a clean sync again. Santa 2026.8 only clears its rules at a clean sync
whose download holds at least one rule it can parse, so a clean sync with no rule to send sends a `REMOVE` of a
placeholder identifier (see [rule-pagination](./rule-pagination.md#tracking-storage-and-costs)).

A Mac that reports holding no rules at all is also clean synced, so a Mac whose rule database was lost gets its rules
back. If Rudolph has no rules for it either (a new deployment in monitor mode, before any rule is written), every sync
of that Mac is therefore a clean sync, and Santa 2026.8 uploads no events during a clean sync: it deletes them unless
the Mac's Santa configuration profile sets `EnableCleanSyncEventUpload` to true. Set it in the MDM profile, or add any
rule, before relying on events from such Macs.

#### Request - JSON

```json
{
    "os_build":"20D5029f",
    "santa_version":"2021.1",
    "hostname":"my-awesome-macbook-pro.attlocal.net",
    "transitive_rule_count":0,
    "os_version":"11.2",
    "certificate_rule_count":0,
    "client_mode":"MONITOR",
    "serial_num":"C02123456789",
    "binary_rule_count":0,
    "primary_user":"",
    "compiler_rule_count":0
}
```

#### Limits on what a host reports
Preflight stores what the host reports on the machine's sensor data row, so every stored value is bounded. A preflight
that exceeds any limit below is refused with HTTP 400 and nothing is written; the host keeps its current rules and
configuration and fails that sync. The limits are far above anything a Mac reports; they exist because an unbounded
value could push the row past DynamoDB's 400 KB item limit, and an oversized serial number (a secondary index key,
limited to 2 KB there) would make every preflight for that host fail.

| Field | Limit (bytes) |
|---|---|
| `serial_num` | 64 |
| `hostname` | 255 |
| `model_identifier` | 64 |
| `os_version`, `os_build` | 32 each |
| `santa_version` | 64 |
| `primary_user` | 256 |
| `primary_user_groups` | 256 entries of up to 256 bytes each |
| `rules_hash` | 128 |

Postflight applies the same kind of check to what it stores: `sync_type` must be a sync type Rudolph knows (`NORMAL`,
`CLEAN` or `CLEAN_ALL`, in either case) and `rules_hash` is limited to 128 bytes. Request bodies of any sync stage are
limited to 6 MiB after decompression (HTTP 413 above that).

Preflight, eventupload and postflight require `Content-Type: application/json` (HTTP 415 otherwise). The header name
and media type are matched case-insensitively and parameters are allowed, so `content-type: Application/JSON;
charset=UTF-8` is accepted.

#### Sensor data row
Every preflight replaces the machine's sensor data row (`PK = Machine#<machine id>`, `SK = Current`, `DataType = SensorData`) with what the host reported, plus `Time` (the time of the preflight) and `ExpiresAfter` (the row expires 90 days after the last preflight).

A Mac that reports no serial number (virtual machines and some repaired logic boards) syncs normally; its sensor data
row is stored without `SerialNum`, so it is simply absent from the serial number index (`rudolph lookup serial-number` cannot
find it). Rudolph never stores an empty string in an attribute that is a secondary index key, which DynamoDB rejects.

The sensor data row also stores `HostnameKey`, the reported hostname trimmed of surrounding whitespace and in lower case, so the web view can search Macs by hostname prefix without regard to case. `Hostname` itself is stored as reported. A Mac that reports no hostname has neither attribute, and one whose hostname is only whitespace has `Hostname` but no `HostnameKey`.

Likewise `SerialKey` is the reported serial number trimmed of surrounding whitespace and in upper case. Apple serial numbers are upper case, but some virtual machines report mixed case, which an upper-case prefix search would never match against the serial as reported. `SerialNum` itself is stored as reported. A Mac that reports no serial number has neither attribute, and one whose serial number is only whitespace has `SerialNum` but no `SerialKey`.

`LastSeenDay` is the UTC date of the last preflight (`2006-01-02` form, from the same clock as `Time`). It gives the machine list a last-seen column that, unlike `Time`, changes at most once a day.

The web view lists Macs from two global secondary indexes on these rows; Rudolph itself does not query them:

| Index | Partition key | Sort key | Used for |
|---|---|---|---|
| `DataType_SerialKey` | `DataType` | `SerialKey` | the machine list ordered by serial number, and case-insensitive serial number prefix search |
| `DataType_HostnameKey` | `DataType` | `HostnameKey` | case-insensitive hostname prefix search |

A row is in an index only if it has that index's sort key, so a Mac without a serial number or hostname is missing from that index alone. Besides the keys, both indexes project `SerialNum` (the serial as reported, for display), `MachineID`, `Hostname`, `ModelIdentifier`, `OSVersion`, `SantaVersion`, `ClientMode`, `PrimaryUser` and `LastSeenDay`, and each also projects the other's sort key (`HostnameKey` or `SerialKey`), so both return the same row shape. DynamoDB writes to an index only when a write changes the item's key or projected attributes in it, so a steady-state preflight, which rewrites the row with a new `Time` and `ExpiresAfter`, writes nothing to either index; a Mac costs each index about one write a day (when `LastSeenDay` changes) plus one whenever its inventory changes (an OS or Santa upgrade, a new primary user). A changed hostname or serial number is dearer: it changes that index's sort key, which DynamoDB counts as two writes to that index (a delete and a put), and one write to the other index, which projects it. A roaming laptop whose reported hostname follows the network it is on may therefore cost `DataType_HostnameKey` two writes each time it moves. Projecting `Time`, `ExpiresAfter`, `RulesHash` or the rule counts would instead cost a write to each index on every sync.

### Response - JSON

More info found here: https://santa.readthedocs.io/en/latest/deployment/configuration/#sync-server-provided-configuration

```json
{
	"client_mode": "LOCKDOWN",
	"blocked_path_regex": "^(?:(?:/tmp/)|(?:/Users/[^/]+/Downloads/))",
	"allowed_path_regex": "^/usr/local/bin/",
	"batch_size": 37,
	"enable_bundles": true,
	"enable_transitive_rules": false,
	"full_sync_interval": 600,
	"upload_logs_url": "/aaa",
	"block_usb_mount": true,
	"remount_usb_mode": ["rdonly", "noexec"],
	"sync_type": "normal",
	"override_file_access_action": "AUDIT_ONLY",
	"enable_all_event_upload": true,
	"event_detail_url": "https://rudolph.example.com/blocked/%file_sha%",
	"event_detail_text": "Request access"
}
```


### Logupload - Not implemented

#### URL - HTTP POST /logupload/{machine_uuid}
** This endpoint is not enabled in the deployed version of Rudolph and must be enabled via configuration MDM profiles **

logupload - endpoint will upload all new changes to the Santa.log file and uploads the contents to the sync server for ingestion
Logs stored in /var/db/santa/santa.log
Work in progress to move this to JSON or protocol buffer
If KEXT is in use → /usr/bin/log show --info --debug --predicate 'senderImagePath == "/Library/Extensions/santa-driver.kext/Contents/MacOS/santa-driver"'

### Eventupload

#### URL - HTTP POST /eventupload/{machine_uuid}
This endpoint allows the Santa sensor to upload any DENY events to the Rudolph server. This endpoint is useful for gathering logs but is unimportant to the ruledownload part of sync.

This endpoints accepts events logs from the Santa agent which records all binary/application executions. Once the data is uploaded to Rudolph, it is sent via Kinesis Stream.

#### Request - JSON

```json
{
  "events": [
    {
      "parent_name":"","file_path":"\/usr\/local\/Cellar\/git\/2.30.0\/bin",
      "quarantine_timestamp":0,
      "logged_in_users":["derek_wang"],
      "signing_chain":[
        {
          "cn":"Developer ID Application: Hashicorp, Inc. (D38WU7D763)",
          "valid_until":1730930319,
          "org":"Hashicorp, Inc.",
          "valid_from":1573077519,
          "ou":"D38WU7D763",
          "sha256":"7576abcd4a9aa8a96f2860e6a0e38dabfd87491934201bf884835a5d911da500"
        },
        {
          "cn":"Developer ID Certification Authority",
          "valid_until":1801519935,
          "org":"Apple Inc.",
          "valid_from":1328134335,
          "ou":"Apple Certification Authority",
          "sha256":"7afc9d01a62f03a2de9637936d4afe68090d2de18d03f29c88cfb0b1ba63587f"
        },
        {
          "cn":"Apple Root CA",
          "valid_until":2054670036,
          "org":"Apple Inc.",
          "valid_from":1146001236,
          "ou":"Apple Certification Authority",
          "sha256":"b0b1730ecbc7ff4505142c49f1295e6eda6bcaed7e2c68c5be91b5a11001f024"
        }
      ],
      "ppid":69149,
      "executing_user":"derek_wang",
      "file_name":"git",
      "execution_time":1611254994.299253,
      "file_sha256":"d5af4e67bedacb605a415c992d31812a0ca5cfbacf306548d332531d27cd956f",
      "decision":"BLOCK_BINARY",
      "pid":70053,
      "current_sessions":["derek_wang@console","derek_wang@ttys000","derek_wang@ttys001"]
    }
  ]
}
```

#### RESPONSE - None

#### Delivery to the event sink
Events are written to the configured sink (Firehose, Kinesis Data Streams and/or a Lambda function). Each upload is
split into as many requests as the service's limits require (Firehose PutRecordBatch: 500 records / 4 MiB; Kinesis
PutRecords: 500 records / 1 MiB; asynchronous Lambda invocations: 256 KB payloads). Kinesis requests are capped at
1 MiB, below the API's 5 MiB, because every record of an upload has the machine ID as partition key and so goes to a
single shard, which accepts 1 MiB and 1,000 records per second.

Records the service fails individually are retried alone, up to 6 attempts. After a throughput error
(`ProvisionedThroughputExceededException` from Kinesis, `ServiceUnavailableException` from Firehose) the retry waits
1 to 1.5 seconds so it lands in the next throughput window; after any other error it backs off from 100 ms. A large
upload to a single Kinesis shard therefore goes through at about 1 MiB per second.

All sink calls, including the AWS SDK's own retries, are bounded by the Lambda invocation's deadline minus 1.5
seconds (minus half the remaining time when less than 3 seconds remain, so a short invocation still leaves the sinks
time to send). When that deadline is near, the handler stops and answers 500 itself rather than being killed by the
Lambda timeout (which API Gateway would turn into a 502). With the module's default 10 second eventupload timeout this
leaves about 8.5 seconds, enough for roughly 6 to 7 MiB of events to one Kinesis shard. A longer timeout gives more:
set the `santa_api` module's `eventupload_lambda_timeout_seconds` variable (default 10; the api-handler module accepts
3 to 900 whole seconds, and API Gateway itself waits at most 29 seconds by default).

Uploads are accepted up to 6 MiB after decompression, so a maximum-size upload takes most of the default budget even
on an idle shard. If the shard is also carrying other traffic (a stream shared with other producers, or many Macs whose
machine IDs hash to the same shard), or an additional Lambda sink (`eventupload_output_lambda_name`) runs in the same
invocation, the upload can reach the deadline on every retry, answer 500 each time and block that Mac's events. Raise
`eventupload_lambda_timeout_seconds` for headroom, and add shards to the Kinesis stream if it is regularly throttled
(the stream's `WriteProvisionedThroughputExceeded` CloudWatch metric shows this).

The HTTP status tells Santa whether to keep the batch:

- **200** when every event was delivered, and also when the only events not delivered are ones the sink can **never**
  store (for example a single event larger than a Firehose or Kinesis record, about 1 MB, or a Lambda async payload).
  Those events are dropped and the handler logs a line containing `dropped` with the count, the machine ID and the
  reason, never the event itself. Santa stops at the first failed batch and re-sends it on every sync, so failing the
  upload for such an event would block every later event from that Mac.
- **500** when the sink is throttled or failing, or records kept failing after the retries. Santa keeps the batch and
  sends it again on the next sync; events of that batch that were already delivered may then arrive twice.

### Ruledownload

#### URL - HTTP POST /ruledownload/{machine_uuid}
This endpoint allows the Rudolph server to push down new rules to the Santa sensor. This process is complex and is explained within the [rule-pagination page](./rule-pagination.md)

#### Strategies Definitions:
- 1 = Downloads all GlobalRules
- 2 = Downloads all FeedRules which is determined via the last sync state time
- 3 = Downloads all MachineRules (only in cursors issued by earlier versions; machine-specific rules are now sent at the end of the last page, see [rule-pagination](./rule-pagination.md#machine-specific-rules))

#### Request - JSON
```json
{
	"cursor": {
		"strategy": 2,
		"batch_size": 7,
		"page": 2,
		"pk": "AAAA",
		"sk": "eeeeee"
	}
}
```

### Postflight

#### URL - HTTP POST /postflight/{machine_uuid}
This endpoint is called at the end of a successful sync process. It is the way for the Santa sensor to inform the Rudolph server that the sync process was successful and all rules were correctly committed.

Rudolph keeps state records of every synchronization event that occurs including information on time to process the entire synchronization process. 

Postflight also clears the clean sync flag of the machine's configuration when the sync it closes is the clean sync the flag requested (see [Reconfiguring the Sensor](#reconfiguring-the-sensor)).

Santa ignores a failed postflight. Rudolph also counts a sync as completed when the Mac's `rules_hash` in the next preflight shows it applied the sync (the hash changed), when the sync's last rule page was served and it served nothing that the covering completed sync had not already served, or, as a fallback for an incremental sync whose last page was served, when no earlier sync counting through the fallback carried content that fewer than 8 syncs have served. A Mac whose postflights never arrive thus keeps its feed cursor moving (a sync counts at least every 8 syncs, whatever arrives) and gets removals confirmed. Content that changes nothing on the Mac's rules hash (a custom message or URL edit, a REMOVE of a rule it does not hold) is sent again by up to about 15 syncs before the fallback moves past it. A feed rule or removal is lost only after 8 syncs in a row that served it failed after their last page was served (9 when the first is also the first postflight that did not arrive), unless a stray postflight arrives for one of them. The exception is a feed row written late with a stamp inside the feed cursor's overlap window (a CLI clock up to 2 minutes behind, or about a second of eventual consistency): a single such failed sync loses it, as if a stray postflight arrived for every failed sync. The fallback never counts a clean sync as completed; a periodic clean sync whose last page was served counts as performed for scheduling the next one when the Mac was already missing postflights and an earlier clean sync counted, so such a Mac does not repeat it on every sync, and a Mac whose postflights arrive retries a clean sync that did not complete (see [rule-pagination](./rule-pagination.md#macs-whose-postflight-never-arrives)).

#### Request - Blank

#### Response - Blank - Sends HTTP Status 200

# Configuration
Unlike many other sensors, Santa is not configured via a .conf or .yaml file on the disk; it is configured by a [MacOS Configuration Profile](https://developer.apple.com/business/documentation/Configuration-Profile-Reference.pdf).

An example configuration is provided in the docs.

The format of this profile is documented here: https://santa.readthedocs.io/en/latest/deployment/configuration/ 