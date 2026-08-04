# Phase 1 Data Model: Project/Tenant Lifecycle & Isolation Fixes

**Feature**: `022-project-tenant-isolation` | **Date**: 2026-08-04

## Verdict: one schema change, `groups.domain_id` (migration `072`)

Every column each requirement reads already exists. One is corrected: `groups.domain_id`, which migration
`010` created as `VARCHAR(255) DEFAULT 'default'` with no foreign key (see [Group](#group)). Nothing is
added or dropped. This document records *which* existing columns the feature leans on and *what
invariants* it starts enforcing, so the implementation cannot quietly need another migration later.

| Table | Columns read by this feature | Columns written | Change |
|---|---|---|---|
| `domains` | `id`, `name`, `enabled` | — | none |
| `projects` | `id`, `name`, `domain_id` | `id`, `name`, `description`, `domain_id`, `enabled`, timestamps (unchanged set) | none |
| `users` | `domain_id` | `domain_id` (now resolved, same column) | none |
| `groups` | `domain_id` | `domain_id` (now resolved) | `VARCHAR` → `UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE`, alias rows normalised (`072`) |
| `networks` | `id`, `project_id`, `shared` | — (never written by this feature) | none |
| `subnets` | — | unchanged insert set | none |
| `volumes` | — (routing only) | — | none |

---

## Entities

### Domain

The top-level identity container. Exactly one row is the deployment default.

**Schema** (`migrations/008_add_domains.up.sql`, re-declared idempotently by `037`):

| Column | Type | Notes |
|---|---|---|
| `id` | `UUID PRIMARY KEY` | **Cannot hold the string `default`** — this is why the alias must be resolved rather than stored (A1) |
| `name` | `VARCHAR(255) UNIQUE NOT NULL` | The default domain's name is `Default` (capital D) |
| `description` | `TEXT` | nullable |
| `enabled` | `BOOLEAN DEFAULT TRUE` | A disabled domain is not a valid creation target |
| `created_at`, `updated_at` | `TIMESTAMP` | |

**Referenced by**: `users.domain_id`, `projects.domain_id` (both
`UUID REFERENCES domains(id) ON DELETE CASCADE`, added by `008`), and — since `072` — `groups.domain_id`
the same way.

**Identity & uniqueness**: `name` is unique, which is what makes name-based resolution of the default
domain unambiguous (R2). `id` is the primary key.

**The three ways a request may name a domain** (FR-001, FR-001a) — all must resolve to the same row:

| Reference form | Resolution | Introduced by |
|---|---|---|
| omitted / empty | `WHERE name = 'Default' AND enabled = true` | FR-003 (behaviour preserved, mechanism changed) |
| `"default"` (the alias) | same query as omitted | FR-001 — **new** |
| a UUID | `WHERE id = $1 AND enabled = true` | existing (`CreateUser` only); FR-002 extends it to projects and groups |
| anything else | unresolved → client error | FR-002 |

**Seed inconsistency this feature neutralises** (R2): two migrations seed the same domain *name* with
different UUIDs. `009` wins by ordering; `037`'s insert is a no-op.

| Migration | UUID | Effect today |
|---|---|---|
| `009_seed_default_domain.up.sql` | `…000100` | wins — this is the live default domain |
| `037_keystone_domains.up.sql` | `…000001` | no-op (`ON CONFLICT (name) DO NOTHING`) |

Resolving by name makes the outcome independent of migration order. **No migration reconciles them**
(R9; `072` touches only groups) — the code stops depending on which one won.

**State transitions**: none introduced. `enabled` already toggles via `PATCH /v3/domains/:id`; this
feature only starts *honouring* it on the project and group creation paths, which is a read, not a
transition.

---

### Project (tenant)

The unit of resource ownership. Every resource in Lightstack is scoped to one.

**Schema** (`migrations/001_initial_schema.up.sql:11`, `domain_id` added by `008`):

| Column | Type | Notes |
|---|---|---|
| `id` | `UUID PRIMARY KEY DEFAULT gen_random_uuid()` | |
| `name` | `VARCHAR(255) UNIQUE NOT NULL` | **The constraint FR-004 leans on** — violating it raises SQLSTATE `23505`, today surfacing as a `500` |
| `description` | `TEXT` | |
| `enabled` | `BOOLEAN DEFAULT true` | |
| `domain_id` | `UUID REFERENCES domains(id) ON DELETE CASCADE` | **The FK that turns an unresolved alias into a `500`** — a non-UUID string fails the type cast before the FK is even consulted |
| `created_at`, `updated_at` | `TIMESTAMP` | |

**Validation rules newly enforced**:

| Rule | Requirement | Failure answer |
|---|---|---|
| `domain_id` must resolve to an existing, enabled domain | FR-002 | `400`, message names the domain field |
| `name` must not collide with an existing project | FR-004 | `409`, message names the duplicate |
| Nothing is persisted before both checks pass | FR-002 | — |

**Not changed**: project creation remains ungated by role (A6/FR-023). Upstream restricts it to
administrators; that is a policy decision moved to #549.

---

### Group

**Schema before** (`migrations/010_keystone_user_enhancements.up.sql`): `domain_id VARCHAR(255) DEFAULT
'default'`, `UNIQUE(name, domain_id)`, **no foreign key**. The `UUID REFERENCES domains(id)` definitions
in `019` and `028` never applied — both are `CREATE TABLE IF NOT EXISTS` against the table `010` had
made. So the old handler stored whatever reference it was given: the alias verbatim, a string naming no
domain, and the same name twice in one domain (once under the alias, once under the id).

**Schema after** (`072_groups_domain_id_uuid`): `domain_id UUID NOT NULL REFERENCES domains(id) ON
DELETE CASCADE`, matching `users` and `projects`; `UNIQUE(name, domain_id)` unchanged.

**Existing rows**, normalised only where the answer is not a guess:

| Stored value | Becomes |
|---|---|
| `'default'`, `''`, `NULL` | the Default domain's id |
| an existing domain's id, any letter case | that id, canonical form |
| anything else (a string or id naming no domain) | migration **stops**, naming the rows |
| a name that would then appear twice in one domain | migration **stops**, naming it |

A stopped migration changes nothing — the file runs as one transaction — but golang-migrate marks
version 72 dirty: fix the named rows, set `schema_migrations` back to version 71 with `dirty = false`,
and restart.

---

### Network

**Schema** (`migrations/001_initial_schema.up.sql:73`):

| Column | Type | Role in this feature |
|---|---|---|
| `id` | `UUID PRIMARY KEY` | The subnet request's target |
| `name` | `VARCHAR(255) NOT NULL` | — |
| `project_id` | `UUID REFERENCES projects(id) ON DELETE CASCADE` | **The owner.** The single fact `CreateSubnet` never consults today |
| `shared` | `BOOLEAN DEFAULT false` | Distinguishes the `403` case from the `404` case (FR-010a) |
| `admin_state_up`, `status`, `mtu`, timestamps | | — |

**Invariant this feature installs** (FR-009, FR-010): a subnet may be attached to a network only when
the caller owns that network or holds the `admin` role. `shared` grants *use*, never *definition* of
address ranges.

**Read, never written**: the feature adds one `SELECT project_id, shared FROM networks WHERE id = $1`
and no write. A refused request leaves the target network byte-identical (FR-010, SC-003).

---

### Subnet

**Schema** (`migrations/001_initial_schema.up.sql:85`):

| Column | Type | Role in this feature |
|---|---|---|
| `id` | `UUID PRIMARY KEY` | |
| `name` | `VARCHAR(255) NOT NULL` | |
| `network_id` | `UUID REFERENCES networks(id) ON DELETE CASCADE` | **Taken straight from the request body today**, with no ownership check. A nonexistent id hits this FK and surfaces as `500` (FR-011) |
| `project_id` | `UUID REFERENCES projects(id) ON DELETE CASCADE` | Set from the caller's token — so a cross-tenant write produces a subnet owned by the attacker but attached to the victim's network |
| `cidr` | `VARCHAR(50) NOT NULL` | |
| `gateway_ip` | `VARCHAR(50)` | Defaulted to the first address when omitted |
| `ip_version` | `INTEGER DEFAULT 4` | |
| `enable_dhcp` | `BOOLEAN DEFAULT true` | Triggers `applyDHCPForNetwork` — the side effect that must not run on a refused request |
| `dns_nameservers` | `TEXT[]` | |
| `created_at`, `updated_at` | `TIMESTAMP` | |

**The defect, stated as a data fact**: `project_id` and `network_id` come from *different* sources —
the token and the request body — and nothing has ever required them to be consistent. That mismatch is
the whole of B8.

**Write set unchanged**: the same eleven columns, the same values, for a request that is now checked
first. A tenant creating a subnet on their own network sees no difference (FR-013).

**Adjacent paths** (R5) — no invariant is needed for the write, but two reads changed (FR-013a):

| Path | Scoping | Can it move a subnet between networks? |
|---|---|---|
| `UpdateSubnet` (`network.go:1296`) | joins `networks`; `(s.project_id = $2 OR n.project_id = $2)` | No — accepts only `name` |
| `GetSubnet` (`network.go:1189`) | `(s.project_id = $2 OR n.project_id = $2 OR n.shared)` | N/A |
| `DeleteSubnet` (`network.go:1234`) | joins `networks`; `(s.project_id = $2 OR n.project_id = $2)` | N/A |
| bulk subnet create | does not exist — `CreateSubnetRequest` is a single object | N/A |

All three handlers, plus `ListSubnets`, report `tenant_id` from `s.project_id` rather than from the
token: each can return a row the caller does not own, so the caller is not a safe stand-in for the
owner. `CreateSubnet` is the exception and stays as it is — the caller *is* the project it wrote.

The three `n.project_id` disjuncts are upstream's `network_owner` rule, which `ListSubnets` already
applied. They exist because FR-012 (administrators may add a subnet to any network) makes a subnet
whose project differs from its network's a reachable state — see FR-013a. `n.shared` appears only in
the read, matching upstream. The `UPDATE` and `DELETE` statements themselves carry **no** project
predicate: the entitlement query above them has already decided, and duplicating it there made the
owner's call a no-op that still answered success.

---

### Volume

Touched only by routing. No column is read or written by this feature.

**Relevant fact**: every handler in Cinder's `/v3/:project_id` group ignores the `:project_id` path
segment and reads `project_id` from the token (`c.GetString("project_id")`). That is what makes FR-015
true by construction — the path segment cannot widen visibility because nothing reads it — and what
makes adding `GET /volumes` there behaviourally identical to the flat route rather than a new code
path.

---

## Cross-entity constraints introduced

| # | Constraint | Enforced where | Requirement |
|---|---|---|---|
| C1 | A project's `domain_id` must reference an existing, enabled domain | `CreateProject`, before insert | FR-002 |
| C2 | A user's and a group's `domain_id`: same rule | `CreateUser`, `CreateGroup` | FR-005, FR-006 |
| C3 | A project's `name` is unique, reported as a conflict rather than a fault | `CreateProject`, on SQLSTATE `23505` | FR-004 |
| C4 | A subnet's `network_id` must name a network the caller owns, or the caller must be an administrator | `CreateSubnet`, before insert and before any DHCP work | FR-009, FR-010, FR-012 |
| C5 | A subnet's `network_id` must be a well-formed identifier naming an existing network | `CreateSubnet` | FR-011, FR-011a |

All five are **application-level** invariants. None becomes a database constraint: C1/C2 are already
backed by foreign keys that the application must consult *first* to produce a decent error, C3 is
already a unique index whose violation merely needs better handling, and C4 is a cross-row rule
(`subnets.project_id` vs `networks.project_id`) that a table constraint cannot express without a
trigger — which the constitution's simplicity and anti-abstraction articles both rule out.

## Scale and volume

Unchanged. The feature adds, per affected request:

| Path | Added work |
|---|---|
| Project / user / group create | one indexed lookup on `domains` (`name` or `id`, both indexed — `idx_domains_name`, PK) |
| Subnet create | one primary-key lookup on `networks` |
| Domain retrieve / list | none — the same query, with a resolved id or an added `WHERE name = $1` |
| Volume list (project-scoped) | none — an additional route to an existing handler |

No new table, no new index, no query that scans. Well inside the constitution's `< 100ms` standard.
