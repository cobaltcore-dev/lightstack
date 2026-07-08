# Implementation Plan: Ceph RGW (S3) — Glance backend & tenant object storage

**Branch**: `feature/480-o3k-validate-ceph-rgw-s3-backend-glance-images` | **Date**: 2026-07-08 | **Spec**: [spec.md](./spec.md)
**Input**: `docs/specs/014-ceph-rgw-s3-backend/spec.md`

## Summary

Deliver two capabilities against Ceph RGW, sharing one fixture and test harness:

- **A. Glance image backend (validate + implement).** The S3 data round-trip already exists (`pkg/storage/image_store.go`). This feature (1) validates it end-to-end against real RGW, (2) reworks the operation timeout so large images stream to completion (FR-014), (3) adds HTTPS/custom-CA trust (FR-015), (4) implements **lazy fetch-on-boot** so a VM boots from an S3-backed image on any node (FR-013), gated by a **checksum-on-upload + verify-on-fetch** integrity chain (FR-002/FR-017), with **node-local cache reclamation** (FR-016).
- **B. Tenant object storage (new).** Add an OpenStack **EC2-credentials API** and a **public, self-validating `/v3/s3tokens`** endpoint to o3k's Keystone so Ceph RGW (configured with `rgw s3 auth use keystone` + `implicit tenants=s3`) authenticates tenant S3 requests against o3k, with per-project bucket namespaces (FR-005…009).

Technical approach is grounded in the current code (see [research.md](./research.md)): reuse the existing `credentials` table for EC2 creds (add an `access_key` lookup column), recompute S3 signatures server-side (secret stored recoverably, as real Keystone does), thread a `*storage.ImageStore` into Nova for the boot fetch, and inject a custom `http.Client` (CA pool + transport timeouts) into the aws-sdk client.

## Technical Context

**Language/Version**: Go 1.26 (repo), stdlib + minimal deps
**Primary Dependencies**: Gin (HTTP), pgx (PostgreSQL), aws-sdk-go-v2 (S3 — already vendored: `aws-sdk-go-v2 v1.42.0`, `service/s3 v1.103.3`), `crypto/*` + `crypto/hmac`/`sha256` (S3 SigV4 verification — stdlib), `golang-migrate` (migrations)
**Storage**: PostgreSQL (existing `credentials`, `services`, `endpoints`, `images`, `instances` tables; one new migration `062`) + node-local `/var/lib/o3k/images` (image cache bases) + Ceph RGW (external, S3)
**Testing**: Go unit tests (`pkg/storage`, `internal/keystone`); gophercloud/openstackclient contract tests (`test/contract`); bash integration test against a containerized RGW in CI (`ceph/demo`); manual Rook fixture (`deployments/rook/setup-rook-rgw.sh`) for deployment parity
**Target Platform**: Linux (real mode: libvirt/KVM); stub mode elsewhere
**Project Type**: Single Go binary (`cmd/o3k`) — Constitution VI
**Performance Goals**: image transfers stream to completion for images ≥10 GB (FR-014); credential issue + s3token validation are simple DB lookups (<100ms); no throughput target (Constitution II synchronous)
**Constraints**: fail-fast on **connectivity/first-byte** only (Constitution III), NOT on transfer duration (FR-014 — see Constitution Check); no message queues / async workers (Constitution II); reversible migrations
**Scale/Scope**: o3k is effectively single-node today (all disk state under `/var/lib/o3k`; `instances` has no `host` column) — lazy-fetch enables multi-node image *distribution*, but precise per-node cache eviction needs a future `host` column (see research.md §7)

## Constitution Check

*GATE: evaluated pre-Phase-0 and re-checked post-Phase-1. Result: **PASS** with 2 documented justifications.*

| Principle | Status | Notes |
|-----------|--------|-------|
| I. Flamingo API compatibility (NON-NEGOTIABLE) | PASS | EC2-credentials API = standard `openstack ec2 credentials create` surface; `/v3/s3tokens` matches Keystone's public S3-validation endpoint; catalog `s3` service type. Contract tests with openstackclient/gophercloud required (Test-First). |
| II. Synchronous Operations | PASS | Credential issue, s3token validation, and lazy fetch-on-boot are synchronous within the request/create; no queues, no state machines. |
| III. Fail-Fast Design | PASS *(justified)* | See Complexity Tracking: fail-fast is preserved for **connect/first-byte** (short transport timeouts, <1s configurable); only the **data-stream duration** is uncapped (FR-014), because a 1s cap on a multi-GB image transfer is incorrect, not resilient. |
| IV. Multi-Mode Architecture | PASS | S3 backend keeps its stub mode; EC2-creds/`s3tokens` are DB-backed Keystone endpoints that work without RGW (dev); lazy-fetch runs only in real libvirt mode; RGW is the external real-mode dependency. |
| V. Project Isolation | PASS | EC2 credentials are project-scoped; tenant buckets are per-project namespaces (RGW `implicit tenants=s3`); Glance image data stays operator-owned with API-level project scoping. |
| VI. Single Binary | PASS | All new code compiles into `cmd/o3k`; no new services/processes. |
| Tech constraints (Go/PG/migrations/no-forbidden-patterns) | PASS *(justified)* | One reversible migration (`062`). S3 secret stored **recoverably** (not bcrypt) — justified below; this is required for HMAC signature verification and mirrors OpenStack Keystone EC2 credentials. |

