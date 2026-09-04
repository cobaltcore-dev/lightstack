---
description: "Task list — Identity resource not-found for malformed ids (015-identity-uuid-404)"
---

# Tasks: Identity resource not-found for malformed ids

**Input**: Design documents from `docs/specs/015-identity-uuid-404/`
**Prerequisites**: plan.md, spec.md, research.md (D1–D9), data-model.md, contracts/identity-not-found.md, quickstart.md

**Tests**: INCLUDED and test-first — Constitution Article III (TDD) is NON-NEGOTIABLE. Every test task must be written and observed FAILING (RED) before its implementation task.

**Mechanism (from plan.md / research.md)**: hybrid — a keystone-scoped UUID path-param **middleware** on the non-authz-gated routes (`val` subgroup), plus **in-handler** id validation *after* the authz check on the authz-gated routes. One shared verbatim 404 via `common.NewIdentityNotFoundError`; a shared lookup helper maps `pgx.ErrNoRows`→404 else→5xx (FR-010). No schema change, no new dependency. Built on the feature/480 tree; lands as a separate PR after #73 squash-merges.

**Path conventions**: single Go project — `internal/keystone/`, `internal/common/`, `test/`.

---

## Phase 1: Setup

**Purpose**: Establish the RED baseline and confirm the test harness.

- [X] T001 Confirm baseline gates on the branch: `make build`, `make test`, `make lint` pass; confirm DB-backed keystone tests run with `LIGHTSTACK_TEST_DB_URL` set (existing `requireDB`/`TestMain` in `internal/keystone/maindb_test.go`).
- [ ] T002 Capture the RED baseline (reference only): with an admin token, record current responses per `quickstart.md` §1 — `GET /v3/projects/not-a-uuid` → 500; one endpoint per group (roles/groups/services/credentials + a role-assignment path) → 500; `GET /v3/projects/<unknown-uuid>` → 404.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Shared mechanism that ALL user stories depend on. ⚠️ No user-story work begins until this phase is complete.

**Tests (write first, confirm RED):**

- [X] T003 [P] Unit test `common.NewIdentityNotFoundError` in `internal/common/errors_test.go`: asserts 404, `Code=="itemNotFound"`, `Message=="Could not find <resource>: <id>."`, no `Details`.
- [X] T004 [P] Middleware unit tests (no DB) in `internal/keystone/uuid_middleware_test.go` using `gin.CreateTestContext`: valid UUID → `Next` (not aborted); `not-a-uuid` → 404 + verbatim message + `IsAborted()`; `access` param ignored; each allowlisted name (`id`,`user_id`,`role_id`,`cred_id`); multi-segment path reports the FIRST malformed segment with the correct noun; noun derivation incl. `application_credentials → "Application Credential"` override.
- [X] T005 [P] Unit test the lookup helper in `internal/keystone/identity_errors_test.go`: `pgx.ErrNoRows` → 404 verbatim (via `NewIdentityNotFoundError`); a generic non-`ErrNoRows` error → 5xx (NOT masked as 404) — FR-007/FR-010.

**Implementation (make the above GREEN):**

- [X] T006 Add `common.NewIdentityNotFoundError(resource, id string) *OpenStackError` (404, `itemNotFound`, `"Could not find <resource>: <id>."`) in `internal/common/errors.go`; refactor existing `NewProjectNotFoundError` to delegate to it.
- [X] T007 Implement `svc.uuidParamMiddleware` in `internal/keystone/uuid_middleware.go`: for each path param whose name is `id` or ends with `_id` (skip `access` and any other name), in path order, `uuid.Validate` — on first failure send `common.NewIdentityNotFoundError(noun, value)` + `c.Abort()`, else `c.Next()`. `noun = singularize(segment preceding the param in c.FullPath())` (strip trailing `s`, `_`→space; override `application_credentials`→`"Application Credential"`). (depends on T006)
- [X] T008 Implement the lookup helper in `internal/keystone/identity_errors.go` (e.g. `notFoundOr500(c, err, resource, id)`): `errors.Is(err, pgx.ErrNoRows)` → `common.NewIdentityNotFoundError(resource,id)`; else log + `common.NewDatabaseError`/500. (depends on T006)
- [X] T009 Wire route groups in `internal/keystone/handlers.go` `RegisterRoutes`: after `mgmt.Use(authMiddleware)`, add `val := mgmt.Group(""); val.Use(svc.uuidParamMiddleware)`; register all NON-authz-gated by-id routes on `val`; keep authz-gated routes (`DeleteUser`, `ChangePassword`, and the four EC2-credential routes) on plain `mgmt`. (depends on T007)

**Checkpoint**: Foundation ready — malformed path ids on `val` routes now 404 via the middleware; helper + constructor available.

