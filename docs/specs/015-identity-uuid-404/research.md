# Phase 0 Research: Identity resource not-found for malformed ids

All decisions below were settled during specification + two `/cl.clarify` sessions (5 Q&A) and the design discussion. No `NEEDS CLARIFICATION` remain.

## D1 — Root cause & why the fix sidesteps the cast error

**Decision**: Validate id *format* before any DB query (middleware or in-handler), and for well-formed ids rely on `pgx.ErrNoRows` for not-found.
**Rationale**: The identity id columns are Postgres `uuid`; a non-UUID bound value raises `22P02 invalid input syntax for type uuid`, which `common.HandleDatabaseError` maps to 500 (not `pgx.ErrNoRows`). By checking `uuid.Validate` *before* the query, a malformed id never reaches the DB, so `22P02` never occurs; a well-formed-but-unknown id yields `ErrNoRows`. Neither path produces the cast 500.
**Alternatives**: (a) Catch `22P02` (`*pgconn.PgError` code) and map to 404 — works but leaves the wasted round-trip and is implicit; (b) `WHERE id::text = $1` — defeats the uuid index. Rejected in favor of explicit format validation.

## D2 — Mechanism: hybrid (middleware + in-handler after authz)

**Decision**: Two gin subgroups off the authenticated `mgmt` group:
`val := mgmt.Group(""); val.Use(uuidParamMiddleware)` carries the non-authz-gated by-id routes; the authz-gated routes stay on plain `mgmt` and validate the id *in-handler, after* their authorization check.
**Rationale**: Q4=B requires authorization (403) and authentication (401) to precede the malformed-id 404. gin runs group middleware before the handler, so a blanket pre-handler middleware would 404 before a handler's in-code `hasRole`/`denyIfNotSelfOrAdmin` check. gin captures the middleware chain per route at registration, so `GET /users/:id` (on `val`) and `DELETE /users/:id` (on `mgmt`) coexist with different chains. Authz-gated set: `DeleteUser` (admin), `ChangePassword` (self-or-admin), and the EC2-credential endpoints (`denyIfNotSelfOrAdmin`).
**Alternatives**: (a) Pure blanket middleware — simpler but returns 404 before 403 for a non-admin + malformed id (violates Q4=B); (b) Per-handler guards everywhere — no coverage guarantee, ~35 sites, misses future routes; (c) Promote authz to a middleware/policy layer (oslo.policy-style) — the OpenStack-idiomatic shape, but a large separate refactor (filed as a follow-up enhancement, not #515).

## D3 — Resource noun for the verbatim message

**Decision**: `resource = singularize(path segment immediately preceding the offending param)` from `c.FullPath()`; `singularize` strips a trailing `s` and turns `_`→space. Validate params in path order; report the first malformed segment.
**Rationale**: Uniform rule that needs no hand-maintained map and matches every route: `/projects/:id`→"project", `/users/:id/application_credentials/:cred_id`→ `:id`→"user", `:cred_id`→"application credential", `/projects/:id/users/:user_id/roles/:role_id`→ project/user/role. First-in-path-order matches Keystone's resolve order.
**Alternatives**: A static param→noun map — more maintenance, no benefit. One exception needs an override: Keystone title-cases *"Application Credential"* (see D7).

## D4 — Which path params to validate

**Decision**: Validate a param iff its name is `id` or ends with `_id` (today: `id`, `user_id`, `role_id`, `cred_id`). Skip `access` and any other name.
**Rationale**: All keystone uuid path params follow the `id`/`*_id` convention; the only non-uuid path segment is the EC2 `access` key (`credentials.access_key VARCHAR(255)`, migration 062) — FR-005 requires it pass through. The name-suffix rule auto-covers any future `*_id` route without editing the middleware, matching the "coverage guarantee" reason we chose a middleware.
**Alternatives**: A fixed allowlist `{id,user_id,role_id,cred_id}` — equivalent today but wouldn't auto-cover future routes.

## D5 — Shared 404 constructor & envelope

**Decision**: Add `common.NewIdentityNotFoundError(resource, id) → 404`, Nova-style envelope (`"itemNotFound"` code), message `"Could not find <resource>: <id>."`. Refactor the existing `common.NewProjectNotFoundError` (from #73) to delegate to it.
**Rationale**: Single source of truth for the verbatim text used by both the middleware and the in-handler/valid-unknown paths; matches the `6a72b95` precedent. Envelope stays Nova-style — Identity-v3 `{"error":{…}}` conformance is a surface-wide change tracked in #518 (Q3). Clients' name→id resolution keys on the 404 status only.
**Alternatives**: Generic `NewNotFoundError` (non-verbatim) — rejected by Q1/message-conformance; per-resource constructors — unnecessary given the uniform message.

## D6 — Valid-but-unknown normalization + masking fix (FR-010)

**Decision**: Route every touched identity handler's not-found path to a helper that maps `pgx.ErrNoRows → NewIdentityNotFoundError(resource,id)` and any other error → 5xx. This replaces both the 6 `HandleDatabaseError` call sites and the inline `if err != nil → NotFound` masking on `GetDomain`/`UpdateDomain`/`DeleteDomain`/`GetDomainConfig`/`UpdateUser`/`GetUserProjects`/`GetUserGroups`.
**Rationale**: Q1 requires valid-but-unknown ids to return the identical verbatim 404; a middleware only catches malformed (format), so each handler's not-found must emit verbatim. Q5=A requires the touched masking handlers to also stop masking genuine faults (FR-007/FR-010) — checking `ErrNoRows` specifically gives the 500 fall-through for free, matching Keystone (404 = missing resource; backend fault = 5xx, confirmed via `/openstack/keystone` api-ref).
**Alternatives**: Message-only (keep masking) — rejected by Q5 (contradicts FR-007). Fixing masking on untouched handlers — out of scope (separate follow-up).
**Note**: `common.HandleDatabaseError` is keystone-only (6 sites); the helper can extend it (add `id`, distinguish `ErrNoRows`) or be a small keystone-local function — implementation detail for tasks.

## D7 — Exact Keystone message strings (verify at implementation)

**Decision**: Target Keystone's `exception.py` `*NotFound` messages, verified against a reference Keystone (DevStack) during implementation. Working set:

| resource | message |
|---|---|
| project | `Could not find project: <id>.` (DevStack-verified in #73) |
| user | `Could not find user: <id>.` |
| role | `Could not find role: <id>.` |
| group | `Could not find group: <id>.` |
| domain | `Could not find domain: <id>.` |
| service | `Could not find service: <id>.` |
| endpoint | `Could not find endpoint: <id>.` |
| credential | `Could not find credential: <id>.` |
| application credential | `Could not find Application Credential: <id>.` (title-cased — override in `singularize`) |

**Rationale**: Constitution Art. I (OpenStack compatibility) + the user's "conform to OpenStack" rule. The api-ref (via Context7) confirms 404 is a distinct status from 5xx but does not carry the strings; they live in `exception.py`.
**Alternatives**: Generic message — rejected by Q1.

## D8 — Test strategy (test-first, Constitution Art. III)

**Decision**: (1) Middleware **unit tests, no DB** — `gin.CreateTestContext`, set `c.Params` + `FullPath`, assert 404 + verbatim message + `IsAborted()`; cover valid/`not-a-uuid`/`access`-skipped/each param name/first-malformed-in-path/noun derivation. (2) **DB-backed e2e** via `requireDB`/`O3K_TEST_DB_URL` — one representative endpoint per resource group: malformed→404 verbatim, valid-unknown→404 verbatim (identical), valid-existing→200/204. (3) **Authz precedence** — non-admin+malformed→403 on DeleteUser/ChangePassword/EC2; unauth+malformed→401; admin+malformed→404. (4) **Masking fix** — a genuine backend error on a touched handler still →5xx. All RED first.
**Rationale**: Middleware is cheaply unit-testable without a DB; handler behavior needs the real DB per Constitution Art. III/IV (real deps over mocks). No HTTP/router test harness exists in `internal/keystone` yet — build a minimal one in the test file.

## D9 — Landing / branch

**Decision**: Separate PR against main, landing **after** feature/480 (#73) squash-merges. Build on the current `bugfix/515` tree (= feature/480 tip) so the middleware sees the EC2/s3 routes. Do not rebase now (user handles it after #73 merges); reconcile `NewProjectNotFoundError → NewIdentityNotFoundError` with the squashed main.
**Rationale**: #515 is pre-existing and feature-agnostic; keeping it separate avoids bloating the SPEC-014 PR. Building on the feature/480 tree gives the full surface incl. EC2 endpoints.

## Follow-ups (out of scope — tracked separately)

- Malformed **query/filter** ids (e.g. `role_assignments?user.id=`) — likely 200 empty-list per Keystone, not 404; needs per-endpoint handling (separate ticket).
- **#518** — Identity-v3 error envelope + surface-wide message/format normalization.
- Error-masking cleanup on handlers #515 does **not** touch.
- Enhancement (drafted, user to file) — centralize authorization in a policy-driven layer (oslo.policy-style).