## Project Structure

### Documentation (this feature)

```text
docs/specs/014-ceph-rgw-s3-backend/
├── spec.md              # feature spec (done)
├── plan.md              # this file
├── research.md          # Phase 0 — decisions for all deferred/unknown items
├── data-model.md        # Phase 1 — entities + schema deltas
├── quickstart.md        # Phase 1 — configure + validate walkthrough
├── contracts/           # Phase 1 — API + wireup contracts
│   ├── ec2-credentials.md
│   ├── s3tokens.md
│   └── rgw-wireup.md
├── checklists/requirements.md
└── tasks.md             # Phase 2 (/cl.tasks — NOT created here)
```

### Source Code (repository root) — files this feature touches

```text
internal/keystone/
├── ec2_credentials.go     # NEW — EC2-cred API: POST/GET/DELETE /v3/users/:id/credentials/OS-EC2[/:access]
├── s3tokens.go            # NEW — public POST /v3/s3tokens (+ /v3/ec2tokens): recompute S3 sig, return user/project/roles
├── handlers.go            # register EC2 routes on mgmt group; s3tokens on public v3 group; add s3 catalog fallback
└── auth.go                # buildHardcodedCatalog: optional `s3` fallback entry

internal/common/config.go  # GlanceConfig: + s3_ca_bundle, s3_insecure_skip_verify (+ optional scheme); env overrides
pkg/storage/image_store.go # custom http.Client (CA pool + transport timeouts); split connect/first-byte vs stream;
                           #   checksum-on-upload; EnsureLocalBase(ctx,id) = download+normalize+verify (keeps normalizeToRaw private)
internal/glance/images.go  # write images.checksum on upload; thread new config through NewService
internal/nova/handlers.go  # boot branch (~588): ensure local raw base via storage before StageOverlay
internal/nova/service      # NewService gains storage config → builds *storage.ImageStore on the Service struct
cmd/o3k/main.go            # thread new Glance S3 config + Nova storage config

migrations/062_ec2_s3_catalog.{up,down}.sql  # credentials.access_key column + unique index; `s3` service + RGW endpoint seed

charts/o3k/                # values + _helpers.tpl: render glance s3_endpoint/s3_bucket/s3_region/ca-bundle; RGW wireup docs

deployments/rook/setup-rook-rgw.sh   # (done) + add s3tokens/implicit-tenants wireup for capability B
docs/S3_CONFIGURATION.md, docs/STORAGE_MODES.md, docs/testing/rook-rgw-fixture.md  # AC3 docs
test/contract/keystone/    # EC2-cred + s3tokens contract tests
test/*.sh + .github/workflows/ci.yml   # ceph/demo integration job (FR-010)
```

**Structure Decision**: Single-binary layout (Constitution VI). New Keystone endpoints live in `internal/keystone`; the boot-fetch/normalize/verify logic lives in `pkg/storage` (a new `EnsureLocalBase` helper) so `normalizeToRaw`/`detectDiskFormat` stay unexported and Nova gains a thin `*storage.ImageStore` seam mirroring Glance. No new top-level projects.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Transfer duration not bounded by the 1s fail-fast timeout (Constitution III) | A fixed ≤1s timeout cancels any multi-GB image transfer mid-stream (this is the actual bug behind the current 5s cap). FR-014 requires streaming to completion (validated ≥10 GB). | Keeping a fixed total-op timeout would make S3/RGW image storage unusable for real images. Fail-fast is preserved where it belongs: connect + first-byte (short transport timeouts). |
| S3 credential secret stored recoverably (not bcrypt, unlike passwords / application-credentials) | `/v3/s3tokens` must recompute the S3 HMAC signature from the secret to validate RGW's request — a one-way hash cannot sign. This is exactly how OpenStack Keystone stores EC2-credential secrets. | bcrypt (used for user passwords + app-cred secrets) is irreversible → cannot verify S3 signatures. Mitigation: secret confined to the `credentials.blob`, DB-access-controlled; documented in data-model.md. |

*Post-Phase-1 re-check: no new violations introduced by the design; both entries above are intrinsic to the feature and justified.*
