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
   the token. Every sync handler validates the machine ID as a UUID and rejects such requests (UUID validation in all
   handlers is added together with this authorizer; ruledownload lacked it before, see S3 in the audit).
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
A rare (and unconfirmed) vulnerability is when Santa agents are tricked into sync'ing with the wrong server. DNS poisoning
is one possible attack vector.

Santa agents support server trusts by specifying a ServerAuthRootsFile in the macOS configuration profile. This file contains a SSL Certificate chain which is used to verify the identity of the server.

However, there is a bug in Santa: When this file is malformed or if the certificate does not match the SyncBaseURL, Santa defaults back to the Apple MacOS keychain, which will blanket trust anything signed by Amazon Web Services.

Thus it is possible for an attacker to trick a Santa sensor into syncing with the wrong server, forcing it to download incorrect rules. During the Preflight step, the Santa sensor will also upload some basic information about the MacOS machine, such as OS version and Santa version.

We consider this risk to be relatively minimal, and as such have not prioritized fixing it.


## Some Mitigations
Placing your entire Rudolph environment behind a VPN will prevent unauthorized clients from reading your rules. This
DOES NOT protect you against DNS poisoning attacks.

To configur Rudolph to only trust VPN IPs, use `allowed_inbound_ips` to restrict access to specific CIDR blocks.
