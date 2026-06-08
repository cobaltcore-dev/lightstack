# Tasks: SPEC-007 Helm Prerequisites — Admin-Password Bootstrap + Configurable Catalog Scheme

**Input**: Design documents from `docs/specs/007-helm-prereqs-bootstrap-catalog/`
**Prerequisites**: plan.md, spec.md, research.md (R1–R8), data-model.md, contracts/, quickstart.md

**Tests**: REQUIRED — Constitution Article III (Test-First, NON-NEGOTIABLE). Write tests first, confirm they FAIL (RED), then implement (GREEN).

**Organization**: Tasks grouped by user story. US1 (WS-2, P1) and US2 (WS-3, P2) touch disjoint files and are independently implementable/testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1 = admin-password bootstrap (WS-2); US2 = configurable catalog scheme (WS-3)

## Path Conventions

Single Go binary (modular monolith): `internal/`, `cmd/o3k/`, `test/` at repo root (per plan.md Structure Decision).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Surface the configuration knobs; no new packages or dependencies.

- [ ] T001 [P] Document the configuration surface: add `O3K_ENDPOINT_SCHEME` (`http`|`https`, default `http`) to `config/o3k.yaml` comments and `.env.example`, and confirm `keystone.admin_user` / `keystone.admin_password` are present and commented in `config/o3k.yaml` (fields already exist at `internal/common/config.go:48-49`).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Test fixtures both stories' DB-backed tests depend on.

**⚠️ CRITICAL**: Must complete before the DB-backed test tasks (T003, T004, T010) can run.

