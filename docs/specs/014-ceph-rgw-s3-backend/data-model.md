# Phase 1 Data Model

Entities and schema deltas. One reversible migration (`062`); the rest are behavior changes on existing tables.

## Existing tables touched

### `credentials` (migration 041) — EC2 credential storage
Current columns: `id`, `user_id` (FK→users), `project_id` (FK→projects, nullable), `type`, `blob` (TEXT), `created_at`, `updated_at`.

**Delta (migration 062):** add `access_key VARCHAR(255) UNIQUE` (nullable) + supporting index for O(1) lookup by access key.

An **EC2 credential** row:
- `type = 'ec2'`
- `access_key = <access>` (indexed, unique)
- `blob = {"access": "...", "secret": "...", "user_id": "...", "tenant_id": "<project_id>"}` (JSON string; matches OpenStack shape)
- `user_id`, `project_id` set for scoping (Constitution V)

Rules & lifecycle:
- **Secret stored recoverably** (plaintext in `blob`) — required to recompute the S3 HMAC signature (NOT bcrypt; see plan Complexity Tracking).
- Created via the EC2-credentials API (`contracts/ec2-credentials.md`); **revoked by deleting the row** (no server-side token store — `RevokeToken` is a no-op).
- `access_key` globally unique (RGW presents only the access key to `s3tokens`).

### `images` (migration 001) — checksum now populated
Relevant columns: `size_bytes`, `disk_format`, `checksum VARCHAR(255)` (exists, **currently never written**), `status`, `cached_at` (migration 046, Glance-side cache — unrelated to node bases).

**Delta (no schema change):** write `checksum` = MD5 over the **normalized raw** bytes at upload (Glance-conventional). If `os_hash_algo`/`os_hash_value` columns exist, also populate (SHA-512); otherwise defer. `EnsureLocalBase` verifies a fetched base against `checksum` (FR-017).

### `services` / `endpoints` (migration 040) — `s3` catalog
**Delta (migration 062):** insert `services(type='s3', name='rgw', enabled=true)` and `endpoints(service_id→that, interface='public', region=<region>, url=<RGW S3 endpoint>, enabled=true)`. Mirror in `buildHardcodedCatalog` fallback. (FR-009)

### `instances` (migration 001) — cache-eviction refcount source
`image_id UUID` (existing). The reclamation refcount = `instances.image_id` ∪ `volumes.source_image_id` (the predicate Glance `DeleteImage` already uses). **No `host` column** → refcount is cluster-wide (exact single-node; conservative multi-node). Per-node eviction (future) needs a `host` column.

## New (non-DB) entities

### Local image cache base — filesystem
- Path: `/var/lib/lightstack/images/image-<id>.raw` (the `LocalImagePath` single-source-of-truth).
- Producer: Glance local upload, **or** `storage.EnsureLocalBase` lazy fetch (S3 → normalize → verify).
- Consumer: Nova `StageOverlay` (qcow2 overlay backing file).
- Concurrency: created via temp-file + atomic `os.Rename`, guarded by a per-image in-process lock.
- Reclamation: deleted when refcount = 0.

### s3token validation exchange — transient (not persisted)
Request from RGW → recompute signature → response with resolved identity. Shape in `contracts/s3tokens.md`. Nothing stored.

## Key relationships

```
users 1───* credentials(type=ec2, access_key)      # who owns the S3 key
projects 1───* credentials                          # tenant scope → RGW tenant namespace
images 1───* instances(image_id)                    # refcount pins the local base
images 1── checksum                                 # integrity gate for EnsureLocalBase
services(type=s3) 1───1 endpoints(public, RGW url)  # catalog discovery
```
