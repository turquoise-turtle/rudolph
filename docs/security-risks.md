# System Risks and Security

## Unauthorized Clients
API Gateway is publicly reachable, but every request must carry a bearer token (`Authorization: Bearer <token>`), which
Santa sends through `SyncExtraHeaders`. A Lambda authorizer compares the token's SHA-256 digest in constant time with
the digests in `sync_token_sha256` and denies everything when none are configured. Sync routes additionally require a
UUID machine ID. See [configuring Santa](configuring-santa.md).

Remaining risks:
 - The token is shared by the fleet. Anyone who extracts it from a host or a configuration profile can, like any host,
   download rules and configuration, upload events, and sync as any machine ID. Rotate the token when it may have leaked
   (two digests can be valid at once).
 - The authorizer's decision is cached per token for `authorizer_result_ttl_in_seconds` (default 5 minutes) and covers
   all sync routes, so a removed token keeps working for up to that long.
 - Machine ID validation relies on the handlers. The authorizer rejects a non-UUID machine ID only for the request
   that fills the cache, and only when it can deny that exact request: a machine ID containing IAM wildcard characters
   (`*`, `?`) gets the normal Allow policy, because any denial would match (or, once cached, block) every host sharing
   the token. Every sync handler (preflight, eventupload, ruledownload, postflight and xsrf) therefore validates the
   machine ID before it touches storage and answers `400` otherwise. Only the canonical 36 character UUID form
   (`8-4-4-4-12` hex) is accepted, in either case; the spellings a UUID parser would normally also take - braces, a
   `urn:uuid:` prefix, or 32 hex digits without dashes - are refused, so one Mac cannot be addressed under several
   keys. The case of an ID is never rewritten, so a host keeps the identity it already syncs under, and a fleet that
   reports the same UUID in two different cases stores two machines.
 - The token is visible to anyone with access to the Mac's managed preferences.
 - **Tokens in logs of older builds.** Older builds of the API router, eventupload and xsrf handlers log the full
   request, including its headers, so the `Authorization` token would be written to CloudWatch Logs. Do not issue a
   token to hosts before a build without that logging is deployed. If a token was ever used with an older build,
   rotate it after upgrading and delete the old log streams of the `/aws/lambda/<prefix>_rudolph_*` log groups.
   Deploy the Terraform changes and the handlers of this release together (see [deploy.md](deploy.md)); in particular
   `binary_media_types = ["application/json"]` without the handlers that decode compressed bodies makes every sync fail.

For stronger client authentication, enable mutual TLS on the custom domain (`mtls_truststore_uri`) so only hosts with
a certificate from your CA can connect at all. Enabling mTLS disables the default `execute-api` endpoint, which would
otherwise bypass it.

The public API in no way allows clients to modify global rules or configurations.


## Trusting the Wrong Server
A rare (and unconfirmed) vulnerability is when Santa agents are tricked into sync'ing with the wrong server. DNS poisoning is one possible attack vector.

Santa can pin the sync server's certificate: `ServerAuthRootsFile` (a path to a PEM file) or `ServerAuthRootsData` (the PEM itself) in the configuration profile names the root certificates the server's chain must lead to. Without either, Santa trusts any certificate the macOS keychain trusts, from any public certificate authority, for the `SyncBaseURL` host.

How Santa handles the pin (Santa 2026.8, `Source/common/MOLAuthenticatingURLSession.mm`):

- **A pin that loads but does not match** the server's chain fails closed: Santa trusts only the pinned roots, logs `[Server Trust] Unable to verify server identity.` and cancels the connection, so the sync fails.
- **A pin that does not load** (a file Santa cannot read, or one with no certificate it can parse) is ignored: Santa logs `Unable to read server root certificate file ...` or `Unable to read server root certificates from data ...` and falls back to the keychain's trust. Nothing else signals it, so after setting or changing a pin, run `sudo santactl sync --debug` on a test Mac and check that the sync succeeds and neither line appears. To prove the pin is enforced, put a different, valid root in it, check that the sync fails with `Unable to verify server identity`, then restore it.

