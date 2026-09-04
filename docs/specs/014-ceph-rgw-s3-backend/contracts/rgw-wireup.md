# Contract — RGW ↔ Lightstack wireup

The external configuration that makes Ceph RGW trust Lightstack as the S3 auth authority, plus the
lightstack-side config. This is operator/fixture config (documented + scripted), not Lightstack code — except
the Lightstack config fields and the catalog entry.

## RGW side (ceph.conf / Rook `CephObjectStore` config)

```ini
[client.rgw.*]
rgw s3 auth use keystone       = true
rgw keystone api version       = 3
rgw keystone url               = https://<lightstack-keystone>:35357
rgw keystone implicit tenants  = s3          # per-project bucket namespaces (FR-008)
rgw keystone accepted roles    = admin,member,reader
# rgw keystone admin token/user: NOT required — lightstack /v3/s3tokens self-validates the signature.
# TLS: if lightstack Keystone uses an internal CA, RGW must trust it (rgw keystone verify ssl / CA).
```
Rook: set the equivalent under the `CephObjectStore`/RGW config overrides; ensure RGW can reach
Lightstack Keystone on the network (cross-cluster routing is manual/test-setup — spec §5).

## Lightstack side

Config (lightstack.yaml `glance:` — for capability A) + env creds:
```yaml
glance:
  storage_mode: s3
  s3_endpoint: https://<rgw>:443
  s3_bucket: images
  s3_region: us-east-1
  s3_ca_bundle: /etc/lightstack/rgw-ca.pem        # NEW (FR-015); omit for HTTP/public CA
  s3_insecure_skip_verify: false           # NEW; test-only, never default
```
- Credentials for the Glance backend: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (env, default chain).
- Catalog: migration `062` adds a `services(type=s3)` + `endpoints(public, url=<rgw>)` row so clients discover the endpoint (FR-009).

## Network / trust requirements
- RGW → Lightstack Keystone `:35357` reachable (for `/v3/s3tokens`).
- Clients (and, for the manual path, tenant VMs) → RGW S3 endpoint reachable.
- If either side uses TLS with an internal CA, the other must trust that CA (lightstack: `s3_ca_bundle`; RGW: its keystone CA setting).

## Validation
- `radosgw-admin` shows implicit-tenant users after a tenant authenticates.
- A per-project bucket created by tenant A is invisible to tenant B (isolation).
- Turning RGW's keystone trust off → tenant S3 requests fail (proves Lightstack is the authority).
