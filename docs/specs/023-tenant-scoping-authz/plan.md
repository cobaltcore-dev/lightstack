# Implementation Plan: Tenant Scoping & Authorization Model

**Branch**: `feature/549` (feature `023-tenant-scoping-authz`) | **Date**: 2026-08-10 | **Spec**: [spec.md](./spec.md)  
**Input**: Feature specification from `docs/specs/023-tenant-scoping-authz/spec.md`  
**Base**: `feature/545` @ `9992ebf` — tip of the stacked chain (549 ← 545 ← 534 ← 533 ← 532 ← 482 ← 524 ← 481 ← 515 ← 480 ← main)

## Summary

Finish applying an authorization rule the product already has, and stop answering one question dishonestly.

The administrative override exists in three services and in compute's console; compute's core server operations are the outlier. Two compute operations gated to administrators *only* — evacuate and reset-state — then restrict themselves to the administrator's own project, which makes them unusable for the situations they exist to handle. Separately, a domain-scoped authentication request is answered `201` with a token carrying no scope and no roles.

The approach is deliberately unambitious: reuse the two override shapes already in the codebase (`lookupConsoleInstance` for scope-derived targets, `common.QuotaTargetProject` for parameter-derived ones), refuse domain scope in one place rather than build a domain authorization layer, and copy the query-filter shape #545 established on `ListDomains`. **No new dependency, no schema change, no migration.**

Clarification settled seven decisions and grew the feature from three user stories to five: it surfaced a live cross-tenant usage leak and a family of identity list endpoints that discard every filter. Planning then found three more surfaces of the same family, and the decision was taken to fold them in — including the feature's only privilege escalation, host aggregates, where nine handlers carry no role check at all and six of them are writes. Six user stories, 40 requirements.

## Technical Context

**Language/Version**: Go 1.26 (repo pins `go 1.26`; Constitution requires 1.21+)  
**Primary Dependencies**: Gin (HTTP), pgx/pgxpool (PostgreSQL), golang-jwt/v5. **No new modules.** Contract tests use the already-pinned `gophercloud v1.14.1`.  
**Storage**: PostgreSQL — **no schema change, no migration**. `instance_actions.project_id` is already `UUID NOT NULL`; the change is which value is written to it.  
**Testing**: `go test` (unit), `test/contract/{nova,keystone}` (gophercloud), bash suites in `test/` (`multi_tenant_test.sh` is the primary vehicle)  
**Target Platform**: Linux server; **stub mode is sufficient for every behaviour in this feature** — nothing here touches libvirt, netlink or storage backends  
**Project Type**: Single Go binary, six embedded HTTP services (Constitution Art. VI)  
**Performance Goals**: Unchanged. The override adds one branch per handler; a cross-tenant listing omits a predicate. Existing indexes cover both.  
**Constraints**: Backwards compatibility is a hard constraint (spec A-006) — no request valid today may receive a different answer except where a requirement names the change. The `scope.project.domain` nested qualifier is the sharpest instance.  
**Scale/Scope**: 44 compute server-operation handlers, 2 list handlers, 2 usage handlers, 10 deployment-scoped handlers (1 migration list + 9 aggregate), 4 per-server migration handlers, 1 authentication handler, 6 identity list handlers. ~69 call sites across `internal/nova`, `internal/keystone`, `internal/common`.

## Constitution Check

*GATE: evaluated before Phase 0 and re-evaluated after Phase 1.*

| Article | Verdict | Notes |
|---|---|---|
| **I. OpenStack Flamingo API compatibility** (NON-NEGOTIABLE) | ✅ **PASS** | Moves *toward* upstream on every count. `admin_or_owner` is upstream's default server policy; `all_tenants` is upstream's own opt-in with upstream's parsing rules; `server_id` as a UUID is upstream's contract; the identity filters are upstream-documented. Two decisions were verified against the Nova API reference rather than assumed. |
| **II. Synchronous operations** | ✅ **PASS** | No queues, no async, no state machines. Every change is inside the existing request path. |
| **III. Fail-fast** | ✅ **PASS** | No retries or backoff introduced. The usage endpoints stop *swallowing* errors (FR-008d), which moves toward fail-fast, not away. |
| **IV. Multi-mode architecture** | ✅ **PASS** | Authorization is mode-independent; every behaviour is provable in stub mode. No `libvirtMode`/`networkingMode` branch is added. |
| **V. Project isolation** | ✅ **PASS — this feature is Article V** | The article already says "no cross-project resource access **without explicit admin role check**". This adds the explicit check that clause anticipates, and closes four places where isolation was absent entirely — including one, host aggregates, where a tenant could *write*. |
| **VI. Single binary** | ✅ **PASS** | No new process, service or port. |
| **VII. Test-first** | ✅ **PASS** | Contracts written in Phase 1 before implementation; every defect has a documented before/after in `quickstart.md`. `rbac_test.go.broken` is revived as a RED test. |
| **Backwards compatibility** | ✅ **PASS** | Enforced by FR-016/FR-017 and guarded by contract S4/S5 and L1. No migration, so the reversibility requirement is vacuous. |
| **Security requirements** | ✅ **PASS** | No credential handling changes. Three confidentiality defects and one privilege escalation are closed. |