With API Gateway and an ACM certificate, the roots to pin are Amazon's (Amazon Root CA 1 to 4, or at least the one that anchors the chain; `openssl s_client -connect HOST:443 -showcerts` shows it). That narrows trust from every public certificate authority to Amazon's, not to this one server; a CAA record on the domain limiting issuance to Amazon complements it.

Without a working pin, then, an attacker who can redirect a Mac (by DNS poisoning, say) and present a certificate the Mac trusts for the server's name can make it sync with the wrong server. The rogue server can send any rules and configuration (allow its own binaries, block everything, switch the Mac to monitor mode), and it receives everything Santa sends in a sync:

- **Every request:** the fleet's sync token, in the `Authorization` header, and the Mac's machine ID in the URL. With the token the attacker can sync with the real Rudolph as any Mac (see [Unauthorized Clients](#unauthorized-clients)), so rotate the token if you suspect this.
- **preflight:** the serial number, hostname, primary user and the primary user's groups, model identifier, OS version and build, Santa version, SIP status, client mode and rule counts.
- **eventupload:** execution events, with file paths and names, the executing user, the logged-in user names and current sessions, the parent process name, process IDs, file hashes, signing chains, bundle details and quarantine download and referrer URLs; and file access events, with the paths accessed and every path in the process chain. Santa sends the events it has queued, so a Mac that keeps syncing with the rogue server hands over its activity as it happens.
- **ruledownload and postflight:** the sync cursor, rule counts and the rules hash; nothing personal beyond the machine ID.

We consider the risk of a Mac being redirected to be relatively minimal, and as such have not prioritised fixing it, but what it exposes is not: pin the server's certificate (`ServerAuthRootsFile` or `ServerAuthRootsData`) and check that the pin loads, as above, after every change to the profile.


## Some Mitigations
Placing your entire Rudolph environment behind a VPN will prevent unauthorized clients from reading your rules. This
DOES NOT protect you against DNS poisoning attacks.

To configur Rudolph to only trust VPN IPs, use `allowed_inbound_ips` to restrict access to specific CIDR blocks.


## Telemetry and Retention
Preflight and eventupload carry personal and sensitive data: serial numbers, hostnames, primary users and their groups, and, in events, user names, file paths and download URLs. This is every place Rudolph copies it, and how long each copy is kept. Point-in-time recovery and the Firehose bucket's old versions mean a deleted item is not gone at once.

| Copy | What it holds | Kept | Encryption |
|---|---|---|---|
| Store table (`PREFIX_rudolph_store`) | What each Mac reports in preflight (serial number, hostname, primary user, its groups, OS and Santa versions), sync state, rules and configuration, groups and the audit trail | The store table's TTL (`ExpiresAfter`) deletes a Mac's reported data and sync state 90 days after the last preflight that wrote them, and its rule delivery tracking 120 days after the last delivery; TTL usually deletes within a few days of expiry. Items without `ExpiresAfter` (rules, configuration, groups and group membership, the audit trail) stay until deleted. | The store key (`alias/PREFIX-santa-rules-tables-sse`) |
| Events table (`PREFIX_rudolph_events`) | Blocked and unknown executions, those allowed by rules flagged for collection, and every file access event, with paths, file names and the executing user (see [events-table.md](events-table.md)) | Events 90 days after upload; day counters and machine markers 100 days; top list snapshots 8 days | The store key |
| Point-in-time recovery of both tables | A restorable copy of each table as it was at any second | 35 days (the DynamoDB default; the Terraform sets no `recovery_period_in_days`), so items deleted or expired stay restorable that long | The store key |
| Firehose events bucket (`PREFIX-ORG-rudolph-events`), only when Terraform creates the Firehose sink | Every event of every upload, as Santa sent it, unfiltered | 180 days in total by default (`firehose_events_retention_in_days`): S3 expires the object a day before that, and the bucket is versioned, so the expired object stays as a noncurrent version for the last day, to recover one deleted by mistake, before S3 deletes it and then its delete marker. The 180 days are approximate: S3 rounds each lifecycle step up to the next midnight UTC and removes objects asynchronously, with some lag, so an event can stay a little longer. Incomplete multipart uploads are removed after 7 days. | The Firehose module's key, as are records the delivery stream buffers (up to 24 hours when S3 cannot be reached) |
| Kinesis stream (`eventupload_kinesis_name`), when the Kinesis sink is configured | Every event of every upload | The stream is the operator's own: Rudolph only gets permission to write to it. Its encryption, its retention (24 hours by default, up to 365 days) and whatever reads from it are yours to set and check. | Yours to set on the stream |
| Output Lambda (`eventupload_output_lambda_name`), when configured | Every event of every upload, as its invocation payload | The function is the operator's own: Rudolph only gets permission to invoke it. What it keeps, where it sends events, its log retention and its encryption are yours to set and check. | Yours to set on the function and wherever it writes |
| CloudWatch Logs (`/aws/lambda/PREFIX_rudolph_*`, the Firehose log group and, with `enable_api_access_logs`, the API access log `/aws/apigateway/PREFIX_rudolph_access`) | What the handlers log about each request; the API access log adds a line for every request with the source IP address (the Mac's public address, or its NAT's), the method and the resource path template (no machine ID) | `log_retention_in_days` (30 by default) | CloudWatch Logs default encryption |

**Who can read the tables.** As deployed, the store key's policy lets every principal of the account use the key through DynamoDB, so table permissions alone decide who can read the store and events tables: any role or user with DynamoDB read access on them (AWS `ReadOnlyAccess`, say, or a `dynamodb:*` policy for another team) can read all telemetry. Set `store_kms_key_user_arns` to close this: the key may then be used through DynamoDB, and DynamoDB may create grants on it, only by Rudolph's own Lambda roles (health, preflight, ruledownload, postflight, eventupload and the ranker) and the ARNs listed, so a principal outside the list gets an access denied error from DynamoDB even with table permissions. List rudolph-web's role and every role or user that runs the CLI first, or they lose access to the tables. Principals whose own IAM policy grants `kms:Decrypt` on the key (an `AdministratorAccess` role, say) still use it, through the key policy's root statement. The deployer, the role that runs Terraform, must be in the list too, or be given the key through IAM: creating or replacing a table and restoring one from point-in-time recovery make DynamoDB call `CreateGrant` on the key as the principal doing it, so whoever restores a table needs the same. The list does not hold back the key administrators either: those in `kms_key_administrators_arns` cannot use the key themselves, but they can change its policy or create grants on it, and so reach the data. See [deploy.md](deploy.md#25-telemetry-retention-and-encryption) and [cli-iam.md](cli-iam.md).

The list has limits of its own:

- **Other accounts.** ARNs of another account in `store_kms_key_user_arns` never match: the key policy only lets callers of this account (`kms:CallerAccount`) use the key, so a role in another account needs its own way in, such as a role of this account it can assume.
- **Wildcards.** `store_kms_key_user_arns` entries are `ArnLike` patterns, whose `*` also matches `/`, so the plan refuses a wildcard covering a whole path segment (`role/aws-reserved/*`, `role/aws-reserved/sso.amazonaws.com/*`, or `/*/` mid-path): it would let in every role under that path, every Identity Center permission set included, read-only ones too. Use a prefix pattern, such as `role/aws-reserved/sso.amazonaws.com/AWSReservedSSO_RudolphOperators_*`.
- **Prefixes.** The plan only catches whole-segment wildcards; a prefix pattern in `store_kms_key_user_arns` passes, however broad. `role/aws-reserved/sso.amazonaws.com/AWSReservedSSO_*`, with no permission set name after it, still lets in every Identity Center permission set, and `role/a*` lets in every role whose path or name starts with "a", as ArnLike's `*` matches `/`. Choose prefixes as narrow as the roles you mean, ending in the permission set or role name.
- **Rudolph's own roles.** Rudolph's Lambda roles, under the `/rudolph/` path, are always allowed, so whoever can pass a `/rudolph/` role to a Lambda function of their own (`iam:PassRole`), or change Rudolph's Lambda function code or configuration (`lambda:UpdateFunctionCode`, `lambda:UpdateFunctionConfiguration`), runs as a listed role and reads the tables. Keep those permissions as narrow as the list.
