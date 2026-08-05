# Implementation Plan: Horizon compatibility — create/write UI enablement (F2, F3, F7, F8)

**Branch**: `feature/533` (feature id `020-horizon-create-ui-enablement`) | **Date**: 2026-07-27 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/020-horizon-create-ui-enablement/spec.md`

## Summary

Horizon's **read** panels work against O3K but every **create/write** UI fails (SPEC-018 findings F2/F3/F7/F8),
even though the same operations succeed via the API/CLI. The cause is one shared root: Horizon loads
**quota + limits + capability + type metadata up front** and hides create/launch actions, blanks the Overview
widget, and refuses to open modals when that metadata is missing, inconsistent, or unrenderable. The endpoints
largely exist — the defects are **gaps and inconsistencies in that read surface**.

Fourteen gaps were confirmed by code inspection of this branch (research [R1](./research.md#r1--confirmed-code-level-gaps-from-inspection-of-this-branch)).
The work, per service:

- **Nova** — reconcile absolute limits to the `quotas` table at the upstream values (10/20/51200), replacing
  four duplicated default maps (one of which shadows the others with 10× values) with a single package helper;
  add the missing `os-quota-sets/{id}/detail`; populate three usage counters hardcoded to `0`.
- **Neutron** — add the missing `quotas/{id}/default` and a `.json` form of `quotas/{id}/details`; make the
  quota response project-scoped and carry `project_id`/`tenant_id`; advertise the `quota_details` extension so
  Horizon takes the single deterministic usage call instead of its multi-call fallback; add `project_id` to
  network responses and align subnet-reference scope so the launch wizard can render selectable rows.
- **Cinder** — add the entirely missing `/v3/extensions` (a real Cinder v3 endpoint, and the prime suspect for
  the dead Create Volume modal), plus the `?usage=true` view and `/defaults`; make limits read `cinder_quotas`
  so a quota override actually affects them; de-duplicate the two byte-identical limits handlers.
- **Cross-cutting** — quota reads scope to the caller's project with an `admin` override via the existing
  `common.HasRole` helper (FR-002a); first-ever tests for these endpoints (there are none today).

Per the ticket's explicit instruction, a **trace of what Horizon actually calls gates the implementation**
(FR-014): the confirmed gaps are the starting hypothesis, not an assumed-complete list.

**No new Go modules. No migration and no schema change** — the `quotas` (migration `007`) and `cinder_quotas`
(`039`) tables already exist and are sufficient. Verified in **both** stub and real modes.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: gin-gonic/gin (HTTP), jackc/pgx/v5 + pgxpool (PostgreSQL). **No new modules.**
**Storage**: External PostgreSQL. **No migration, no schema change** — reuses `quotas` (migration `007`, row-per-resource, `project_id UUID NOT NULL REFERENCES projects(id)`) and `cinder_quotas` (migration `039`). Neutron has no quota table and does not gain one (limits stay code defaults; usage is counted live).
**Testing**: Three layers, all existing tooling. (1) Go unit tests via each package's `httptest` router harness (`newNovaTestRouter` / `newNeutronTestRouter` / `newCinderTestRouter`, `X-Test-*` headers for scope; `requireDB(t)` + `O3K_TEST_DB_URL` for DB-backed cases) in the CI unit-tests job. (2) A bash endpoint probe in `test/` following the `test/horizon_compat_test.sh` pattern. (3) The **pre-existing** contract/script suites that already cover these endpoints — `make test-contract` (gophercloud) plus `test/quota_test.sh`, `test/quota_admin_test.sh`, `test/horizon_compat_test.sh` — which `make test` deliberately excludes and which FR-016 requires to keep passing. Plus two manual runbook passes (stub + real) per SPEC-018.
**Target Platform**: Linux for real mode (libvirt/KVM, Rook/Ceph RGW); stub mode is cross-platform. Both modes in scope.
**Project Type**: Single ~35 MB Go binary with six embedded services (`cmd/o3k/main.go`). No new project or package.
**Performance Goals**: Unchanged — these are response-shape/routing changes on metadata paths (< 100 ms per Art. Performance Standards). The one added cost is a small per-project quota `SELECT` in handlers that previously used constants; it mirrors queries those handlers already run.
**Constraints**: OpenStack Flamingo (2025.2) API compatibility (Art. I, NON-NEGOTIABLE); synchronous (Art. II); fail-fast (Art. III); stub **and** real parity (Art. IV); project isolation preserved and *strengthened* (Art. V). Fixes are **O3K-side only against stock, unpatched Horizon** (FR-018).
**Scale/Scope**: 3 services, ~6 files (`internal/nova/{quotas,handlers}.go`, `internal/neutron/network.go`, `internal/cinder/{quotas,volumes}.go`), plus tests and one probe script. No migration.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Assessment |
|---|---|
| **I. Flamingo API Compatibility (NON-NEGOTIABLE)** | ✅ **This feature exists to restore it.** Every change moves a response toward the Flamingo contract: the Compute/Block-Storage quota `detail`+`defaults` variants, the Networking `quotas/{id}/default` and `quota_details` extension, Cinder v3 `/extensions` (a real upstream endpoint O3K omits), `project_id` on Neutron networks, and absolute limits at the **upstream default values**. Deliberately **not** added: Nova `/extensions`, which upstream removed at microversion 2.36 — adding it would *diverge*. Verified with real clients (Horizon, python-openstackclient) in both passes. |
| **II. Synchronous Operations** | ✅ No async introduced. All changes are reads completed within the request. |
| **III. Fail-Fast Design** | ✅ No retries/backoff. Unauthorized cross-project quota reads now fail immediately and explicitly rather than silently serving another project's data. |
| **IV. Multi-Mode Architecture** | ✅ Every change is a response-shape/routing change with **no** `stub`/`real` branch, so behavior is identical in both modes; both are verified (FR-017). Several new handlers touch no DB at all, so part of the test suite runs with no database. |
| **V. Project Isolation** | ✅ **Strengthened.** FR-002a scopes quota reads to the caller's `project_id` with an `admin` override via the existing shared `common.HasRole` helper, closing a current cross-project read (Cinder's quota-set endpoints have no authz check at all today). |
| **VI. Single Binary Deployment** | ✅ Unchanged — same binary, same six services, no new package. |
| **VII. Simplicity / VIII. Anti-Abstraction** | ✅ **Net simplification**: four duplicated Nova default maps and two Cinder ones collapse into one package-local helper each; two byte-identical Cinder limits handlers become one. No new abstraction layer, no cross-service quota package, no new table. |
| **Test-First** | ✅ Tests written RED first (FR-015). Note these endpoints have **zero** test coverage today, so this is net-new safety. |
| **Backwards Compatibility** | ✅ Additive: new routes, new keys, `project_id` **alongside** the existing `tenant_id`. No migration to reverse. The one intentional value change is Nova's absolute limits (FR-003) — the defect being fixed. |
| **Forbidden patterns** | ✅ None used (no queues, workers, state machines, service discovery). |

**Result: PASS — no violations.** Complexity Tracking is intentionally empty.

**Post-Phase-1 re-check (after data-model + contracts): PASS, unchanged.** The design added no schema, no
table, no package, and no abstraction; it *removed* six duplicated default maps and one duplicated handler.
Three points were surfaced by the design and are deliberately, visibly handled rather than hidden:

1. **Art. I, a divergence avoided** — Nova `/extensions` is *not* added (upstream removed it at microversion
   2.36); Cinder `/v3/extensions` *is* (upstream serves it). Capability lists advertise only what O3K
   genuinely implements.
2. **Art. Backwards-Compat, one intentional value change** — Nova's absolute limits move 100/200/512000 →
   10/20/51200. That is the defect being fixed (FR-003, spec Clarification 3), it is additive-safe for clients
   (a *lower* advertised limit with real usage below it still enables actions), and it is asserted by test.
3. **Art. V, resolved by clarification (2026-07-27)** — Cinder's `PUT`/`DELETE /v3/quota-sets/{id}` have
   **no** authorization at all today, so any authenticated caller can rewrite or reset any project's quota.
   These become **admin-only** (FR-020), matching Nova and upstream Cinder, and Nova's `UpdateQuotaSet` moves
   its admin check ahead of body binding (403 instead of 400 for a non-admin). Both are deliberate, tested
   exceptions to FR-016. Alongside, **FR-019** unifies every role check onto the existing shared
   `common.HasRole`, removing four hand-rolled loops and Keystone's redundant wrapper — a further Art. VII/VIII
   simplification that also removes the temptation to add a fifth copy here.

## Project Structure

### Documentation (this feature)

```text
docs/specs/020-horizon-create-ui-enablement/
├── plan.md              # This file
├── spec.md              # Feature specification (+ 5 Clarifications)
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output (endpoint/response contracts)
│   ├── nova-limits-and-quota-sets.md
│   ├── neutron-quotas-and-extensions.md
│   ├── cinder-extensions-and-quota-sets.md
│   └── neutron-network-selectability.md
├── checklists/
│   └── requirements.md
└── tasks.md             # Phase 2 output (/cl.tasks — NOT created here)
```

### Source Code (repository root)

Existing single-project Go layout; no new packages.

```text
internal/
├── nova/
│   ├── quotas.go        # single limits helper (defaults ⊕ `quotas` rows); GetQuotaSet/Defaults use it;
│   │                    #   new GetQuotaSetDetail; caller-scoped target project (admin override)
│   └── handlers.go      # GetLimits reads the helper (drops the shadowing 100/200/512000 map);
│                        #   real totalFloatingIpsUsed / totalSecurityGroupsUsed / totalServerGroupsUsed;
│                        #   route: + /os-quota-sets/:id/detail
├── neutron/
│   └── network.go       # one shared quota-defaults map; GetQuota project-scoped + project_id/tenant_id;
│                        #   new GetQuotaDefault; `.json` route aliases; advertise `quota_details`;
│                        #   network responses gain project_id; subnet-reference scope aligned
└── cinder/
    ├── quotas.go        # single limits helper (defaults ⊕ `cinder_quotas`); `?usage=true` view + defaults handler;
    │                    #   caller-scoped target project (admin override); PUT/DELETE become admin-only (FR-020)
    └── volumes.go       # GetLimits reads cinder_quotas via the helper; GetLimitsNoProject delegates;
                         #   new ListExtensions + routes (registered BEFORE the /v3/:project_id group)

