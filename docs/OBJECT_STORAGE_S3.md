# Tenant object storage on Ceph RGW (S3)

Lightstack can broker **S3 access to Ceph RGW for tenants**: a project user mints
long-lived S3 credentials from lightstack's identity service, and RGW validates their S3
requests against lightstack. This is distinct from *RGW as Glance's image backend* (see
[S3_CONFIGURATION.md](./S3_CONFIGURATION.md)); here RGW is object storage Lightstack
tenants use directly.

## How it works

1. A tenant runs `openstack ec2 credentials create` → Lightstack mints an access/secret
   pair scoped to their project.
2. RGW is configured with `rgw s3 auth use keystone = true` pointing at lightstack.
3. On each S3 request, RGW calls Lightstack `POST /v3/s3tokens`; Lightstack looks up the
   credential by access key, **recomputes the signature** from the stored secret
   (AWS SigV4, with SigV2 fallback), and returns the user/project/roles.
4. Per-project isolation comes from `rgw keystone implicit tenants = s3` (the
   OpenStack/Swift `AUTH_<project>` account model).

## Operator wireup (RGW ↔ lightstack)

`ceph.conf` (or the Rook `CephObjectStore` config overrides):

```ini
rgw s3 auth use keystone       = true
rgw keystone api version       = 3
rgw keystone url               = https://<lightstack-keystone>:35357
rgw keystone implicit tenants  = s3
rgw keystone accepted roles    = admin,member,reader
```

No `rgw keystone admin token` is required — lightstack's `/v3/s3tokens` **self-validates**
the signature. RGW must reach Lightstack on `:35357`; if Lightstack uses an internal-CA TLS,
RGW must trust that CA.

## Tenant usage

```bash
openstack ec2 credentials create          # prints access + secret (project-scoped)
export AWS_ACCESS_KEY_ID=<access> AWS_SECRET_ACCESS_KEY=<secret> AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url https://<rgw> s3 mb s3://mybucket
aws --endpoint-url https://<rgw> s3 cp ./file s3://mybucket/
openstack ec2 credentials list
openstack ec2 credentials delete <access>  # revocation — subsequent requests are rejected
```

## Isolation & catalog

- Each project has a **private bucket namespace** (implicit tenants): two projects
  may reuse a bucket name, and cross-tenant access requires explicit
  `tenant:bucket` addressing plus permission.
- Lightstack advertises the RGW endpoint under the **`s3`** service type (not the Swift
  `object-store` type — Lightstack exposes no Swift API). S3 clients configure the
  endpoint directly; the catalog entry is for discovery.

## Security notes

- The S3 **secret is stored recoverably** (in `credentials.blob`), because
  `/v3/s3tokens` must recompute the HMAC signature — this mirrors OpenStack
  Keystone's EC2-credential storage. Protect DB access accordingly; secrets are
  never logged.
- `s3tokens` / `ec2tokens` are **public and fail-closed**: any lookup or signature
  failure returns `401` (the signature is the credential; no admin token needed).
  The reason is deliberately masked in the response (matching Keystone, so the
  endpoint never confirms *why* a request failed) but logged server-side with the
  client IP and access key, so probing is visible in the Lightstack log.
- **Unauthenticated exposure.** Because they cannot sit behind the auth middleware,
  these two endpoints accept unauthenticated POSTs and do one indexed DB lookup plus
  an HMAC verification per request, and **lightstack has no rate limiting today** (the
  `rate_limiting` block in `config/lightstack.yaml` is not yet wired to any middleware —
  ADR-002). Keep the RGW↔Keystone path on a trusted network, or front `:35357` with
  a proxy/ingress request limit if it is reachable more widely.
  When rate limiting does land, these endpoints must be limited **per client IP**
  (the `auth_rate` class), never per project: the request carries no token, so a
  project-keyed limiter would put all RGW traffic in one shared bucket and let a
  single noisy client lock out every tenant.
- `s3_insecure_skip_verify` defaults to `false` and is test-only.

## Known limitations

- **Per-node image-cache eviction** uses a cluster-wide refcount
  (`instances.image_id` ∪ `volumes.source_image_id`) — exact on a single node,
  and safely conservative on multi-node (it never evicts a base still in use).
  Precise per-node eviction would need a `host` column on `instances` (future work).
- **Concurrent boots of the same image** are serialized per image id (in-process
  lock) so the base is fetched once, atomically.
- **Swift API / Horizon Swift UI** are out of scope (tracked in lightstack#15); tenant
  access is S3-only.

## Validate

Use `deployments/rook/setup-rook-rgw.sh` + [testing/rook-rgw-fixture.md](./testing/rook-rgw-fixture.md)
for a real RGW fixture, and the CI `ceph/demo` integration test.
