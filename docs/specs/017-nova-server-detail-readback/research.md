# Phase 0 Research: Nova server-detail read-back

**Feature**: `017-nova-server-detail-readback` | **Date**: 2026-07-17

All product ambiguities were resolved in the two `/cl.clarify` sessions (see spec §Clarifications).
This document resolves the remaining **code-fact** unknowns — including the three items the
clarify pass deferred to planning — each grounded in the current source.

---

## D1 — `key_name` persistence

**Decision**: Add a nullable `key_name` column to `instances` via migration `063`; write it in
`CreateServer`'s INSERT; select it in `GetServer` and `ListServersDetail`. Emit `"key_name": null`
when unset.

**Rationale**: `key_name` is parsed today (`handlers.go:284`) but only used transiently inside the
async cloud-init goroutine (`handlers.go:517-526`); there is no column to read back. A nullable
column means pre-existing rows read back as `null` — exactly the Nova wire value for "no keypair"
(verified in the api-ref) — with no data backfill.

**Alternatives considered**: Deriving key_name from a keypair↔instance link — rejected: `keypairs`
has no instance FK (`UNIQUE(user_id, name)` only), so there is nothing to join.

---

## D2 — `security_groups` read-back source & shape

**Decision**: Resolve an instance's groups by **INNER JOIN** `server_security_groups` →
`security_groups` (on `security_group_id = security_groups.id`), returning `[{"name": <name>}]`.
When the instance has **no** resolvable rows, synthesize `[{"name": "default"}]`.

**Rationale**:
- The join table exists (migration `049`) and is the only per-instance SG store.
- `server_security_groups.security_group_id` has **no FK** (`049:9-14`) and SG deletion is
  unconditional (`neutron/ports.go:804-829`), so **orphaned rows are reachable**. An INNER JOIN
  drops orphans automatically — detail reflects only groups that currently exist. *(Resolves
  deferred item (b): orphaned-SG handling.)*
- `security_groups` shape `[{"name": …}]` and the always-present `default` were verified against
  the Nova api-ref during clarify.

