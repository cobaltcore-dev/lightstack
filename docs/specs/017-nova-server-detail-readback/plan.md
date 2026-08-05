# Implementation Plan: Nova server-detail read-back (Terraform compatibility follow-ups)

**Branch**: `feature/524-…` (feature id `017-nova-server-detail-readback`) | **Date**: 2026-07-17 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/017-nova-server-detail-readback/spec.md`

## Summary

Populate `key_name` and `security_groups` in the Nova server-detail responses (single-server show **and** list-detail) so that a repeated `terraform apply`/`plan` against an unchanged configuration is a no-op — resolving finding **F1** from #481. Neither value is readable today, so the work is: **persist at create, read back on detail**. Migration `063` adds a nullable `instances.key_name`; `CreateServer` persists the key name and associates supplied security groups via the existing `server_security_groups` join table (migration 049), each **validated synchronously** (unknown keypair or unknown *non-default* security group → Nova-compatible `400`, no instance created). The project's `default` security group is **ensured (auto-created) on demand**, matching OpenStack's guarantee that every project has one. The read paths resolve associations back to names and default to `[{"name":"default"}]` when an instance has none, and emit `"key_name": null` when there is no keypair — matching upstream Nova v2.1 exactly. Regression is guarded by Go unit tests running in the existing CI job (US3); a dedicated terraform-in-CI smoke is deferred.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: gin-gonic/gin (HTTP), jackc/pgx/v5 + pgxpool (PostgreSQL); **no new modules**
**Storage**: PostgreSQL — new migration `063` adds `instances.key_name` (nullable `text`); reuses `server_security_groups` (migration 049) and the Neutron `security_groups` table for name→id resolution
**Testing**: `go test` (internal/nova unit tests), gophercloud contract tests (`test/contract/`), bash integration (`test/`), Terraform compatibility scenario (`test/terraform/compat/`)
**Target Platform**: Linux server (all changes are DB + HTTP JSON — they work identically in **stub** mode on any platform; no libvirt/Ceph/netlink involved)
**Project Type**: Single Go binary, multi-service (Nova API on :8774)
**Performance Goals**: server-detail response < 100 ms; list-detail must resolve security groups for the whole page in a **single batch query** (no per-server N+1)
**Constraints**: OpenStack Flamingo (2025.2) **Nova v2.1** wire compatibility (Constitution I, non-negotiable); synchronous create (II); fail-fast validation (III); must work in stub mode (IV); project/user scoping (V)
**Scale/Scope**: 2 response fields across 2 endpoints, 1 reversible migration, 2 create-time validations; changes localized to `internal/nova` + one migration + docs/tests

*No `NEEDS CLARIFICATION` remain*: the product decisions were resolved across two `/cl.clarify` sessions (unknown-SG → 400, unknown-keypair → 400, `key_name: null`, default SG, US3 scope). The only remaining unknowns were **code facts** (FK cascade, default-SG creation, keypair-validation control flow, in-use-SG delete guard, test harness) — resolved in Phase 0 `research.md`.

## Constitution Check

*GATE: evaluated before Phase 0 and re-checked after Phase 1 design.*

| Principle | Verdict | Notes |
|---|---|---|
| **I. Flamingo API compatibility (NON-NEGOTIABLE)** | ✅ PASS | Fields match Nova v2.1 shape verified against the api-ref: `key_name` is a nullable string; `security_groups` is `[{"name": …}]` and defaults to `[{"name":"default"}]`. This feature *increases* compatibility. Confirmed by a gophercloud contract test. |
| **II. Synchronous operations** | ✅ PASS | key_name/SG validation + persistence occur synchronously inside the create request (the keypair lookup used for validation runs **before** the response, not only in the post-202 async cloud-init goroutine). Read-back is a synchronous DB query. |
| **III. Fail-fast** | ✅ PASS | Unknown keypair or security group returns an immediate `400` — no retries, no delayed error. |
| **IV. Multi-mode** | ✅ PASS | Purely DB + HTTP; identical behavior in stub and real mode. Unit/contract tests run in stub mode. |
| **V. Project isolation** | ✅ PASS | Security-group name→id resolution is scoped by `project_id`; keypair validation is scoped by `user_id` (keypairs are user-owned). |
| **VI. Single binary** | ✅ PASS | No new process, dependency, or service. |
| **Backwards compat / reversible migration** | ✅ PASS | Migration `063` ships both `.up.sql` and `.down.sql`. `key_name` is nullable so pre-existing rows read back as `null`. |
| **III. Test-First (workflow)** | ✅ PASS | Contract + unit tests authored first and confirmed RED before implementation. |

**No violations** → Complexity Tracking is empty.

## Project Structure

### Documentation (this feature)

```text
docs/specs/017-nova-server-detail-readback/
├── plan.md              # This file
├── research.md          # Phase 0 output (code-fact decisions)
├── data-model.md        # Phase 1 output (instances.key_name + associations)
├── quickstart.md        # Phase 1 output (how to validate the no-op re-apply)
├── contracts/           # Phase 1 output (server-detail response contract)
└── tasks.md             # Phase 2 output (/cl.tasks — NOT created here)
```

### Source Code (repository root)

```text
internal/nova/
├── handlers.go            # CreateServer: parse+validate+persist key_name & security_groups;
│                          #   GetServer & ListServersDetail: read back both fields
├── advanced_actions.go    # reuse the existing SG name→id resolution (AddSecurityGroup)
│                          # + idempotent neutron.EnsureDefaultSecurityGroup(projectID) (FR-006a)
└── *_test.go              # NEW unit tests asserting both fields on show + list-detail, and 400 paths

internal/database/models.go  # add KeyName to the Instance model if that struct is used by these paths

migrations/
├── 063_nova_instance_key_name.up.sql    # ALTER TABLE instances ADD COLUMN key_name text
└── 063_nova_instance_key_name.down.sql  # ALTER TABLE instances DROP COLUMN key_name

test/
├── terraform/compat/      # confirm apply → re-plan = 0 changes (F1)
└── contract/              # gophercloud assertions for the two fields (Constitution I)

docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md  # mark F1 resolved
```

**Structure Decision**: Single-project Go binary. All production changes are confined to `internal/nova` plus one reversible migration; no new packages, modules, or services (Constitution VI, VII/Simplicity).

## Complexity Tracking

> No Constitution violations — no entries.