---

## Phase 3: User Story 1 - Reference identity resources by name (Priority: P1) 🎯 MVP

**Goal**: `GET /v3/{projects,users,roles}/{malformed-or-unknown}` returns a Keystone-verbatim 404 so CLI/Terraform name→id resolution works.

**Independent Test**: `openstack project show <existing-name>` resolves; `openstack project show not-a-uuid` → NotFound (not 500); a role added by name succeeds.

**Tests (write first, confirm RED):**

- [X] T010 [P] [US1] DB-backed test in `internal/keystone/identity_notfound_test.go` (`requireDB`): for projects, users, roles — `GET /{malformed}` → 404 verbatim; `GET /{unknown-uuid}` → 404 verbatim (identical body); `GET /{existing}` → 200.
- [X] T011 [P] [US1] Integration test `test/keystone_name_resolution_test.sh`: ticket repro → 404; `openstack project show <name>`, `--project <name>`, `openstack role add … <role-name>` succeed with no 500.

**Implementation:**

- [X] T012 [US1] Normalize the not-found path of `GetProject`, `GetUser`, `GetRole` in `internal/keystone/handlers.go` to route through the T008 helper (valid-but-unknown → verbatim 404 via `NewIdentityNotFoundError`); malformed already handled by the middleware.

**Checkpoint**: The ticket's headline (name resolution for projects/users/roles) is fixed and independently demoable.

---

## Phase 4: User Story 2 - Consistent not-found across the whole identity surface (Priority: P2)

**Goal**: Every identity by-id endpoint (groups, domains, services, endpoints, credentials, application credentials, EC2, nested role-assignment/group-membership) returns the identical verbatim 404 for malformed AND valid-but-unknown ids, with authorization taking precedence, and no masking of genuine faults.

**Independent Test**: probe one endpoint per resource group with malformed / unknown-uuid / existing id → 404 / 404 / 200-204; non-admin + malformed on a gated route → 403.

**Tests (write first, confirm RED):**

- [X] T013 [P] [US2] DB-backed coverage in `internal/keystone/identity_notfound_test.go`: one endpoint per group (group, domain, service, endpoint, credential, application_credential, EC2) — malformed→404 verbatim, unknown→404 verbatim, existing→200/204; plus a nested role-assignment path (`/projects/:id/users/:user_id/roles/:role_id`) with each segment malformed → 404 with the matching noun, and a group-membership path.
- [X] T014 [P] [US2] Authz-precedence test in `internal/keystone/identity_authz_test.go`: non-admin + malformed id on `DeleteUser`, `ChangePassword`, and an EC2 route → 403; unauthenticated + malformed → 401; admin/owner + malformed → 404 (verbatim).

**Implementation (T015–T019 are parallel — distinct files):**

- [X] T015 [P] [US2] `internal/keystone/domains.go` — route `GetDomain`/`UpdateDomain`/`DeleteDomain`/`GetDomainConfig` not-found through the T008 helper; remove the `if err != nil → NotFound` masking (ErrNoRows→404 verbatim, genuine error→5xx).
- [X] T016 [P] [US2] `internal/keystone/groups.go` — same for `GetGroup`/`UpdateGroup`/`DeleteGroup`/`ListGroupUsers`/`AddUserToGroup`/`RemoveUserFromGroup`.
- [X] T017 [P] [US2] `internal/keystone/services.go` — same for `GetService`/`UpdateService`/`DeleteService` and `DeleteEndpoint`.
- [X] T018 [P] [US2] `internal/keystone/credentials.go` — same for `GetCredential`/`UpdateCredential`/`DeleteCredential`.
- [X] T019 [P] [US2] `internal/keystone/application_credentials.go` — same for `GetApplicationCredential`/`ListApplicationCredentials`/`DeleteApplicationCredential`/`GetApplicationCredentialByID`.
- [X] T020 [US2] `internal/keystone/handlers.go` — normalize the remaining not-found paths + remove masking: `UpdateUser`, `GetUserProjects`, `GetUserGroups` (drop `if err != nil → NotFound`), `UpdateProject`/`UpdateRole` fetch, `DeleteProject`/`DeleteRole` (RowsAffected==0), and the role-assignment handlers (`AssignRole`/`UnassignRole`/`ListUserProjectRoles`/`CheckRoleAssignment`) → verbatim 404 via the helper.
- [X] T021 [US2] `internal/keystone/handlers.go` — add in-handler id validation AFTER the authz check for `DeleteUser` (after `hasRole("admin")`) and `ChangePassword` (after the self-or-admin check): `uuid.Validate(id)` fail → `common.NewIdentityNotFoundError("user", id)`. (depends on T020 — same file)
- [X] T022 [P] [US2] `internal/keystone/ec2_credentials.go` — add in-handler `:id` (user) validation AFTER `denyIfNotSelfOrAdmin` in `CreateEC2Credential`/`ListEC2Credentials`/`GetEC2Credential`/`DeleteEC2Credential` → `NewIdentityNotFoundError("user", id)`; leave the `access` param and the existing tenant/project check untouched.

