# Contract — S3 token validation endpoint (RGW-facing)

The endpoint Ceph RGW calls to authenticate a tenant S3 request against o3k.
**Public** (registered on the `v3` group, bypasses `authMiddleware`) — the S3 signature in the
body *is* the credential; o3k self-validates. No caller/admin token required.

## POST /v3/s3tokens
Called by RGW when `rgw s3 auth use keystone = true`.

Request (RGW-supplied; Keystone `s3tokens` shape):
```json
{ "credentials": {
    "access": "<access_key>",
    "token":  "<base64 string-to-sign / canonical request>",
    "signature": "<client signature>"
} }
```

Processing:
1. Look up the credential by `access` (indexed `credentials.access_key`, `type=ec2`). Not found → 401.
2. Recompute the S3 signature from the stored **secret** over the provided string-to-sign:
   **SigV4** (HMAC-SHA256 chain) primary; **SigV2** (HMAC-SHA1) fallback. stdlib `crypto/hmac`.
3. Constant-time compare vs `signature`. Mismatch → 401.
4. Resolve `user`, `project`(tenant), `roles` from `users`/`projects`/`role_assignments`.

200 (Keystone token document RGW expects):
```json
{ "token": {
    "user":    { "id": "<user_id>", "name": "<user>" },
    "project": { "id": "<project_id>", "name": "<project>" },
    "roles":   [ { "name": "member" }, ... ],
    "expires_at": "<optional>"
} }
```
- The `project.id` is what RGW uses as the tenant for per-project namespacing (`implicit tenants=s3`).

401 on any lookup/signature failure — RGW then denies the S3 request (fail-closed; FR-006/FR-008).

## POST /v3/ec2tokens
Same behavior for EC2-style signing (AWS query/EC2 clients). Shares the lookup + verify code.

## Rules
- Fail-closed: any error → 401, never a permissive default (edge case: RGW/identity trust misconfig).
- No persistence; pure validation.
- Secret must be recoverable (see data-model) to recompute the HMAC.

## Test (contract)
- Valid signature (from an `openstack ec2 credentials`-minted key) → 200 with correct project/roles.
- Tampered signature / unknown access / deleted credential → 401.
- End-to-end: real RGW (`ceph/demo`) validates an aws-cli request via this endpoint (CI).