**Result: PASS — no deviations, no Complexity Tracking entries.**

**Post-Phase-1 re-evaluation: still PASS**, and re-confirmed after User Story 6 was folded in. The design added two files (`internal/common/query.go`, `internal/nova/authz.go`), both small, both in existing packages, neither introducing a pattern the repo lacks. User Story 6 added no file at all — it is `common.HasRole` at ten more call sites. No gate moved. The one thing worth re-testing was Article I: designing the `all_tenants` parsing table (R2) and the name-matching boundary (R10) against the Nova API reference *strengthened* the compatibility case rather than straining it — both were verified from upstream documentation rather than assumed.

Worth stating explicitly, because the first-pass spec assumed otherwise: this feature introduces **no new abstraction**. Both override shapes, the role helper, the role constant and the filter shape all exist on this base.

## Project Structure

### Documentation (this feature)

```text
docs/specs/023-tenant-scoping-authz/
├── spec.md                        # clarified, 8 decisions, 0 open questions
├── plan.md                        # this file
├── research.md                    # Phase 0 — R1–R11
├── data-model.md                  # Phase 1 — no schema change
├── quickstart.md                  # Phase 1 — before/after per defect, stub-only
├── contracts/                     # Phase 1
│   ├── compute-override.md        # C1–C8   (US1)
│   ├── compute-listing.md         # L1–L6   (US1)
│   ├── identity-scope.md          # S1–S9   (US2)
│   ├── usage-and-filters.md       # U1–U4, F1–F6 (US3, US5)
│   └── operator-surfaces.md       # O1–O6   (US6)
├── checklists/requirements.md     # all-pass
└── tasks.md                       # NOT created by this command
```

### Source code (repository root)

```text
internal/
├── common/
│   ├── roles.go                   # HasRole, RoleAdmin — reused unchanged
│   ├── project.go                 # QuotaTargetProject — reused for usage
│   └── query.go                   # NEW: upstream-vocabulary bool query flag (R2)
├── nova/
│   ├── authz.go                   # NEW: the scope-derived override helper (R1)
│   ├── console.go                 # lookupConsoleInstance → calls the shared helper
│   ├── handlers.go                # GetServer, DeleteServer, ServerAction, metadata,
│   │                              #   ListServers, ListServersDetail, action recording
│   ├── advanced_actions.go        # 17 actions incl. Evacuate + ResetState (FR-008a)
│   ├── tags.go, diagnostics.go, server_update.go
│   ├── volume_attachment.go, interface_attach.go
│   ├── migrations.go              # list → admin gate; per-server → ownership + override
│   ├── aggregates.go              # NEW gate on all 9 handlers — 6 of them writes
│   └── tenant_usage.go            # US3
└── keystone/
    ├── auth.go                    # ScopeField gains a parsed-but-refused Domain
    ├── handlers.go                # single rejection point; ListRoles/Users/Projects
    ├── groups.go, services.go     # remaining identity filters
    └── domains.go                 # unchanged — the shape being copied

test/
├── contract/nova/rbac_test.go     # revived from .broken, extended cross-project
├── contract/nova/tenant_usage_test.go
├── contract/keystone/roles_test.go
└── multi_tenant_test.sh           # override coverage + deliberately-red check

docs/
├── blueprint.md                   # :592, :594, :588, :149
├── KEYSTONE_AUTH_FLOW.md
└── compatibility/OPENSTACK_COMPATIBILITY_REPORT.md
CLAUDE.md                          # JWT Authentication / Project Isolation notes
```

**Structure Decision**: existing layout, unchanged. Two new files, both small and both in packages that already exist: `internal/common/query.go` (the bool-flag helper) and `internal/nova/authz.go` (the override helper). Everything else is edits to files already carrying the logic being changed. No new package, consistent with Constitution Art. VI and VII.

## Implementation sequencing

Ordered so the riskiest work lands on top of proven foundations, and so the two independently-valuable pieces can ship early.