# FR-019 — unify every role check onto the existing shared helper (repo-wide, behavior-preserving)
internal/common/roles.go              # unchanged — the single implementation
internal/nova/quotas.go               # :166 hand-rolled loop  → common.HasRole
internal/nova/advanced_actions.go     # :478 and :1224 loops   → common.HasRole
internal/cinder/volumes.go            # :980 loop (VolumeAction os-reset_status) → common.HasRole
internal/keystone/handlers.go         # delete the pass-through wrapper hasRole (:147)
internal/keystone/{handlers,ec2_credentials}.go  # its 5 call sites → common.HasRole directly

# Tests — new files
internal/nova/quotas_test.go            # limits⇄quota-set agreement; detail shape; scoping/admin override
internal/neutron/quotas_test.go         # default/details(+.json); project scoping; quota_details advertised
internal/cinder/quotas_test.go          # extensions; ?usage=true/defaults; limits honor cinder_quotas; write authz
test/horizon_create_ui_test.sh          # endpoint probe (doubles as the FR-014 trace tool) + SC-005 guard

# Tests — existing files this feature extends
internal/neutron/network_test.go        # project_id present; every `subnets` id resolves in the subnet list
internal/common/roles_test.go           # EXISTS (TestHasRole) — add only the wrong-typed roles-claim case
internal/cinder/volumes_test.go         # EXISTS — add the route-registration (gin ordering) smoke test

