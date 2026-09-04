# Tasks: Ceph RGW (S3) — Glance backend & tenant object storage

**Input**: Design documents from `docs/specs/014-ceph-rgw-s3-backend/`
**Prerequisites**: plan.md, spec.md (required); research.md, data-model.md, contracts/, quickstart.md

**Tests**: INCLUDED — the constitution mandates Test-First (Article III, NON-NEGOTIABLE) and contract tests with real OpenStack clients (Article I). Write each story's tests first and confirm they FAIL before implementing. Real-mode/host tests are authored now and executed on the Linux/k3s host; unit + contract + the `ceph/demo` integration test run in CI.

**Organization**: by user story (US1=P1 Glance backend, US2=P2 tenant object storage, US3=P3 validated/documented/reproducible). Each task cites the FR(s)/SC(s) it serves for traceability.

## Format: `[ID] [P?] [Story] Description with file path`

- **[P]**: parallelizable (different files, no dependency on an incomplete task)
- **[Story]**: US1 / US2 / US3 (story phases only)

---

## Phase 1: Setup (Shared Infrastructure)

- [X] T001 [P] Add S3/TLS config fields to `GlanceConfig` (`s3_ca_bundle`, `s3_insecure_skip_verify`, optional `s3_connect_timeout`) + `lightstack_*` env overrides in `LoadConfig`, in `internal/common/config.go` (FR-003, FR-015)
- [X] T002 Introduce an S3 options struct in `pkg/storage/image_store.go` and thread it through `storage.NewImageStore`, `glance.NewService` (`internal/glance/images.go`), and `cmd/lightstack/main.go` — replaces the positional S3 args (FR-003; depends on T001)

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ Blocks the automated tests in all stories.**

- [X] T003 Add a real RGW test fixture: `ceph/demo` all-in-one service + a bucket/RGW-user bootstrap step, in `deployments/docker-compose.rgw-test.yml` (reused by US1/US2/US3 CI tests) (FR-010)

**Checkpoint**: config plumbing + a real RGW to test against are ready.

---

## Phase 3: User Story 1 - Operator runs Glance on Ceph RGW (Priority: P1) 🎯 MVP

**Goal**: Lightstack stores/serves image data in RGW and boots VMs from RGW-backed images (pure-S3, multi-node), with integrity + streaming + TLS.

**Independent Test**: configure `storage_mode: s3` → RGW; `openstack image create`/`image save` checksum-matches; boot a VM on a node with no local base (lazy fetch) → `ACTIVE`.

### Tests for User Story 1 (write first, confirm RED)

- [X] T004 [P] [US1] Integration test: image upload→download checksum roundtrip against the `ceph/demo` RGW (small runtime-generated image) — realized (with T008) in the unified `test/rgw_integration_test.sh` driver that T026's CI job runs; verifies the object lands under `images/image-<id>.raw` via `radosgw-admin` and the round-trip is byte-identical (FR-001, FR-002, SC-001)
- [X] T005 [P] [US1] Host test (real mode): boot a VM from an RGW-backed image with NO local base (lazy fetch) + a corrupt-checksum → `ERROR` case, in `test/ws0_s3_boot_test.sh` (FR-013, SC-009, SC-010, SC-012) — **HOST-VALIDATED 2026-07-08** on libvirt (real Nova/Neutron) + ceph/demo RGW: T005.1 lazy fetch → ACTIVE, T005.2 corrupt object → ERROR
- [X] T006 [P] [US1] Unit tests: `EnsureLocalBase` (fetch+normalize+verify+atomic-rename+lock) and checksum-on-upload, in `pkg/storage/image_store_test.go` (FR-013, FR-016, FR-017)
- [X] T007 [P] [US1] Streaming-not-capped **unit** test (FR-014/SC-002): a **throttled reader/writer** delivering a payload slower than the connect/first-byte budget asserts the transfer completes (proves no total-duration cap), in `pkg/storage/stream_timeout_test.go`
- [X] T008 [P] [US1] Streaming **integration realism** (FR-014/SC-002): a **runtime-generated** large image — `truncate -s ${LIGHTSTACK_TEST_IMAGE_SIZE:-1G}` (sparse, instant) — written to an `mktemp` path **outside the repo tree** (never committed, never downloaded), pushed end-to-end through the real RGW (`openstack image create --file` → `image save` → md5 match), asserting a large image completes and cleaning up after — realized in the unified `test/rgw_integration_test.sh` driver (FR-014, SC-002)
- [X] T009 [P] [US1] Cache-eviction test (FR-016/SC-011): base present after boot; reclaimed when the last referencing instance is deleted; base pinned while another instance still references the image, in `test/cache_eviction_test.sh` — **HOST-VALIDATED 2026-07-08** on libvirt: base pinned while a 2nd instance referenced it, reclaimed only after the last delete

