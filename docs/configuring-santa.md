# Deploying Santa Agents
For reference, it's recommended to read over how to [configure Santa](https://northpole.dev/configuration/keys/).

Rudolph requires **Santa 2026.8 or newer**, and is tested with Santa 2026.8 and 2026.9. Santa 2025.12, the last release for macOS 13, is supported only for macOS 13 Macs, for basic sync, with these limits:

- Santa 2025.12 compiles a path regex as it receives it, so an empty one would match every path. Where no allowed or blocked path regex is configured, it gets a regex that never matches, `(?!x)x`, instead of the empty string that clears the regex on newer releases. A configured regex is sent unchanged.
- Santa 2025.12 does not know the `SILENT_GUI_BLOCKLIST` and `SILENT_TTY_BLOCKLIST` policies (they arrived in 2026.6) and would skip such a rule, leaving the binary unblocked, so those rules are sent to it as `SILENT_BLOCKLIST`: the binary is blocked without any notification.
- CEL rules are evaluated by Santa 2025.12's older CEL support, which may not parse an expression written for Santa 2026.x; such a rule may then not apply on those Macs.
- When a Mac is upgraded from Santa 2025.12 to 2026.6 or newer, its next sync is a clean sync, so that it gets the real policies of the rules that were sent as `SILENT_BLOCKLIST`. A Mac going back to Santa 2025.12 (or syncing with it for the first time since Rudolph adapted to it) also gets a clean sync.

Santa 2025.12 supports `SyncExtraHeaders`, so it authenticates with the settings described below. Releases older than 2025.12 are not supported; they compile path regexes as received and skip unknown policies too, so Rudolph sends them the same adjusted responses as 2025.12. Rudolph reads the release from the `santa_version` each Mac reports at preflight; a missing or unreadable version is treated as the current release.

Provided here are some accelerated instructions.

## Full Disk Access and Sysext Approval
For unattended installs of Santa you'll want to approve Santa's system extension and full disk access. For convenience we've provided some sample configuration profiles for North Pole Security's Santa (team ID `ZMCG7MLDV9`) here to get you started:

* [configs/santa-sysext.mobileconfig](/configs/santa-sysext.mobileconfig)
* [configs/santa-tcc.mobileconfig](/configs/santa-tcc.mobileconfig)

Make sure to doctor them up a bit prior to a real production deploy.


## Configuration Profile
The Santa agent is configured via MacOS Profiles. Take a look at the example `.mobileconfig` file we've provided
[configs/santa-configuration.mobileconfig](/configs/santa-configuration.mobileconfig). Below we'll go over some of
the **_more important_** values to pay attention to.

### `SyncBaseURL`
The most important parameter to consider is the `SyncBaseURL`. The value here must match the `sync_base_url`
that is output by your `make deploy` command. This will instruct the Santa sensor to treat your Rudolph sync server
as its authority.

Santa 2026.8 reads its managed preferences from the `com.northpolesec.santa` domain.

### `SyncExtraHeaders` (required): the sync token
Rudolph only answers clients that send a bearer token. Santa adds it to every sync request through the
`SyncExtraHeaders` dictionary:

```xml
<key>SyncExtraHeaders</key>
<dict>
    <key>Authorization</key>
    <string>Bearer REPLACE-WITH-YOUR-SYNC-TOKEN</string>
</dict>
```

Generate a high-entropy token once, and store it in your MDM or secrets manager:

```
TOKEN="$(openssl rand -base64 32)"
```

Rudolph never stores the token itself. Deploy its SHA-256 digest instead, via `sync_token_sha256` in your environment's
`config.auto.tfvars.json`:

```
printf '%s' "$TOKEN" | shasum -a 256 | cut -d ' ' -f 1   # macOS; on Linux use sha256sum instead of shasum -a 256
```

```json
"sync_token_sha256": ["3f0c...64 hex characters...9a1b"]
```

Hash exactly the characters that follow `Bearer ` in the header (use `printf '%s'`, not `echo`, so no newline is
hashed). The authorizer denies every request when no digest is configured, when the header is missing, or when the token
does not match.

Anyone holding the token can sync as any machine ID, so treat it like a password: deliver it only through configuration
profiles, and rotate it when it leaks.

#### Rotating the token
1. Generate a new token and add its digest next to the old one, then deploy:
   `"sync_token_sha256": ["<old digest>", "<new digest>"]`.
2. Roll out the configuration profile with the new token and wait until every host has received it.
3. Remove the old digest and deploy again.

API Gateway caches the authorizer's decision per token for `authorizer_result_ttl_in_seconds` (default 300), so a
removed token can keep working for up to that long.

