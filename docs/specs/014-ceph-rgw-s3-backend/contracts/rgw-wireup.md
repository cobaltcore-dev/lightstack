# Contract — RGW ↔ o3k wireup

The external configuration that makes Ceph RGW trust o3k as the S3 auth authority, plus the
o3k-side config. This is operator/fixture config (documented + scripted), not o3k code — except
the o3k config fields and the catalog entry.

## RGW side (ceph.conf / Rook `CephObjectStore` config)

```ini
[client.rgw.*]
rgw s3 auth use keystone       = true
rgw keystone api version       = 3
rgw keystone url               = https://<o3k-keystone>:35357
rgw keystone implicit tenants  = s3          # per-project bucket namespaces (FR-008)
rgw keystone accepted roles    = admin,member,reader
# rgw keystone admin token/user: NOT required — o3k /v3/s3tokens self-validates the signature.
# TLS: if o3k Keystone uses an internal CA, RGW must trust it (rgw keystone verify ssl / CA).
```
Rook: set the equivalent under the `CephObjectStore`/RGW config overrides; ensure RGW can reach
o3k Keystone on the network (cross-cluster routing is manual/test-setup — spec §5).

## o3k side

Config (o3k.yaml `glance:` — for capability A) + env creds:
```yaml
glance:
  storage_mode: s3
  s3_endpoint: https://<rgw>:443
  s3_bucket: images
  s3_region: us-east-1
  s3_ca_bundle: /etc/o3k/rgw-ca.pem        # NEW (FR-015); omit for HTTP/public CA
  s3_insecure_skip_verify: false           # NEW; test-only, never default
```
- Credentials for the Glance backend: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (env, default chain).
- Catalog: migration `062` adds a `services(type=s3)` + `endpoints(public, url=<rgw>)` row so clients discover the endpoint (FR-009).

## Network / trust requirements
- RGW → o3k Keystone `:35357` reachable (for `/v3/s3tokens`).
- Clients (and, for the manual path, tenant VMs) → RGW S3 endpoint reachable.
- If either side uses TLS with an internal CA, the other must trust that CA (o3k: `s3_ca_bundle`; RGW: its keystone CA setting).

## Validation
- `radosgw-admin` shows implicit-tenant users after a tenant authenticates.
- A per-project bucket created by tenant A is invisible to tenant B (isolation).
- Turning RGW's keystone trust off → tenant S3 requests fail (proves o3k is the authority).