### Implementation for User Story 1

- [X] T010 [US1] Build a custom aws http client (`awshttp.NewBuildableClient` + `tls.Config{RootCAs, InsecureSkipVerify}`) from the S3 options and pass via `config.WithHTTPClient` in `initS3Client`, in `pkg/storage/image_store.go` (FR-015)
- [X] T011 [US1] Rework the timeout: short connect/first-byte deadline (transport `ResponseHeaderTimeout`/dial) and stream `GetObject`/`PutObject` bodies under the request context (remove the total-duration `s.timeout` around body copies), in `pkg/storage/image_store.go` (FR-014)
- [X] T012 [US1] Fix `imageExistsS3` to distinguish 404 (not-found) from real errors instead of swallowing all as not-found, in `pkg/storage/image_store.go` (FR-004)
- [X] T013 [US1] Compute a checksum (MD5 over the normalized raw) on upload and persist `images.checksum`, in `internal/glance/images.go` (`UploadImageData`) + the raw-normalize path in `pkg/storage/image_store.go` (FR-002; prereq for FR-017)
- [X] T014 [US1] Add `storage.EnsureLocalBase(ctx, imageID, expectedChecksum)`: return path if present, else download `images/image-<id>.raw` → `normalizeToRaw` → verify checksum → atomic rename; serialize per image id, in `pkg/storage/image_store.go` (FR-013, FR-017)
- [X] T015 [US1] Thread storage config into `nova.NewService` and build a `*storage.ImageStore` on the Nova `Service` struct, in `internal/nova/handlers.go` + `cmd/lightstack/main.go` (FR-013)
- [X] T016 [US1] In the real-mode boot branch (~`internal/nova/handlers.go:588`), call `EnsureLocalBase` before `hypervisor.StageOverlay`; on fetch/checksum/normalize failure mark the instance `ERROR` (fail-fast), in `internal/nova/handlers.go` (FR-013)
- [X] T017 [US1] Cache eviction: reclaim `/var/lib/lightstack/images/image-<id>.raw` when the refcount is 0 (`instances.image_id` ∪ `volumes.source_image_id`), on instance delete and/or a periodic sweep, in `internal/nova/handlers.go` (+ helper in `pkg/storage`) (FR-016)

**Checkpoint**: US1 fully functional — RGW-backed image round-trip + boot works, streams large images, TLS-capable, integrity-checked. **MVP.**

---

## Phase 4: User Story 2 - Tenant stores objects in Ceph RGW via Lightstack identity (Priority: P2)

**Goal**: tenants mint long-lived S3 creds from lightstack; RGW authenticates their S3 requests against lightstack; per-project isolation.

**Independent Test**: `openstack ec2 credentials create` → S3 client put/get vs RGW succeeds; invalid/revoked creds rejected; another project can't reach the bucket.

- [X] T018 [US2] Migration `062` (paired up/down): add unique `credentials.access_key` column + index; seed an `s3` service + RGW `endpoints` row, in `migrations/062_ec2_s3_catalog.up.sql` / `.down.sql` (FR-005, FR-009)

### Tests for User Story 2 (write first, confirm RED)

- [X] T019 [P] [US2] Contract test: `openstack ec2 credentials create/list/show/delete` round-trip, in `test/contract/keystone/ec2_credentials_test.go` (FR-005, FR-007, SC-003)
- [X] T020 [P] [US2] Integration test: a minted key signs an S3 (boto3/SigV4) request that RGW validates via `/v3/s3tokens`; tampered/unknown/deleted → denied; cross-project denied, in `test/tenant_s3_rgw_test.sh` (FR-006, FR-007, FR-008, SC-004, SC-005) — 6/6 green against real ceph/demo RGW

### Implementation for User Story 2