### Client certificates (optional mTLS)
For a stronger guarantee that only managed hosts reach Rudolph, require client certificates on the custom domain:

1. Upload a PEM bundle of the CA certificates that issue your hosts' client certificates (for example the CA of your MDM
   SCEP/ACME payload) to S3, and set `mtls_truststore_uri` (e.g. `s3://my-bucket/rudolph/truststore.pem`) and, if the
   bucket is versioned, `mtls_truststore_version` in `config.auto.tfvars.json`. Update the version to roll out a new
   truststore.
2. Deploy. When mTLS is enabled, the default `execute-api` endpoint is disabled, so hosts must use the `sync_base_url`
   custom domain.
3. Tell Santa which keychain identity to present. `ClientAuthCertificateIssuerCN` is usually the easiest, since it
   matches any certificate issued by your CA:

```xml
<key>ClientAuthCertificateIssuerCN</key>
<string>Acme Device CA</string>
```

Alternatively use `ClientAuthCertificateCN` (the certificate's common name) or `ClientAuthCertificateFile` plus
`ClientAuthCertificatePassword` (a PKCS#12 file on disk). mTLS is in addition to the bearer token, not a replacement.

### `SyncClientContentEncoding` and `SyncEnableProtoTransfer`
Leave `SyncClientContentEncoding` unset: Santa's default (`deflate`) is supported, as are `gzip` and `none`.
Leave `SyncEnableProtoTransfer` unset (off); Rudolph speaks the JSON sync protocol.

### `MachineIDPlist` and `MachineIDKey`
Specifies the `.plist` file and relevant XML key located on a MacOS machine's filesystem that specifies the `MachineID`
of the Santa agent. This `MachineID` is used to uniquely identify the MacOS machine and all rules, configurations, and
logs are tied to this `MachineID`.

### `ClientMode`
We **_highly recommend_** the integer value `1`. This will initialize all Santa sensors in `MONITOR` mode. The mode can be
later remotely changed by Rudolph once everything is set up, but an initial default setting of `MONITOR` will reduce the chances for problems.

Once Santa syncs, Rudolph sends the client mode and the other settings it manages from its own configuration, which is changed with `rudolph config set` / `rudolph config update` and shown with `rudolph config get`, globally (`--global`) or per Mac (`--machine <machine-id>`). See the [CLI reference](cli.md#configuration) for the commands and [sync.md](sync.md) for what each setting does.


## Plist File
Deploy a `.plist` file to the `MachineIDPlist` location, using your MDM or otherwise. We've included an example file, [configs/com.northpolesec.santa.machine-mapping.plist](/configs/com.northpolesec.santa.machine-mapping.plist).

The example profile and the `rudolph` CLI use `/Library/Preferences/com.northpolesec.santa.machine-mapping.plist`. Santa reads whatever file `MachineIDPlist` names, but the CLI reads this path when a command run on a Mac targets that Mac (no `--global` or `--machine`), so keep it unless you always pass `--machine`. Macs set up from the older example still have `/Library/Preferences/com.google.santa.machine-mapping.plist`; the CLI falls back to that file when the `com.northpolesec.santa` one does not exist.

We **_highly recommend_** using **UPPER-CASE-HEXADECIMAL-UUID-WITH-SPACES** (e.g. `AAAABBBB-CCCC-DDDD-EEEE-123456780000`)
for the `MachineID`. The `MachineID` must be a UUID: the API rejects sync requests for any other machine ID.


## Installation
Grab the installation `.dmg` (2026.8 or newer) from https://github.com/northpolesec/santa/releases and install it.


## Verify it Works
After Santa is installed, you can perform your first sync via:

```
sudo santactl sync --debug
```

If the sync completes successfully, you should be good to go. The first sync is a clean sync, and on a deployment with no rules Rudolph sends it one rule, a `REMOVE` of a placeholder identifier, so Santa may report one rule received; the Mac still holds 0 rules (`santactl status` shows the rule counts).

An HTTP 401 means the `Authorization` header is missing or its token does not match a configured digest. With mTLS
enabled, a TLS handshake failure means Santa did not find or present a client certificate the truststore accepts.


## Other Miscellaneous
Santa dumps tons of logs into `/var/db/santa/santa.log`. These logs are extremely useful but this file can fill up very quickly.

We recommend a newsyslog configuration, for example `/private/etc/newsyslog.d/com.northpolesec.santa.newsyslog.conf` (newsyslog reads every file in that directory, whatever its name):
```
# logfilename             [owner:group]       mode   count size(KiB) when   flags [/pid_file] # [sig_num]
/var/db/santa/santa.log   root:wheel          644    10    5000     *      NZ
```