- [ ] T002 Add a test-DB harness in `internal/keystone/maindb_test.go` (new): a `TestMain` that connects to `O3K_TEST_DB_URL`, runs `database.MigrateUp`, exposes the seeded DB (admin `…0001`/bcrypt('secret'), `Default` domain `…0100`, `default` project `…0002`, `admin` role `…0003`, seeded `endpoints`) to package tests, and `t.Skip`s every DB-backed test when `O3K_TEST_DB_URL` is unset. (Verified: no keystone `_test.go` or DB helper exists yet — this is new; mirrors the integration suite's real-Postgres approach per Constitution "real dependencies over mocks".)

**Checkpoint**: Foundation ready — US1 and US2 can proceed (in parallel if staffed).

---

## Phase 3: User Story 1 — Admin-password bootstrap (Priority: P1) 🎯 MVP

**Goal**: At startup, provision the admin account from `keystone.admin_password` as a first-boot seed so no deployment ships reachable with `admin/secret` when a password is configured.

**Independent Test**: Configure `admin_password`, start O3K, `openstack token issue` succeeds with the configured password and fails with `secret`. Fully testable without WS-3.

### Tests for User Story 1 (write first — confirm RED) ⚠️

- [ ] T003 [P] [US1] Unit tests for `BootstrapAdmin` in `internal/keystone/bootstrap_test.go` covering: (a) seeded admin still `secret` → configured password applied, `secret` rejected (FR-001/002); (b) idempotent restart when stored already verifies configured → no-op (FR-003); (c) `admin_password: secret` configured → no-op write-once (R2 precedence); (d) operator/API-rotated non-default password NOT overwritten (FR-002 no-clobber); (e) custom `admin_user` → new user created with `domain_id=…0100` + admin role on `default` project (`…0002`/`…0003`) + seeded `admin` set `enabled=false` (FR-004/004a/004b); (f) no password → `WARN` log, default retained, no crash (FR-006); (g) password value never appears in emitted logs (FR-006b/SC-008); **(h) project isolation preserved — no other user / role_assignment / project row is created or altered by any path (FR-007, Constitution V); (i) an arbitrary non-default password (no min-length/complexity/denylist) is accepted verbatim after trimming (FR-006a).**
- [ ] T004 [P] [US1] Concurrency test in `internal/keystone/bootstrap_concurrency_test.go` (new): run N `BootstrapAdmin` calls concurrently against one seeded test DB; assert exactly one admin row, the configured password wins, and no duplicate-key error or failure (FR-008a, SC-009).
- [ ] T005 [P] [US1] Integration test `test/spec007_admin_bootstrap_test.sh` (bash + openstack CLI): configured password authenticates and `secret` is rejected; custom-`admin_user` variant (custom user works, `admin/secret` rejected); no-password run emits the `WARN` and still boots (quickstart §WS-2 A/C/D).

### Implementation for User Story 1

- [ ] T006 [US1] Implement `BootstrapAdmin(ctx, cfg common.KeystoneConfig)` in `internal/keystone/bootstrap.go` (new): trim password and treat empty/whitespace as unsupplied (FR-006/006a); resolve admin row and apply R2 precedence (already-configured → no-op; default `secret` → write configured; else leave); default-name path uses optimistic `UPDATE … WHERE id=…0001 AND password_hash=<observed>`; custom-name path uses `INSERT … ON CONFLICT (name) DO NOTHING` with `domain_id=…0100`, `INSERT role_assignments(…,…0002,…0003) ON CONFLICT DO NOTHING`, and `UPDATE users SET enabled=false WHERE name='admin'` (R3/R4); reuse `HashPassword` (bcrypt, FR-005); emit one structured log on create/seed without the password (FR-006b) and a `WARN` when unset (FR-006); MUST NOT alter any other identity (FR-007). (depends on T003, T004, T005 being RED)
- [ ] T007 [US1] Wire `keystone.BootstrapAdmin(ctx, cfg.Keystone)` into `cmd/o3k/main.go` immediately after `database.MigrateUp` and before service wiring (R1); fail-soft (log, do not crash) consistent with FR-006. (depends on T006)

**Checkpoint**: US1 fully functional and independently testable — MVP deliverable.

---

## Phase 4: User Story 2 — Configurable catalog scheme + self-links (Priority: P2)

**Goal**: `O3K_ENDPOINT_SCHEME=https` makes the advertised catalog **and** resource self-links use `https://<host>:<port>`; default/invalid stays `http`. Catalog shape unchanged.

**Independent Test**: With `O3K_ENDPOINT_SCHEME=https`, `openstack catalog list` URLs and resource self-links are `https://`; unset/invalid → `http://`; catalog is identical except the scheme. Testable independently of WS-2.

### Tests for User Story 2 (write first — confirm RED) ⚠️

- [ ] T008 [P] [US2] Unit tests for the scheme helper in `internal/common/scheme_test.go`: `EndpointScheme()` returns `http` when unset/empty, `https` when set, is case-insensitive + trimmed, and on an invalid value warns once (`WARN`) and returns `http` (FR-009/011/014); `ForceScheme(url, scheme)` rewrites a leading `http://`/`https://`.
- [ ] T009 [P] [US2] Unit test in `internal/common/baseurl_test.go`: `BaseURL` returns `https://…` when `O3K_ENDPOINT_SCHEME=https` (with `O3K_ENDPOINT_HOST` set) and `http://…` by default (FR-013a).
- [ ] T010 [P] [US2] Unit tests in `internal/keystone/auth_catalog_test.go`: `BuildServiceCatalog` (DB path) and `buildHardcodedCatalog` (fallback) apply the scheme to every endpoint URL; structure identical between `http` and `https` except the scheme (FR-010/011/013, SC-006).
- [ ] T011 [P] [US2] Integration test `test/spec007_catalog_scheme_test.sh`: `O3K_ENDPOINT_SCHEME=https` → all `openstack catalog list` URLs and a resource self-link (e.g. `server list --long`) are `https://`; unset and invalid (`ftp`) → `http://` with one `WARN` (quickstart §WS-3).

### Implementation for User Story 2

- [ ] T012 [US2] Implement the shared helper in `internal/common/scheme.go` (new): `EndpointScheme()` (read `O3K_ENDPOINT_SCHEME`, trim + lowercase, default `http`, accept `http`/`https`, else `sync.Once` `WARN` + `http`) and `ForceScheme(url, scheme string) string` (R5). (depends on T008–T011 being RED)
- [ ] T013 [P] [US2] Apply scheme in `internal/keystone/auth.go`: call `common.ForceScheme(url, common.EndpointScheme())` in `substituteURLTemplates` (after the existing `O3K_ENDPOINT_HOST` rewrite, `auth.go:642`) and in `buildHardcodedCatalog` so every advertised URL honors the scheme (FR-010/012/013). (depends on T012)
- [ ] T014 [P] [US2] Apply scheme in `internal/common/baseurl.go`: replace the hardcoded `http://` (line 13) with `common.EndpointScheme()` / `ForceScheme` so self-links and pagination honor the scheme (FR-013a). (depends on T012)

**Checkpoint**: US1 and US2 both work independently.

---

## Phase 5: Polish & Cross-Cutting Concerns

- [ ] T015 [P] Run `quickstart.md` verification end-to-end for WS-2 and WS-3 (all cases A–D / http-https-invalid).
- [ ] T016 Run the SPEC-000 contract tests with both workstreams enabled and confirm 100% pass + existing OpenStack CLI/Terraform flows unchanged (FR-015, FR-016, SC-007).
- [ ] T017 [P] Assert the admin password value never appears in any log output (SC-008) — add a log-capture assertion in `bootstrap_test.go` and a grep gate in the integration test.
- [ ] T018 [P] Update docs: `docs/KUBERNETES_DEPLOYMENT.md` (note `O3K_ENDPOINT_SCHEME` + the **port-based TLS** requirement, Option B) and `docs/CONFIGURATION.md` (`admin_user`/`admin_password`, `O3K_ENDPOINT_SCHEME`).
- [ ] T019 Run `go test ./...` and `make lint` — no regressions.

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (T001)**: no dependencies.
- **Foundational (T002)**: after Setup — blocks the DB-backed test tasks (T003, T004, T010).
- **US1 (T003–T007)** and **US2 (T008–T014)**: after Foundational. Disjoint files → the two stories can run fully in parallel.
- **Polish (T015–T019)**: after US1 + US2 complete.

### Within US1

- Tests T003, T004, T005 (RED) → implementation T006 → wiring T007.

### Within US2

- Tests T008–T011 (RED) → helper T012 → T013 and T014 (both depend on T012; different files, so [P] with each other).

### Cross-story

- US1 ↔ US2 fully independent (`bootstrap.go`/`main.go` vs `internal/common` + `auth.go`). No shared files.

### Parallel opportunities

- T003 ∥ T004 ∥ T005 (US1 tests — `bootstrap_test.go`, `bootstrap_concurrency_test.go`, `test/*.sh`: distinct files).
- T008 ∥ T009 ∥ T010 ∥ T011 (US2 tests, distinct files).
- T013 ∥ T014 (after T012).
- Entire US1 phase ∥ entire US2 phase once T002 is done.
- T015 ∥ T017 ∥ T018 in Polish.

---

## Parallel Example: after Foundational (T002)

```bash
# US1 and US2 in parallel — write the RED tests first:
Task: "US1 unit tests in internal/keystone/bootstrap_test.go"               # T003
Task: "US1 concurrency test in internal/keystone/bootstrap_concurrency_test.go"  # T004
Task: "US1 integration test in test/spec007_admin_bootstrap_test.sh"        # T005
Task: "US2 scheme-helper tests in internal/common/scheme_test.go"           # T008
Task: "US2 BaseURL test in internal/common/baseurl_test.go"                 # T009
Task: "US2 catalog tests in internal/keystone/auth_catalog_test.go"         # T010
```

---

## Implementation Strategy

### MVP First (User Story 1 only)

1. Phase 1 Setup (T001) → Phase 2 Foundational (T002).
2. Phase 3 US1 (T003–T007): tests RED (incl. concurrency + isolation) → implement → wire.
3. **STOP & VALIDATE**: no deployment reachable with `admin/secret` when a password is configured (SC-001/002), concurrent start converges (SC-009). Ship.

### Incremental delivery

1. Setup + Foundational → foundation ready.
2. US1 (WS-2) → test independently → MVP (the P1 security gate that unblocks the chart).
3. US2 (WS-3) → test independently → enables the chart's optional TLS path.
4. Polish → contract tests + docs.

---

## Notes

- [P] = different files, no dependency on an incomplete task.
- Confirm every test FAILS before implementing it (Constitution Article III).
- No DB migration is added; the seed is retained (R7).
- WS-3 advertises `https://<host>:<port>` — works only under **port-based TLS** (the SPEC-008 chart's responsibility; out of scope here).
- Commit after each task or logical group; stop at either checkpoint to validate a story independently.
