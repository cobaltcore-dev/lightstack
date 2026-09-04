# Implementation Plan: Identity resource not-found for malformed ids

**Branch**: `015-identity-uuid-404` | **Date**: 2026-07-14 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/015-identity-uuid-404/spec.md`

## Summary

Keystone identity lookups by path id return HTTP 500 for a non-UUID id (the `id` columns are Postgres `uuid`; a non-UUID raises `22P02`, surfaced as 500) instead of the Keystone-conformant 404. Fix the whole identity path-id surface with a **hybrid** mechanism: a keystone-scoped **UUID path-param middleware** on the non-authz-gated routes, plus **in-handler** id validation *after* the authorization check on the authz-gated routes (so 401/403 still precede 404). Both paths emit one shared, Keystone-verbatim 404 — `"Could not find <resource>: <id>."` — via a new `common.NewIdentityNotFoundError`. Valid-but-unknown ids are normalized to the identical response, and handlers that currently mask all errors as 404 are fixed to distinguish `ErrNoRows` (404) from a genuine backend error (5xx). Envelope stays lightstack's Nova-style form (Identity-v3 envelope deferred to #518). No schema change, no new dependency.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: gin-gonic/gin v1.12.0 (HTTP), google/uuid v1.6.0 (`uuid.Validate` — already imported), jackc/pgx/v5 v5.10.0 (`pgx.ErrNoRows`). No new modules.
**Storage**: PostgreSQL — **no schema change, no migration** (identity id columns stay `uuid`).
**Testing**: `go test` (unit, race) — middleware unit-tested with `gin.CreateTestContext` (no DB); DB-backed handler/e2e tests via the existing `requireDB` / `LIGHTSTACK_TEST_DB_URL` harness (`internal/keystone/maindb_test.go`); optional integration bash in `test/`.
**Target Platform**: Linux server / single binary. Keystone identity is DB-only (no libvirt/Ceph/netlink), so behavior is identical in stub and real modes.
**Project Type**: Single Go binary, web-service (six embedded HTTP services).
**Performance Goals**: unchanged; middleware adds one `uuid.Validate` per request (negligible). Metadata ops < 100ms (constitution) unaffected.
**Constraints**: keystone-only blast radius (no change to nova/cinder/neutron/glance/metadata). Lands as a separate PR **after** feature/480 (#73) squash-merges to main (built on that tree so the EC2/s3 routes are present).
**Scale/Scope**: ~35 identity by-id routes across 10 resource types; ~6 authz-gated routes handled in-handler.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Notes |
|-----------|--------|-------|
| I. OpenStack Flamingo API compatibility (NON-NEGOTIABLE) | ✅ Improves | Brings identity 404 status + message into line with Keystone; verified against reference Keystone (message strings) + gophercloud/CLI name→id resolution. |
| II. Synchronous operations | ✅ N/A | No async introduced. |
| III. Fail-fast | ✅ N/A | No external dependency added; genuine backend errors surface immediately as 5xx (FR-010). |
| IV. Multi-mode | ✅ Pass | Keystone identity is DB-only; fix is mode-agnostic (works in stub and real). |
| V. Project isolation | ✅ Pass | No change to project scoping. |
| VI. Single binary | ✅ Pass | No new process/dependency; middleware compiled into the binary. |
| Test-First (workflow) | ✅ Pass | Middleware + handler tests written RED first (Phase in tasks.md). |
| Forbidden patterns / new deps | ✅ Pass | None introduced. |
| Backwards compat / migrations reversible | ✅ Pass | No migration. Behavior change = 500→404 (a fix) + verbatim messages; no API-shape break. |

**Result**: PASS — no violations. Complexity Tracking not required.

## Project Structure

### Documentation (this feature)

```text
docs/specs/015-identity-uuid-404/
├── spec.md              # Feature spec (done, clarified)
├── plan.md              # This file
├── research.md          # Phase 0 — decisions & rationale
├── data-model.md        # Phase 1 — no schema change; resource→noun map; error-response model
├── quickstart.md        # Phase 1 — repro + validation steps
├── contracts/
│   └── identity-not-found.md   # Phase 1 — the not-found behavioral contract
├── checklists/
│   └── requirements.md  # spec quality checklist (all pass)
└── tasks.md             # Phase 2 (/cl.tasks — NOT created here)
```

### Source Code (repository root)

```text
internal/
├── common/
│   └── errors.go                     # + NewIdentityNotFoundError(resource, id); NewProjectNotFoundError delegates to it
├── keystone/
│   ├── handlers.go                   # RegisterRoutes: split `mgmt` into `val` (uuid middleware) + gated routes on plain mgmt;
│   │                                 #   normalize not-found paths (GetUser/GetProject/GetRole/…); in-handler id check after authz (DeleteUser, ChangePassword)
│   ├── uuid_middleware.go            # NEW — keystone-scoped UUID path-param middleware
│   ├── uuid_middleware_test.go       # NEW — middleware unit tests (no DB)
│   ├── groups.go                     # not-found → verbatim; masking fix (GetGroup/UpdateGroup/DeleteGroup/…)
│   ├── domains.go                    # not-found → verbatim; masking fix (Get/Update/Delete/Config)
│   ├── services.go                   # not-found → verbatim (Get/Update/Delete service, Delete endpoint)
│   ├── credentials.go                # not-found → verbatim (Get/Update/Delete credential)
│   ├── application_credentials.go    # not-found → verbatim (Get/List/Delete/ByID)
│   ├── ec2_credentials.go            # in-handler :id (user) check after denyIfNotSelfOrAdmin; reuse NewIdentityNotFoundError
│   └── identity_notfound_test.go     # NEW — DB-backed e2e across resource groups (requireDB)
└── ...
migrations/                           # (unchanged — no migration)
test/                                 # (optional) integration bash: CLI name→id resolution
```

**Structure Decision**: Single Go project. The UUID middleware lives in `internal/keystone/` (not `internal/middleware/`) to keep it keystone-scoped — it references keystone/common error constructors and is registered only on keystone's management route group. The shared 404 constructor goes in `internal/common/errors.go` alongside the existing `NewProjectNotFoundError` (added by #73), which it generalizes.

## Complexity Tracking

> No Constitution violations — section intentionally empty.
