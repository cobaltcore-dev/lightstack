# Phase 1 Data Model: Tenant Scoping & Authorization Model

**Feature**: `023-tenant-scoping-authz` | **Date**: 2026-08-10

## Summary

**No schema change. No migration.** Every existing table already has the shape this feature needs; what changes is which value is written to one column, and which rows a query is allowed to return.

This is a consequence of two clarification decisions. Refusing domain scope (FR-009) removed the only requirement that could have needed storage — a way to express a role held at domain level. Recording attribution against the owning project (FR-007) reuses a column that already exists and is already `NOT NULL`.

---

## Entities

### Token scope *(in-memory only — never persisted)*

The authority boundary a token carries. Exactly two kinds exist, and this feature settles that there is no third.

| Kind | Representation | Carries |
|---|---|---|
| Project-scoped | `TokenClaims.ProjectID` populated | `user_id`, `user_name`, `project_id`, `roles` |
| Unscoped | `TokenClaims.ProjectID` empty | `user_id`, `user_name`, no roles |
| ~~Domain-scoped~~ | **refused at authentication** | — |

**Changes**:
- `ScopeField` (`internal/keystone/auth.go:55`) gains a top-level `Domain` member. It is **parsed so it can be detected and refused**, never acted upon.
- `TokenClaims` is **unchanged** — no domain field is added, because no domain-scoped token is ever issued.
- `AuthResponse.Token` is **unchanged** — no `Domain` member, for the same reason.

**Invariant**: `ScopeField.Project.Domain` (the qualifier nested inside a project scope) and the new top-level `ScopeField.Domain` are distinct. The former is the supported form every client sends and must continue to work untouched; only the latter triggers refusal.

**State transitions**: none. A scope is decided once, at authentication, and a token's scope never changes. Re-scoping issues a new token.

---

### Resource ownership *(unchanged)*

`instances.project_id` binds a server to exactly one project. This feature does not change ownership, who may hold it, or how it is assigned — only who may act across it.

**Invariant preserved**: every server has exactly one owning project, and the override never transfers, clears, or duplicates it. An administrator acting on another project's server leaves `instances.project_id` untouched.

---

### Role assignment *(unchanged)*

`role_assignments` binds `user_id` → `role_id` → `project_id`. Its project-only shape is the reason FR-009 refuses domain scope rather than honouring it: there is no domain-level grant to read.

`roles` is unconstrained — operators may define their own — so nothing may assume the seeded `admin`/`member`/`reader` set is exhaustive. Only `admin` is gated on, via the existing `common.RoleAdmin` constant.

---

### Instance action record

`instance_actions` — the tenant-visible history of what happened to a server.

| Column | Type | Change |
|---|---|---|
| `id` | `UUID` PK | — |
| `instance_id` | `UUID NOT NULL` | — |
| `action` | `VARCHAR(255) NOT NULL` | — (values extended, see below) |
| `request_id` | `VARCHAR(255)` | — |
| `user_id` | `UUID` | — (now identifies an operator acting cross-project) |
| `project_id` | `UUID NOT NULL` | **semantics change**: the **owning** project, not the caller's |
| `start_time` | `TIMESTAMP` | — |
| `message` | `TEXT` | — |

**Semantic change (FR-007)**: `project_id` previously always equalled the caller's project because the two could not differ. Once the override exists they can, and the column records the **owner**.

**Rationale**: the read path (`internal/nova/diagnostics.go:84`) selects by `instance_id` behind the instance ownership check, so the affected tenant reads their own server's history. Writing the caller's project would leave that history silently empty for exactly the operations the tenant most wants to see.

**Recognising a cross-project action**: no new column. An action is cross-project when the `user_id` holds no role in the `project_id` the row records — derivable from `role_assignments`.

**Coverage change (FR-007a)**: rows are currently written for `create` only (one insert site). The write path extends to every operation the override covers, using upstream's action vocabulary (`delete`, `stop`, `start`, `reboot`, `resize`, `confirmResize`, `revertResize`, `rebuild`, `pause`, `unpause`, `suspend`, `resume`, `lock`, `unlock`, `shelve`, `unshelve`, `migrate`, `live-migration`, `evacuate`, `resetState`, `changePassword`, `createImage`, `createBackup`, `restore`, `attach_volume`, `detach_volume`, `attach_interface`, `detach_interface`; a force delete is recorded as `delete`, as upstream records it).

**Validation**: `project_id` is `NOT NULL`, so the owner must be resolved before the insert. The override helper returns it, which is why it returns the owning project rather than a boolean.

---

### Usage report *(derived — no table)*

An aggregate over `instances` joined to `flavors`, grouped by project. It has no row of its own, and no owner column — which is how it came to have no ownership check at all.

**Changes**: none to storage. The aggregate form gains a role gate; the per-project form resolves its target through `common.QuotaTargetProject`.

**Validation added (FR-008d)**: a target that is malformed or names no project is refused with a client error. Today every error — including the `invalid input syntax for type uuid` a malformed target produces — is swallowed into a `200` reporting zeroes, indistinguishable from a real project owning no instances.

---

### Identity collections *(unchanged)*

`roles`, `users`, `projects`, `groups`, `services`, `endpoints`. No column, index or constraint changes. What changes is that a `WHERE` clause is built from query parameters that are currently discarded.

**Validation**: a filter matching nothing yields an empty collection — never the unfiltered one.

---

## Query-shape changes

The feature's substance is in which rows a query may return, not in what is stored.

| Shape | Before | After |
|---|---|---|
| Point lookup | `WHERE id = $1 AND project_id = $2` | administrator: `WHERE id = $1`; everyone else: unchanged |
| Listing | `WHERE project_id = $1` | administrator **with** the opt-in: predicate omitted; otherwise unchanged |
| Listing order | `ORDER BY created_at DESC` | `ORDER BY created_at DESC, id DESC` — total, so page boundaries are stable |
| Marker resolution | `WHERE id = $1 AND project_id = $2` | project predicate omitted under the cross-tenant view |
| Name matching | `WHERE (id::text = $1 OR name = $1) AND project_id = $2` | **project predicate retained even for administrators** (FR-005c) |
| Usage aggregate | `GROUP BY i.project_id`, no predicate | unchanged query, administrator-only |
| Usage per project | `WHERE i.project_id = $1` from the raw path parameter | `$1` resolved through the shared resolver first |
| Identity collections | no `WHERE` | `WHERE` built conditionally from present filters |

**Indexes**: no new ones. `idx_instances_project_id`, `idx_instances_project_status`, `idx_instances_created_at` and `idx_instance_actions_instance` already exist. Omitting the project predicate under a cross-tenant listing means `idx_instances_created_at` carries the ordering instead of the composite — acceptable at the deployment scale this product targets, and the default page size (1000) bounds it.

---

## What is deliberately *not* modelled

- **Domain-level role assignment** — FR-009 refuses domain scope, so no representation is needed.
- **An "acting project" column on the action record** — considered and rejected during clarification: `user_id` already answers "who", and adding a column would require a migration for information that is derivable.
- **A policy table** — one rule, expressed in code. An externally configurable policy engine is explicitly out of scope.
- **Soft-delete state on instances** — noted only because `GetLimits` filters `status != 'DELETED'` against a status nothing ever sets. Out of scope here; tracked as B7 in #547.