# Pre-existing regression gate — NOT run by `make test`; must be run explicitly (FR-016)
test/contract/nova/limits_test.go       # gophercloud; value-agnostic assertions, authenticates as admin
test/contract/cinder/quotas_test.go     # gophercloud; covers quota-set GET/PUT/DELETE
test/contract/neutron/quotas_test.go    # gophercloud; covers the network quota
test/quota_test.sh, test/quota_admin_test.sh, test/horizon_compat_test.sh

docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md   # tag F2/F3/F7/F8 fixed (mirrors the F4/F5/F6/F9 block)
```

**Structure Decision**: Reuse the existing single-binary layout (Art. VI/VII). Each fix stays inside the
service package that owns the endpoint; the only cross-service element is the shared `common.HasRole` helper
that already exists. **No migration directory changes** — unlike SPEC-019, this feature needs no schema work.

### Route additions (exact placement constraints)

Gin imposes hard ordering rules discovered during research; getting these wrong panics at startup:

| Service | New routes | Constraint |
|---|---|---|
| Nova | `GET /v2.1/os-quota-sets/:id/detail` | must reuse the param name `:id` (a differently-named param at that position panics) |
| Neutron | `GET /v2.0/quotas/:id/default`, `+ .json` aliases for `details`/`default` | static children of the existing `:id` — safe. A bare `/quotas/default` would be ambiguous, so `default` is handled *inside* the `:id` handler (nova's existing idiom) |
| Cinder | `GET /v3/extensions`, `GET /v3/quota-sets/:id/defaults` (+ the `/v3/:project_id` variants — see the note below on why the group is mirrored); usage via `?usage=true` on the show route, **not** a `/detail` path (FR-002 amended) | registration **order is irrelevant** — gin splices a static child ahead of an existing wildcard, verified in both orders against the vendored v1.12.0. What *does* panic is reusing a different param name at a position already claimed by one, so every route uses `:id` |

**Why every new Cinder resource is registered twice.** A branch review flagged the `/v3/:project_id`
duplicates as speculative surface. They are not: that group **predates this feature by 43 routes**, Cinder's
canonical upstream path carries the project (`/v3/{admin_project_id}/os-quota-sets/{project_id}`), and the
single gap in the group — `GET /v3/{project}/volumes`, registered as `POST` but not `GET` — is filed as a
real defect after a client hit the 405 (cloud-storage#536 B2). Omitting the group form for groups and
quota-sets would reproduce that same class of bug, so mirroring is the consistent choice. The `os-quota-sets`
spelling is separately justified: it is upstream's, it is what python-cinderclient requests, and Horizon
returned 500 without it (finding N3).

## Implementation Phases

**Phase A — Trace (gates everything; FR-014).** Drive each of the four create UIs against the stub stack and
capture O3K's request log plus Horizon's apache error log to produce a **confirmed call map** (path → status)
per panel. Reconcile it against the 14 confirmed gaps; fold in anything new. The probe script written here is
kept as the SC-005 regression guard.

**Phase B — Implement, per service, tests first.** Nova (limits⇄quota reconciliation + `detail` + usage
counters) → Neutron (quota `default`/`.json` + scoping + `quota_details` + network `project_id` + subnet-scope
alignment) → Cinder (`/extensions` + quota `?usage=true`/`defaults` + limits honoring `cinder_quotas`). Each unit
test lands RED before its fix.

**Phase C — Verify in both modes (FR-017, SC-006).** Re-run the SPEC-018 stub and real runbooks, observing all
four workflows; confirm the launched instance actually boots in real mode; update the compatibility report.

**Mapping to `tasks.md`**: Phase A → tasks Phase 1; Phase B → tasks Phases 2–6 (Foundational + one phase per
user story); Phase C → tasks Phase 7. Detailed, dependency-ordered tasks live in
[tasks.md](./tasks.md).

## Complexity Tracking

> No constitution violations — section intentionally empty.