**Alternatives considered**: LEFT JOIN keeping stale names — rejected (would report deleted
groups). Storing SG **names** on the instance (snapshot) — rejected (diverges from "current
membership", spec Edge Cases).

---

## D3 — Default security group: synthesize on read, do **not** persist at create

**Decision**: When no security group is supplied at create, persist **nothing**; the read-back
synthesizes `[{"name": "default"}]` for any instance whose join is empty.

**Rationale**: The `default` SG is **only seeded for the seeded default project** (`002_seed_data`
`:54-61`); `CreateProject` (`keystone/handlers.go:448-501`) does **not** create one. So a
"persist the default association at create" approach would try to resolve a `default` row that may
not exist for API-created projects. Synthesizing on empty read is robust regardless of whether a
`default` row exists, matches Nova (an instance always shows at least `default`), and also covers
the "created with SGs, later removed them all" edge → empty → `default`. *(Resolves deferred item
(a): persist-vs-synthesize.)*

**Conformance fold-in → see D3a.** (An earlier draft scoped this out; folded in after confirming
it is OpenStack-conformant.)

---

## D3a — Ensure a per-project `default` security group (conformance fold-in)

**Decision**: Add an idempotent `ensureDefaultSecurityGroup(ctx, projectID)` that creates the
project's `default` SG (+ its default rules) if missing, and call it in the create SG-resolution
path when `default` is referenced but does not resolve. `security_groups=["default"]` then succeeds
in **any** project, not only the seeded default project.

**Rationale**: Verified OpenStack-conformant — the Neutron admin docs state *"Each project has a
default security group … automatically applied to instances and ports unless otherwise specified."*
o3k only **seeds** a `default` SG for the seeded default project (`002_seed_data:54-61`);
`CreateProject` never creates one (`keystone/handlers.go:448-501`), and neither Nova nor Neutron
ensures it at use-time (both only `SELECT … WHERE name='default'`). This closed a real gap: an
explicit `security_groups=["default"]` in an API-created project would otherwise 400.

**Implementation**: INSERT into `security_groups` (`id=gen_random_uuid()`, `name='default'`,
`project_id`, description) + `security_group_rules` mirroring `002_seed_data.up.sql:54-69` (egress
IPv4/IPv6 allow-all, ingress tcp/22), both `ON CONFLICT … DO NOTHING`, then re-select the id.
**Idempotent** (safe under concurrent creates), **no schema change** (runtime INSERTs into existing
tables). Genuinely-unknown *non-default* names still 400 (D4). Read-side synthesis (D2/D3) remains
the safety net for instances with no associations.

**Scope note**: This is a targeted, conformance-driven fold-in at the Nova create path — it does
**not** retrofit a `default` SG onto every existing project eagerly (that would be a Keystone
`CreateProject` change); the lazy ensure-on-use covers old and new projects where it matters.

**Alternatives considered**: (a) Eager creation in Keystone `CreateProject` — additive but does not
help projects created before the change, so insufficient alone; kept out to bound the change to the
Nova path. (b) Leave as a 400 limitation — rejected by the user's "fold in if conformant" directive.

## D4 — Create-time SG persistence & validation (FR-005 / FR-005a)

**Decision**: Add `security_groups []{ Name string }` to `CreateServerRequest`
(`handlers.go:276-294`). In `CreateServer`, **before** the INSERT, resolve each supplied name with
the existing query `SELECT id FROM security_groups WHERE name = $1 AND project_id = $2`
(`advanced_actions.go:690-693`); on any `pgx.ErrNoRows` return `400` and create nothing; otherwise
INSERT each `(server_id, security_group_id)` into `server_security_groups`.

**Rationale**: Matches real Nova (unknown SG at boot → `400`, synchronous fault — verified) and
Constitution II/III (synchronous, fail-fast). Reuses the proven, project-scoped resolver from
`AddSecurityGroup`. project_id comes from `c.GetString("project_id")` (Constitution V).

**Alternatives considered**: Lenient skip of unknown names — rejected in clarify (Q1 → A).

---

## D5 — Create-time keypair validation (FR-002a)

**Decision**: In `CreateServer`, before the `instances` INSERT (in the synchronous window
`handlers.go:~341-424`, alongside flavor/quota checks), if `req.Server.KeyName != ""`, run
`SELECT 1 FROM keypairs WHERE user_id = $1 AND name = $2` (`userID` from `handlers.go:340`); on
`pgx.ErrNoRows` return `common.SendError(c, common.NewBadRequestError("Invalid key_name provided.")); return`.

**Rationale**: Today there is **no** synchronous keypair validation — the only lookup sits inside
the async goroutine (`handlers.go:517-526`) *after* the 202 and is warn-only. Real Nova v2.1 applies
strict input validation and rejects an unknown key_name with `400` before boot. Adding the sync
check satisfies Constitution II (synchronous) and III (fail-fast) and clarify Q4 → A. Keypairs are
user-owned, so scope by `user_id` (Constitution V).

**Note**: Once validated synchronously, the existing async lookup (`:517-526`) will always find the
key; it can stay as-is (warn-only) or reuse the validated `public_key`. Keep the async lookup — no
behavior change needed there.

---

## D6 — List-detail batch query (no N+1)

**Decision**: For `ListServersDetail`, fetch all groups for the page in **one** query —
`SELECT ssg.server_id, sg.name FROM server_security_groups ssg JOIN security_groups sg ON sg.id = ssg.security_group_id WHERE ssg.server_id = ANY($1)` — then group by `server_id` in Go and apply the
`default` synthesis per server. `key_name` is added to the existing per-page SELECT (no extra
query). *(Resolves deferred item (c): N+1.)*

**Rationale**: Constitution performance standard (< 100 ms) and Simplicity; `= ANY($1)` over the
page's server IDs is a single round-trip.

**Alternatives considered**: Per-server helper call in a loop — rejected (N+1). Acceptable for
`GetServer` (single server → one query) but not for list-detail.

---

## D7 — Error emission (Nova-compatible 400)

**Decision**: Use `common.SendError(c, common.NewBadRequestError(<msg>)); return`
(`common/errors.go:129-135`, `SendError` `:42-44`). Messages mirror Nova: `"Invalid key_name
provided."` (keypair) and `"Security group <name> not found."` (SG). This yields the Nova envelope
`{"badRequest": {"message": …, "code": 400}}` (`ToJSON` `:26-39`), the idiom already used at
`handlers.go:335`.

**Rationale**: Constitution I — response envelope must match Nova; reuse the established helper.

---

## D8 — Migration `063` shape & rollback

**Decision**:
- `063_nova_instance_key_name.up.sql`: `ALTER TABLE instances ADD COLUMN IF NOT EXISTS key_name VARCHAR(255);`
- `063_nova_instance_key_name.down.sql`: leading `-- Rollback` comment + `ALTER TABLE instances DROP COLUMN IF EXISTS key_name;`

**Rationale**: Mirrors the repo convention (`049`/`062` down files: `-- Rollback` header, `IF
EXISTS`, reverse order). `VARCHAR(255)` matches `security_groups.name`/keypair name sizing and the
Nova key_name limit. Nullable (no `NOT NULL`, no default) so existing rows → `null`. Constitution:
migrations reversible.

---

## D9 — Test harness (Nova has none today)

**Decision**: Add to `internal/nova`:
1. a `TestMain` mirroring `internal/keystone/maindb_test.go` (connect via `O3K_TEST_DB_URL`,
   `database.MigrateUp(dbURL, "../../migrations")`, `requireDB(t)` skip-gate);
2. a gin + `httptest` router helper mirroring `internal/keystone/identity_surface_test.go`
   (inject `project_id`/`user_id`/`roles` via a test middleware);
3. table tests asserting `key_name` and `security_groups` JSON on `GetServer` + `ListServersDetail`
   (with/without keypair, with/no/unknown SG), plus the two `400` create paths.

**Rationale**: The repo uses a **real test Postgres gated by `O3K_TEST_DB_URL`**, no mocks (go.mod
has only testify; grep for pgxmock/sqlmock/testcontainers = none). Constitution "real dependencies
over mocks". Nova currently has only `boot_from_volume_test.go` (pure unit, no DB/gin) so the
scaffolding must be created by copying keystone's primitives.

**CI dependency (US3 guard)**: these DB tests **skip** unless `O3K_TEST_DB_URL` is set. The CI
`unit-tests` job (`.github/workflows/ci.yml:57-107`) has a Postgres service — the plan must ensure
that job exports `O3K_TEST_DB_URL` (and runs migrations), otherwise the nova read-back tests skip
and US3's regression guard is inert. **Verify/instrument this in `/cl.tasks`.**

---

## Summary of deferred-item resolutions

| Deferred (from clarify) | Resolution | Grounding |
|---|---|---|
| (a) default-SG persist vs synthesize | **Synthesize on empty read** | `default` SG only seeded for default project (`002_seed_data:54-61`); not per-project |
| (b) orphaned SG on delete | **INNER JOIN drops orphans** | no FK on `server_security_groups.security_group_id` (`049:9-14`); unconditional SG delete (`neutron/ports.go:804-829`) |
| (c) list-detail N+1 | **Single `= ANY($1)` batch query** | Constitution perf + Simplicity |
