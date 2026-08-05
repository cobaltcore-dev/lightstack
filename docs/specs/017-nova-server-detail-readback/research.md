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

## D3 — Default security group: ensure + persist at create; synthesize only as fallback

**Decision** *(revised 2026-09-14, PR #76 review)*: When no security group is supplied at create,
**ensure the project's `default` group (D3a) and persist the association** — the instance then
reads back `[{"name": "default"}]` from a real `server_security_groups` row, and the port
allocated at boot attaches the same group. Read-side synthesis of `[{"name": "default"}]` is kept
**only as a fallback** for instances with no surviving association: rows created before migration
`063`, or instances whose groups were all deleted since (the INNER JOIN drops orphans).

**Rationale**: The original decision persisted nothing and synthesized on read, because the
`default` SG is **only seeded for the seeded default project** (`002_seed_data:54-61`) and
`CreateProject` does **not** create one, so a persist-at-create approach could not resolve a row
that might not exist. D3a removed that obstacle by ensuring the group on demand, and once it
existed for the explicit path, leaving the implicit path unpersisted produced an honest-looking
lie: the API reported `default` while the port had no group at all (fail-closed, but not what was
reported). Persisting matches upstream, where a server's `security_groups` derives from its ports.
*(Supersedes the earlier resolution of deferred item (a): persist-vs-synthesize.)*

**Conformance fold-in → see D3a.** (An earlier draft scoped this out; folded in after confirming
it is OpenStack-conformant.)

---

## D3a — Ensure a per-project `default` security group (conformance fold-in)

**Decision**: Add an idempotent `neutron.EnsureDefaultSecurityGroup(ctx, projectID)` that creates the
project's `default` SG (+ its default rules) if missing, and call it in the create SG-resolution
path when `default` is referenced but does not resolve. `security_groups=["default"]` then succeeds
in **any** project, not only the seeded default project.

**Rationale**: Verified OpenStack-conformant — the Neutron admin docs state *"Each project has a
default security group … automatically applied to instances and ports unless otherwise specified."*
Lightstack only **seeds** a `default` SG for the seeded default project (`002_seed_data:54-61`);
`CreateProject` never creates one (`keystone/handlers.go:448-501`), and neither Nova nor Neutron
ensures it at use-time (both only `SELECT … WHERE name='default'`). This closed a real gap: an
explicit `security_groups=["default"]` in an API-created project would otherwise 400.

**Implementation** *(revised 2026-09-14, PR #76 review)*: `neutron.EnsureDefaultSecurityGroup`
runs one transaction: `INSERT INTO security_groups … ON CONFLICT (project_id, name) DO NOTHING
RETURNING id`. A returned row means **this call created the group**, and only then are the seed
rules written (mirroring `002_seed_data.up.sql:54-69`: egress IPv4/IPv6 allow-all, ingress tcp/22).
No row means the group already existed — a concurrent creator, an API-created `default`, or an
operator-managed one — and it is left exactly as found, rules included. The rules insert has **no**
`ON CONFLICT` clause and needs none: `security_group_rules` has no unique constraint on the rule
shape (only `PRIMARY KEY (id)`), and only the creating transaction reaches that insert. Concurrent
first use is serialized by the unique index on `(project_id, name)` — the second inserter blocks
until the first commits, then takes the "existing" branch. Measured with 8 concurrent callers ×
10 runs: fresh project → 3 rules every time; pre-existing empty `default` → 0 rules every time
(the earlier count-then-seed version produced 15 rules in 9 of 10 runs for the latter). **No
schema change** (runtime INSERTs into existing tables). Genuinely-unknown *non-default* names
still 400 (D4). Read-side synthesis (D2/D3) remains the fallback for instances with no
surviving association.

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
1. a `TestMain` mirroring `internal/keystone/maindb_test.go` (connect via `LIGHTSTACK_TEST_DB_URL`,
   `database.MigrateUp(dbURL, "../../migrations")`, `requireDB(t)` skip-gate);
2. a gin + `httptest` router helper mirroring `internal/keystone/identity_surface_test.go`
   (inject `project_id`/`user_id`/`roles` via a test middleware);
3. table tests asserting `key_name` and `security_groups` JSON on `GetServer` + `ListServersDetail`
   (with/without keypair, with/no/unknown SG), plus the two `400` create paths.

**Rationale**: The repo uses a **real test Postgres gated by `LIGHTSTACK_TEST_DB_URL`**, no mocks (go.mod
has only testify; grep for pgxmock/sqlmock/testcontainers = none). Constitution "real dependencies
over mocks". Nova currently has only `boot_from_volume_test.go` (pure unit, no DB/gin) so the
scaffolding must be created by copying keystone's primitives.

**CI dependency (US3 guard)**: these DB tests **skip** unless `LIGHTSTACK_TEST_DB_URL` is set. The CI
`unit-tests` job (`.github/workflows/ci.yml:57-107`) has a Postgres service — the plan must ensure
that job exports `LIGHTSTACK_TEST_DB_URL` (and runs migrations), otherwise the nova read-back tests skip
and US3's regression guard is inert. **Verify/instrument this in `/cl.tasks`.**

---

## Summary of deferred-item resolutions

| Deferred (from clarify) | Resolution | Grounding |
|---|---|---|
| (a) default-SG persist vs synthesize | **Persist at create** (ensure via D3a, then associate); synthesize on empty read only as fallback for pre-migration / orphaned rows | D3a removes the "no `default` row for API-created projects" obstacle; reporting `default` for a port with no group was an honest-looking lie (revised 2026-09-14, PR #76 review) |
| (b) orphaned SG on delete | **INNER JOIN drops orphans** | no FK on `server_security_groups.security_group_id` (`049:9-14`); unconditional SG delete (`neutron/ports.go:804-829`) |
| (c) list-detail N+1 | **Single `= ANY($1)` batch query** | Constitution perf + Simplicity |