**Checkpoint**: The entire identity path-id surface is uniformly conformant (US1 + US2).

---

## Phase 5: User Story 3 - Malformed input does not masquerade as a server fault (Priority: P3)

**Goal**: A malformed id is a client error (4xx), never a 5xx; and a genuine backend fault still surfaces as 5xx (delivered by the T008 helper) rather than a masked 404.

**Independent Test**: malformed-id requests across the surface return 4xx (never 5xx); a simulated genuine backend error on a touched handler returns 5xx; malformed-id requests are not logged as server errors.

**Tests (write first, confirm RED):**

- [X] T023 [P] [US3] DB-backed sweep in `internal/keystone/identity_notfound_test.go`: assert every representative endpoint returns a 4xx (not 5xx) for a malformed id. (The genuine-error→5xx guarantee is covered by the helper unit test T005.)

**Implementation:**

- [X] T024 [US3] Ensure observability honesty in `internal/keystone/uuid_middleware.go` + `internal/keystone/identity_errors.go`: malformed-id 404s are logged at debug/info (client error), genuine backend errors keep error-level logging + 5xx. Confirm no malformed-id path is counted/logged as a server fault.

**Checkpoint**: Client mistakes read as 4xx; real faults read as 5xx.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T025 [P] Verify per-resource message strings against a reference Keystone (DevStack / `exception.py`) per `research.md` D7; adjust the `singularize`/override map in `internal/keystone/uuid_middleware.go` if any noun differs (esp. "Application Credential").
- [X] T026 Run gates: `make test` (with `-race`), `make lint`, `make build` — all green.
- [ ] T027 Run `./test/quick_test.sh` + keystone integration and the full `quickstart.md` validation (steps 3–4); confirm SC-001..005.
- [X] T028 [P] File the two follow-up tickets: (a) malformed query/filter ids (`role_assignments?user.id=`, likely 200 empty-list); (b) the drafted authZ-policy-layer enhancement. (Optionally reconcile the posted issue impl-checklist wording with Q5=A.)

---

## Dependencies & Execution Order

- **Setup (T001–T002)** → no deps.
- **Foundational (T003–T009)** → after Setup; **blocks all user stories**. Within: T003/T004/T005 (RED) before T006/T007/T008; T006 before T007 & T008; T007 before T009.
- **US1 (T010–T012)** → after Foundational. T010/T011 (RED) before T012. **MVP.**
- **US2 (T013–T022)** → after Foundational. T013/T014 (RED) before T015–T022. T015–T019 parallel (distinct files); T020 before T021 (same file `handlers.go`); T022 parallel with T020/T021.
- **US3 (T023–T024)** → after Foundational (helper honesty is foundational); the masking fix it verifies is implemented in US2, so run US3 verification after US2. T023 (RED) before T024.
- **Polish (T025–T028)** → after all desired stories.

### Story independence

- US1 is a standalone MVP (fix the ticket's named resources + name resolution).
- US2 extends coverage to the rest of the surface + authz-gated wiring (independently testable per group).
- US3 is primarily a verification lens over the shared helper (honest 4xx/5xx); its guarantee is built in Foundational + US2.

## Parallel Example: Foundational tests

```bash
# RED, together (distinct files):
Task: "T003 Unit test NewIdentityNotFoundError in internal/common/errors_test.go"
Task: "T004 Middleware unit tests in internal/keystone/uuid_middleware_test.go"
Task: "T005 Lookup-helper unit test in internal/keystone/identity_errors_test.go"
```

## Parallel Example: User Story 2 implementation

```bash
# After US2 tests are RED, these touch distinct files → parallel:
Task: "T015 domains.go"   Task: "T016 groups.go"   Task: "T017 services.go"
Task: "T018 credentials.go"   Task: "T019 application_credentials.go"   Task: "T022 ec2_credentials.go"
```

## Implementation Strategy

- **MVP** = Phase 1 → Phase 2 → Phase 3 (US1). STOP and validate name resolution end-to-end; the ticket's user-visible breakage is fixed.
- **Increment 2** = Phase 4 (US2): whole-surface conformance + authz precedence.
- **Increment 3** = Phase 5 (US3): honesty guarantees + observability.
- **Finish** = Phase 6: Keystone message parity + gates + follow-ups.
- Keep genuine-error→5xx intact throughout (FR-007/FR-010); never achieve 404 by masking.