| Wave | Content | Depends on | Why here |
|---|---|---|---|
| **W1** | `internal/common/query.go`; `internal/nova/authz.go`; revive `rbac_test.go.broken` (RED) | — | Foundations plus the failing test that proves W2. |
| **W2** | **FR-008a** — evacuate + reset-state reach other projects | W1 | Carries no policy decision and is independently valuable. Turns the revived test GREEN. |
| **W3** | **US6** operator-only surfaces — the aggregate gate, the migration-list gate, per-server migration ownership | W1 (per-server migrations only) | **Scheduled early because it is the highest-severity item in the feature** — the only place a tenant can *change* something that is not theirs — and among the smallest: one check at nine call sites. Its two gates depend on nothing at all. Deferring a P1 privilege escalation behind three waves of P1 capability work would be scheduling by convenience rather than by risk. |
| **W4** | Override across the 44 server operations; console adopts the shared helper | W1, W2 | The bulk. Mechanical once W1 exists. |
| **W5** | `all_tenants` on both list handlers; paging tiebreaker; owning-project attribution + extended action write path; refusal logging | W1, W4 | Needs the helper and the widened lookups. |
| **W6** | **US3** usage endpoints | W1 | Independent of W2–W5. |
| **W7** | **US2** domain-scope refusal | — | Fully independent of everything above. |
| **W8** | **US5** identity filters | — | Fully independent. The cleanest split-out if the branch needs trimming. |
| **W9** | Suites, docs, `make test-all` | all | FR-019–FR-027. |

W3, W6, W7 and W8 have no dependency on each other. W3's two gates need nothing beyond the baseline; only its per-server migration work needs W1.

**Wave order matches the story priorities in spec.md and the phase order in tasks.md**: the two P1 stories (US1, US6) come first, then P2 (US3, US2), then P3 (US5, and validation in W9).

## Risks

| # | Risk | Mitigation |
|---|---|---|
| **1** | The domain-scope check catches the **nested** `scope.project.domain` qualifier, breaking authentication for every client | Contract S4 is a dedicated regression guard; the check tests a top-level member only. Highest-severity risk in the feature — a break here is total. |
| **2** | 44 handlers touched by hand; one missed leaves an inconsistent rule (SC-002) | One shared helper, and a static check that no in-scope handler still reads `GetString("project_id")` directly for a point lookup. |
| **3** | Widening a lookup without widening its sub-lookups | Already found one: `getInstanceAddresses` scopes the ports query to the caller, so an admin would see a server with no network. Recorded as a spec edge case; the sweep must check every sub-query. |
| **4** | Cross-project delete leaves dangling state the owning tenant can see | Console tokens (#534), ports, volume attachments. Covered by the spec's edge case; exercised in `multi_tenant_test.sh`. |
| **5** | `make test-all` is already red on B10 (#546) | Pre-existing and unrelated. Record the baseline before starting so this feature is not blamed for it. |
| **6** | The aggregate action route dispatches to three handlers plus an unrecognised-action default; a gate on the recognised branches only would leave the default path open | Gate at the dispatcher, not per action. Contract O3 covers the unrecognised-action case explicitly. |
| **7** | FR-008e/FR-008f **remove** access callers have today — the only such changes in the feature | Verified no supported client depends on it (Horizon reaches both only from its Admin panel). Existing contract tests authenticate as `admin` via `setupNovaClient` and are unaffected. Contract O5 guards administrator behaviour. |
| **8** | FR-008g tightens a per-server path for non-administrators, unlike every other change | Called out in its own contract (O4) so it is not mistaken for an override that merely widens. |

## Discovered during planning — folded in

Research turned up three more defects of the same family. All three are **in scope**, by decision taken after planning surfaced them. They are specified as User Story 6 (FR-008e–FR-008g) and designed in research R11.

| # | Finding | Requirement |
|---|---|---|
| **D1** | The four per-server migration handlers have **zero** project predicates and **zero** role checks. Any tenant can read, act on and **delete** another tenant's server migration records. | FR-008g — ownership check *plus* the override. The only in-scope change that tightens a per-server path for non-administrators. |
| **D2** | `GET /os-migrations` returns every project's migrations to any authenticated caller. Its own source comment records that this is "an admin endpoint in real OpenStack". | FR-008e — administrative gate. Same shape as the usage aggregate. |
| **D3** | `internal/nova/aggregates.go` has **no role check at all** across 9 handlers, 6 of them writes: create, update, delete, add host, remove host, set metadata. | FR-008f — administrative gate on every one, placed at the action dispatcher so no unrecognised-action path stays open. |

**Why they belong here rather than in a follow-up**: they are the same rule this feature exists to apply. Shipping "administrators act across projects, tenants do not" beside three surfaces that contradict it would leave the rule stated but not true. D3 in particular is the feature's only privilege escalation — every other defect is a missing capability, a dishonest answer, or a read that leaks.

**Root cause worth recording**: every other handler in `internal/nova` was written against a table carrying a `project_id` column, so a scoping predicate was the obvious thing to write. Aggregates and the deployment-wide migration listing have no such column — so no predicate was written, and nothing was written in its place. The gap follows from the schema, which is why it escaped both review and the ticket's own `HasRole` count.

## Complexity Tracking

No Constitution violations. Table intentionally empty.