- [X] T021 [P] [US2] EC2-credentials API handlers (`POST/GET/DELETE /v3/users/:id/credentials/OS-EC2[/:access]`) storing `type=ec2` + `access_key` + JSON blob, in `internal/keystone/ec2_credentials.go` (FR-005, FR-007; contracts/ec2-credentials.md)
- [X] T022 [P] [US2] Public `/v3/s3tokens` (+ `/v3/ec2tokens`): look up by `access_key`, recompute SigV4 (fallback SigV2) with `crypto/hmac`+`sha256`, constant-time compare, return user/project/roles; fail-closed 401, in `internal/keystone/s3tokens.go` (FR-006, FR-007, FR-008; contracts/s3tokens.md)
- [X] T023 [US2] Register routes — EC2 creds on the authenticated `mgmt` group, `s3tokens`/`ec2tokens` on the public `v3` group, in `internal/keystone/handlers.go` (FR-005, FR-006; depends on T021, T022)
- [X] T024 [P] [US2] Add an `s3` entry to `buildHardcodedCatalog` fallback, in `internal/keystone/auth.go` (FR-009)
- [X] T025 [US2] RGW↔lightstack wireup: `rgw s3 auth use keystone=true`, `rgw keystone url`, `api version=3`, `implicit tenants=s3`, `accepted roles`, **plus** `rgw keystone admin_user/password/project/domain` (RGW must fetch its own token before validating), empty `rgw_dns_name` (path-style for any client host), and `rgw_keystone_token_cache_size=0` (prompt revocation) — done via the `deployments/rgw-keystone-entrypoint.sh` wrapper that `docker-compose.rgw-test.yml` runs. Rook wireup (setup-rook-rgw.sh) is T030 (FR-008; contracts/rgw-wireup.md)

**Checkpoint**: US1 AND US2 both work independently — tenants use RGW S3 through lightstack-issued creds, isolated per project.

---

## Phase 5: User Story 3 - Validated, documented, reproducible RGW integration (Priority: P3)

**Goal**: CI proves both capabilities against a real RGW; RGW quirks + config documented; reproducible fixtures.

**Independent Test**: CI job runs and gates; docs let an operator configure without reading source; runbook stands up a non-degraded RGW.

- [X] T026 [US3] New CI job `rgw-integration-tests`: brings up Lightstack + postgres + `ceph/demo` (deployments/docker-compose.rgw-test.yml), waits for the RGW→Keystone wireup, then runs A (`test/rgw_integration_test.sh`: roundtrip + large stream) and B (`test/tenant_s3_rgw_test.sh`: creds → boto3 S3 → RGW → `/v3/s3tokens` + isolation + revocation); dumps stack logs on failure; gated in status-check. In `.github/workflows/ci.yml` (FR-010, SC-006). Boot-from-RGW fetch = T005 (host, real root)
- [X] T027 [P] [US3] Update `docs/S3_CONFIGURATION.md` + `docs/STORAGE_MODES.md`: validated RGW recipe, quirks (path-style, region/SigV4, bucket pre-create, TLS/CA, streaming timeout, imageExists), and fix the stale `~/.lightstack/images` → `/var/lib/lightstack/images`, in `docs/` (FR-011, SC-001)
- [X] T028 [P] [US3] Add a tenant-object-storage guide (EC2-creds + RGW keystone wireup + `openstack ec2 credentials create` + isolation), in `docs/OBJECT_STORAGE_S3.md` (FR-005, FR-008)
- [X] T029 [P] [US3] Chart: render `glance.s3_endpoint`/`s3_bucket`/`s3_region`/`s3_ca_bundle` in `charts/lightstack/templates/_helpers.tpl`, expose in `charts/lightstack/values.yaml`, and validate in `charts/lightstack/values.schema.json` (FR-003, FR-015)
- [X] T030 [US3] Extend `deployments/rook/setup-rook-rgw.sh` with a `--wire-keystone` mode (injects the RGW→lightstack keystone options — url/api-version/implicit-tenants=s3/accepted-roles/**admin creds**/token-cache=0 — into `rook-config-override`, then restarts the RGW; reachability probe + path-style/IP guidance) and a capability-B section in `docs/testing/rook-rgw-fixture.md`. Options mirror the real-RGW-validated T025 wireup; `bash -n` clean. **HOST-VALIDATED 2026-07-09** on a live Rook cluster (HOST_TESTING.md §9: RGW→lightstack `:35357` reachable + the in-guest capability-B roundtrip that depends on it passed) (FR-012, SC-007, SC-008)

