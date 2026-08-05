# Tasks: Nova server-detail read-back (Terraform compatibility follow-ups)

**Input**: Design documents from `docs/specs/017-nova-server-detail-readback/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md (all present)

**Tests**: INCLUDED — the project constitution mandates Test-First (Article III) and US3 is a
regression-gate story. Feature tests are written RED before implementation.

**Organization**: Grouped by user story. **File-sharing note**: US1 and US2 both edit the same Nova
handler functions (`CreateServer`, `GetServer`, `ListServersDetail` in `internal/nova/handlers.go`),
so they are independently *testable* but their `handlers.go` tasks are **sequential** (US1 → US2),
not parallel. Only tasks in distinct files carry `[P]`.

## Format: `[ID] [P?] [Story?] Description with file path`

---

## Phase 1: Setup (Shared Infrastructure) — ✅ COMPLETE

**Purpose**: Nova DB test harness (Nova had none; keystone was the only package with DB tests). Built
and verified in the planning session — kept here for traceability.

- [X] T001 [P] Nova DB test harness `TestMain` + `requireDB` (gated by `O3K_TEST_DB_URL`, migrates `../../migrations`; mirrors `internal/keystone/maindb_test.go`) in `internal/nova/maindb_test.go`
- [X] T002 [P] Nova gin/httptest router helper `newNovaTestRouter` (`r.Use(testAuthMW)` + `RegisterRoutes(&r.RouterGroup)`, `NewService("","stub",nil)`), `doReq`, `testAuthHdr`, seeded scope consts, + two harness self-tests (`TestNovaHarness_Version` no-DB, `TestNovaHarness_DB` full-stack) in `internal/nova/testrouter_test.go`

**Checkpoint**: ✅ Harness compiles, `go vet` + `golangci-lint` clean, `TestNovaHarness_Version` passes (no DB), `TestNovaHarness_DB` passes against real Postgres (migrations v62).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Cross-story prerequisites.

The only shared prerequisite is the test harness (Phase 1), which is complete. There is **no
additional foundational work** — US1 and US2 are self-contained slices that share `handlers.go`
functions (implement US1 before US2 on that file). No task here.

**Checkpoint**: Foundation ready — user stories can begin.

---

## Phase 3: User Story 1 - key_name read-back (Priority: P1) 🎯 MVP

**Goal**: Server-detail (show + list-detail) returns `key_name`; a repeated `terraform apply` no longer
churns on the `key_pair` attribute. Unknown keypair at create is rejected (FR-002a).

**Independent Test**: `openstack server create --key-name demo-key …` → `openstack server show` reports
`key_name: "demo-key"`; a server with no keypair reports `key_name: null`; `--key-name nope` → HTTP 400,
no instance created.

### Tests for User Story 1 (write FIRST — must FAIL before implementation) ⚠️

- [X] T003 [P] [US1] Failing unit tests: `GET /v2.1/servers/:id` and `GET /v2.1/servers/detail` return `key_name` (created with a keypair → the name; created without → JSON `null`), using `newNovaTestRouter`/`requireDB`/seeded scope (seed a keypair + use a seeded flavor/image) in `internal/nova/server_detail_keyname_test.go`
- [X] T004 [P] [US1] Failing unit test: `POST /v2.1/servers` with an unknown `key_name` → `400 badRequest` "Invalid key_name provided." and **no** instance row created (FR-002a) in `internal/nova/server_detail_keyname_test.go`

### Implementation for User Story 1

- [X] T005 [P] [US1] Migration up: `ALTER TABLE instances ADD COLUMN IF NOT EXISTS key_name VARCHAR(255);` in `migrations/063_nova_instance_key_name.up.sql`
- [X] T006 [P] [US1] Migration down (`-- Rollback` header, `IF EXISTS`): `ALTER TABLE instances DROP COLUMN IF EXISTS key_name;` in `migrations/063_nova_instance_key_name.down.sql`
- [X] T007 [US1] `CreateServer`: add synchronous keypair validation **before** the `instances` INSERT (in the `~handlers.go:341-424` sync window) — when `req.Server.KeyName != ""`, run `SELECT 1 FROM keypairs WHERE user_id = $1 AND name = $2` (`userID` from `:340`); on `pgx.ErrNoRows` → `common.SendError(c, common.NewBadRequestError("Invalid key_name provided.")); return` in `internal/nova/handlers.go`
- [X] T008 [US1] `CreateServer`: persist `key_name` in the `instances` INSERT (`handlers.go:425-428`) — add the column + bind `req.Server.KeyName` (empty string → SQL `NULL`) in `internal/nova/handlers.go`
- [X] T009 [US1] `GetServer`: add `key_name` to the SELECT (`handlers.go:992-1000`), scan into a `*string`, and set `key_name` in the response map (nil → JSON `null`) (`handlers.go:1016-1035`); add `KeyName *string` to the Instance model (`internal/database/models.go:62-76`) only if that struct is used on this path, in `internal/nova/handlers.go`
- [X] T010 [US1] `ListServersDetail`: add `key_name` to the SELECT (`handlers.go:849-858`) and each per-server entry (`handlers.go:885-912`), consistent with `GetServer`, in `internal/nova/handlers.go`
- [X] T011 [US1] Apply migration (`make migrate`) and turn US1 GREEN: `O3K_TEST_DB_URL=… go test ./internal/nova/ -run KeyName` passes

**Checkpoint**: US1 fully functional and independently testable — `key_name` round-trips; unknown keypair → 400.

---

## Phase 4: User Story 2 - security_groups read-back (Priority: P1)

**Goal**: Server-detail returns `security_groups` (list of `{name}`); a repeated `terraform apply` no
longer churns on `security_groups`. Groups supplied at create are persisted (FR-005); unknown
*non-default* names are rejected (FR-005a); the project's `default` SG is ensured on demand (FR-006a).

**Independent Test**: create with `--security-group default` → `show` lists `[{"name":"default"}]`; create
with no SG → still `[{"name":"default"}]`; unknown non-default name → 400; explicit `["default"]` in a
fresh (non-seeded) project succeeds (ensure-default).

**Dependency**: US2's `handlers.go` tasks (T017, T018, T020, T021) follow US1's edits to the same
functions — do US1 first on that file. T015, T016, T019 and the tests are in independent regions/files.

### Tests for User Story 2 (write FIRST — must FAIL before implementation) ⚠️

- [X] T012 [P] [US2] Failing unit tests: `GET /v2.1/servers/:id` and `/servers/detail` return `security_groups` as `[{"name":…}]` — created with one group, with multiple groups, with none → `[{"name":"default"}]`, and an orphaned association (SG row deleted) is dropped — in `internal/nova/server_detail_secgroups_test.go`
- [X] T013 [P] [US2] Failing unit tests: `POST /v2.1/servers` create-time SG behavior — unknown non-default name → `400` "Security group `<name>` not found." (no instance); explicit `security_groups=[{"name":"default"}]` succeeds and is read back, including in a project with no pre-existing `default` SG (FR-006a ensure) — in `internal/nova/server_detail_secgroups_test.go`

### Implementation for User Story 2

- [X] T014 [US2] Add `SecurityGroups []struct{ Name string \`json:"name"\` } \`json:"security_groups"\`` to `CreateServerRequest.Server` (`handlers.go:276-294`) in `internal/nova/handlers.go`
- [X] T015 [P] [US2] Add idempotent `ensureDefaultSecurityGroup(ctx, projectID) (string, error)` — INSERT `security_groups` (`gen_random_uuid()`, `name='default'`, project, description) + default rules into `security_group_rules`, mirroring `migrations/002_seed_data.up.sql:54-69`, `ON CONFLICT … DO NOTHING`, then re-select and return the id (FR-006a) in `internal/nova/advanced_actions.go`
- [X] T016 [P] [US2] Add a read helper resolving a server's SG names via INNER JOIN `server_security_groups` → `security_groups` — a single-server form and a batched `WHERE server_id = ANY($1)` form (returns `map[server_id][]name`); orphaned rows drop out of the INNER JOIN (D2) — in `internal/nova/advanced_actions.go`
- [X] T017 [US2] `CreateServer`: **before** the INSERT, resolve each supplied SG name via `SELECT id FROM security_groups WHERE name = $1 AND project_id = $2` (`advanced_actions.go:690-693` pattern); if the name is `default` and not found → `ensureDefaultSecurityGroup` (T015); any other `pgx.ErrNoRows` → `common.SendError(c, common.NewBadRequestError("Security group "+name+" not found.")); return` (FR-005a); collect resolved ids in `internal/nova/handlers.go`
- [X] T018 [US2] `CreateServer`: after the `instances` INSERT, INSERT each `(server_id, security_group_id)` into `server_security_groups` (migration 049) in `internal/nova/handlers.go`
- [X] T019 [US2] `GetServer`: build `security_groups` from the single-server helper (T016) as `[{"name":…}]`; when empty synthesize `[{"name":"default"}]` (D3); add to the response map (`handlers.go:1016-1035`) in `internal/nova/handlers.go`
- [X] T020 [US2] `ListServersDetail`: fetch groups for the whole page with the batched helper (T016, single query — no N+1, D6), group per server, synthesize `[{"name":"default"}]` per empty server, add `security_groups` to each entry (`handlers.go:885-912`) in `internal/nova/handlers.go`
- [X] T021 [US2] Turn US2 GREEN: `O3K_TEST_DB_URL=… go test ./internal/nova/ -run SecGroup` passes

**Checkpoint**: US1 + US2 both work independently — the full F1 no-op is achievable.

---

## Phase 5: User Story 3 - Regression caught in CI (Priority: P2)

**Goal**: A later change that drops `key_name` or `security_groups` from server-detail fails the build.

**Independent Test**: remove a field from the response → the CI test job fails; restore → passes.

- [X] T022 [US3] Wire the DB-gated nova tests into CI: in the `unit-tests` job (`.github/workflows/ci.yml:57-107`, which already has a Postgres service) export `O3K_TEST_DB_URL` pointing at that service on the `go test` step, so the nova read-back tests actually execute instead of skipping (`TestMain` runs migrations itself — research.md D9) in `.github/workflows/ci.yml`
- [X] T023 [US3] Sanity-check the guard: temporarily remove one read-back field locally, confirm `go test ./internal/nova/...` fails, then restore and confirm it passes (document in the PR; no committed change)

**Checkpoint**: Regression protection is live in CI.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T024 [P] Update `docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md` — mark finding **F1** resolved (write-up at lines ~86-116; note read-back + ensure-default)
- [X] T025 [P] Add a gophercloud contract test in `test/contract/` asserting `server.KeyName` and `server.SecurityGroups` round-trip after create, plus the unknown-keypair / unknown-non-default-SG 400 paths (Constitution I — real clients)
- [ ] T026 Run the Terraform compatibility scenario `test/terraform/compat/`: `terraform apply` then `terraform plan -detailed-exitcode` returns **0** (F1 no-op, SC-001); record the result — ⏳ **host validation**: not runnable in this sandbox (the OpenStack ports are already bound by an external process, and `terraform init` needs network to fetch the provider). Run per `docs/specs/016-.../HOST_TESTING.md`.
- [ ] T027 Run `quickstart.md` end-to-end in stub mode (key_name round-trip, SG round-trip, both 400 paths, the re-plan no-op) — ⏳ **host validation** (same blocker as T026). Behavior is already covered by the DB-backed unit tests, which drive the real handlers over HTTP against Postgres.
- [X] T028 Gates green: `make build` + `go tool golangci-lint run ./...` + `O3K_TEST_DB_URL=… go test -race ./internal/nova/...`

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (Phase 1)**: ✅ complete.
- **Foundational (Phase 2)**: none beyond Setup.
- **US1 (Phase 3)**: can start now. MVP.
- **US2 (Phase 4)**: independently testable, but its `handlers.go` tasks (T017/T018/T019/T020) must follow US1's edits to the same functions. T015/T016 (advanced_actions.go) and T012/T013 (tests) are independent.
- **US3 (Phase 5)**: meaningful only once US1/US2 tests exist (something to gate); the CI env change itself is independent.
- **Polish (Phase 6)**: after US1 + US2 are GREEN.

### Within each user story

- Tests (RED) before implementation.
- Migration before the create/read code depends on the column (`make migrate`).
- Create-time persistence before read-back can return real values (but read-back defaults/null work regardless).

### Parallel opportunities

- T001/T002 (done) were parallel.
- US1: T003+T004 (tests, same file — write together), T005+T006 (migration up/down, distinct files) are `[P]`.
- US2: T012+T013 (tests), T015+T016 (advanced_actions.go helpers) are `[P]`.
- Polish: T024 + T025 are `[P]`.
- **Not parallel**: any two tasks both editing `internal/nova/handlers.go` (T007–T010, T014, T017–T020) — same file.

---

## Parallel Example: User Story 1

```bash
# Tests first (RED) — same test file, write together:
Task: "T003/T004 key_name read-back + FR-002a 400 tests in internal/nova/server_detail_keyname_test.go"
# Then the two migration files in parallel:
Task: "T005 migrations/063_nova_instance_key_name.up.sql"
Task: "T006 migrations/063_nova_instance_key_name.down.sql"
# Then the handlers.go edits sequentially (same file): T007 → T008 → T009 → T010
```

---

## Implementation Strategy

### MVP (US1 + US2 — both P1)

The ticket's F1 no-op needs *both* fields, so the true MVP is US1 + US2. US1 alone is still a valid,
shippable increment (kills the key_pair churn) and is the recommended first slice.

1. Phase 1 Setup ✅ → 2. US1 (key_name) → validate → 3. US2 (security_groups) → validate → 4. US3 CI gate → 5. Polish.

### Incremental delivery

- US1 → `key_name` churn gone (demo `terraform plan` no longer replaces on key_pair).
- US2 → `security_groups` churn gone → **full F1 no-op** (SC-001: `terraform plan -detailed-exitcode` = 0).
- US3 → regression locked in CI.
- Polish → report updated, contract + terraform + quickstart validated, gates green.

---

## Notes

- `[P]` = different files, no dependency on an incomplete task.
- `[X]` = already complete (Phase 1 harness, built + verified in planning).
- Constitution: synchronous + fail-fast validation (FR-002a/FR-005a) before the INSERT; reversible
  migration (up+down); real-DB tests over mocks (gated by `O3K_TEST_DB_URL`); Nova v2.1 wire shapes.
- `ensureDefaultSecurityGroup` is idempotent (`ON CONFLICT DO NOTHING`) — safe under concurrent creates.
- Commit after each task or logical group; verify tests are RED before implementing.