**Checkpoint**: integration is tested in CI, documented, and reproducible.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T031 [P] Unit tests for SigV4/SigV2 verification edge cases and new config parsing, in `internal/keystone/s3tokens_test.go` + `internal/common/config_test.go` (FR-006)
- [X] T032 [P] Security review: EC2 secret confined to `credentials.blob` (documented, DB-access-controlled); confirm `s3_insecure_skip_verify` defaults false and is test-only (FR-015)
- [X] T033 Run the end-to-end host flow on the two-cluster setup (k3s Lightstack real mode + minikube Rook RGW) — **HOST-VALIDATED 2026-07-09** via `HOST_TESTING.md`: capability A (image→RGW→boot VM by lazy fetch, SSH in) **and** capability B (tenant alice mints EC2 creds → **in-guest** `mc` PUT/GET → RGW → Lightstack `/v3/s3tokens` → `IN-VM S3 ROUNDTRIP OK` + revocation). Also validated **T030**'s `--wire-keystone` Rook path live. Run surfaced + captured several env fixes (k3s kubeconfig mode, cross-shell cred handoff, guest→minikube libvirt FORWARD/MASQUERADE, cirros no-sftp/no-bash) and a **separate pre-existing Lightstack identity bug** (GET `/v3/{projects,users,roles}/:id` → 500 on non-UUID; filed) worked around with ID-based lookups (SC-007, SC-008)
- [X] T034 [P] Document the known limitations in `docs/` and spec notes: per-node cache eviction needs a future `instances.host` column (cluster-wide refcount meanwhile); concurrent-fetch is serialized per image (FR-016)

---

## Dependencies & Execution Order

### Phase dependencies
- **Setup (P1)** → no deps.
- **Foundational (P2, T003)** → after Setup; blocks automated story tests.
- **US1 (P3)** → after Foundational. **US2 (P4)** → after Foundational; independent of US1 (different subsystems: keystone vs glance/nova/storage). **US3 (P5)** → after the code it validates (US1/US2), though docs tasks can start earlier.
- **Polish (P6)** → after US1–US3.

### Critical path within US1
T010 → T011 → T012 → T013 → T014 (all touch `image_store.go`, sequential); T015 → T016 (nova/handlers.go); T017 after T016. Tests T004–T009 first (RED).

### Critical path within US2
T018 (migration) → T021 ∥ T022 (different files) → T023 (routes); T024 ∥; T025 after T021–T023. Tests T019 ∥ T020 first.

### Parallel opportunities
- Setup: T001 ∥ (T002 depends on T001).
- US1 tests T004 ∥ T005 ∥ T006 ∥ T007 ∥ T008 ∥ T009 (all different files).
- US2: T021 ∥ T022 ∥ T024; tests T019 ∥ T020.
- US3 docs/chart: T027 ∥ T028 ∥ T029.
- Whole stories US1 ∥ US2 (different subsystems) once Foundational is done.

---

## Parallel Example: User Story 2

```bash
# tests first (RED):
Task: "ec2 credentials contract test in test/contract/keystone/ec2_credentials_test.go"
Task: "s3tokens+isolation+revocation test in test/tenant_s3_rgw_test.sh"
# then implementation in parallel (different files):
Task: "EC2-credentials handlers in internal/keystone/ec2_credentials.go"
Task: "s3tokens/ec2tokens endpoint in internal/keystone/s3tokens.go"
Task: "s3 catalog fallback in internal/keystone/auth.go"
```

---

## Implementation Strategy

### MVP (US1 only)
Setup → Foundational → US1 → **stop & validate** (RGW image round-trip + boot). This alone satisfies the original ticket ACs (upload/download) plus boot; deployable.

### Incremental delivery
US1 (images on RGW) → US2 (tenant object storage) → US3 (CI + docs + runbook). Each is independently testable; US1 and US2 can be built in parallel by different developers after Foundational.

### Notes
- Tests fail before implementing (Article III).
- Large-transfer testing = a **throttled reader** unit test (T007, proves no total-duration cap) **plus** a **runtime-generated** large-file integration test (T008, `truncate`/`/dev/urandom` to an `mktemp` path) — no large fixture is committed or downloaded.
- `EnsureLocalBase` keeps `normalizeToRaw`/`detectDiskFormat` unexported (logic lives in `pkg/storage`).
- Migration `062` must ship paired up/down (`ValidateMigrations` enforces equal counts).
- Secret stored recoverably (HMAC verification) — justified in plan.md Complexity Tracking.
- Out of scope (do not add): Swift API/UI (lightstack#15), object quotas, a turnkey provisioning operator, per-node eviction host column.
