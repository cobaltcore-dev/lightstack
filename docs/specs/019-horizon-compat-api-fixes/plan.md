# Implementation Plan: Horizon compatibility — Cinder/Neutron/Glance API & response fixes (F4, F5, F6, F9)

**Branch**: `feature/532` (feature id `019-horizon-compat-api-fixes`) | **Date**: 2026-07-24 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/019-horizon-compat-api-fixes/spec.md`

## Summary

Four self-contained backend/response fixes that restore OpenStack Flamingo API compatibility for Horizon
(SPEC-018 findings F4/F5/F6/F9). Each is independent and touches a single service:

- **F9** (Cinder): `volume show` / `volume list --long` omit ~10 fields Horizon's volume table renders → they
  arrive as `None` → HTTP 500. Fix: a shared volume serializer that emits the full upstream Cinder field set
  with non-null defaults; persist `volume_type` on create (default `__DEFAULT__`).
- **F5** (Keystone catalog): the `volume` alias endpoint (`…015`) is seeded at `http://localhost:8776/v3` and
  runtime substitution only rewrites the `http://lightstack:` prefix, so it stays localhost. Fix: a corrective
  migration reseeding `…015` to the `http://lightstack:` convention every other endpoint uses, plus a defensive
  substitution that also rewrites `localhost`. *(Implementation finding: the S3 endpoint `…016` is already
  correct — `BuildServiceCatalog` overrides it from `cfg.Glance.S3Endpoint` or omits it, never serving the
  localhost seed — so it needs no migration; only verification.)*
- **F4** (Neutron): `ListNetworks` ignores the `shared` / `router:external` / `tenant_id` filters, so Horizon's
  three sub-queries return the same superset → duplicate rows. Fix: honor the filters (treating all Lightstack
  networks as non-external) and emit the `router:external` attribute.
- **F6** (Glance): public images are stored ownerless (`project_id` NULL), so `DELETE … AND project_id=$2`
  matches zero rows yet returns 204. Fix: preserve the owner on create for all visibilities, authorize delete
  as **owner-or-admin** (upstream Glance semantics), return 403/404 correctly, and check rows-affected.

No new Go modules. One new migration (`064`, F5 catalog reseed). No table/schema changes elsewhere (F9 reuses
the existing `volumes.volume_type` column; F4/F6 are code-only). Verified in **both** stub and real modes.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: gin-gonic/gin (HTTP), jackc/pgx/v5 + pgxpool (PostgreSQL); `google/uuid`. **No new modules.**
**Storage**: External PostgreSQL. **One** new migration `064_fix_cinder_volume_alias_localhost` (reseeds the `volume` alias `…015` endpoint URL; s3 `…016` is already config-overridden, so it is not touched). F9 reuses the existing `volumes.volume_type` column (migration `016`); F4 and F6 add **no** columns.
**Testing**: Go unit tests (`internal/**/**_test.go`) as the primary regression guard (run in the existing CI unit-tests job); gophercloud / `python-openstackclient` contract checks under `test/contract/` and `test/`; bash integration scripts in `test/`; two manual runbook passes (stub + real) per SPEC-018.
**Target Platform**: Linux for real mode (libvirt/KVM, Rook/Ceph RGW); stub mode is cross-platform. Both modes are in scope.
**Project Type**: Single ~35 MB Go binary with six embedded services (`cmd/lightstack/main.go`). No new project.
**Performance Goals**: Unchanged — these are response-shape/authorization fixes, not new hot paths. Metadata ops stay < 100 ms; fail-fast timeouts unchanged.
**Constraints**: OpenStack Flamingo (2025.2) API compatibility (Constitution Art. I, NON-NEGOTIABLE); synchronous operations (Art. II); fail-fast (Art. III); stub **and** real mode parity (Art. IV); project isolation preserved (Art. V).
**Scale/Scope**: 4 fixes across 4 files (`internal/cinder/volumes.go`, `internal/keystone/auth.go`, `internal/neutron/network.go`, `internal/glance/images.go`) + 1 migration + tests.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Assessment |
|---|---|
| **I. Flamingo API Compatibility (NON-NEGOTIABLE)** | ✅ **This feature exists to restore it.** Each fix moves a response/behavior toward the Flamingo contract (Cinder v3 volume schema, Neutron v2.0 list filters + `router:external`, Glance v2 delete authorization, a resolvable service catalog). Verified with real clients (Horizon, python-openstackclient) in both passes. |
| **II. Synchronous Operations** | ✅ No async introduced. All changes are within the existing synchronous request path (serialize a response, run one `DELETE`, one migration). |
| **III. Fail-Fast Design** | ✅ Unchanged. No retries/backoff added; F6 now fails *faster/clearer* (403/404 instead of a false 204). |
| **IV. Multi-Mode Architecture** | ✅ All four fixes are storage/mode-agnostic and are verified in stub **and** real (RGW) modes. F6 explicitly removes the DB row (the list's source of truth) regardless of backend object deletion. |
| **V. Project Isolation** | ✅ Reinforced. F4 honors `tenant_id`/`project_id` only as a *narrowing* filter within the caller's scope (never widens); F6 authorizes deletion by owner-or-admin using the JWT `roles`, matching the isolation model. |
| **VI. Single Binary Deployment** | ✅ Unchanged — same binary, same six services. |
| **Test-First / Backwards-Compat** | ✅ Contract/unit tests written first (RED → GREEN). Migration `064` ships reversible up/down. Existing responses/flows unchanged except the targeted fields. |
| **Forbidden patterns** | ✅ None used (no queues, workers, state machines, service discovery). |

**Result: PASS — no violations.** Complexity Tracking is intentionally empty.

## Project Structure

### Documentation (this feature)

```text
docs/specs/019-horizon-compat-api-fixes/
├── plan.md              # This file
├── spec.md              # Feature specification (+ Clarifications)
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output (response/behavior contracts)
│   ├── cinder-volume-response.md
│   ├── neutron-network-list-filters.md
│   ├── glance-image-delete.md
│   └── keystone-catalog-endpoints.md
├── checklists/
│   └── requirements.md
└── tasks.md             # Phase 2 output (/cl.tasks — NOT created here)
```

### Source Code (repository root)

Existing single-project Go layout. Affected files (one per finding, plus tests):

```text
internal/
├── cinder/
│   └── volumes.go          # F9: shared volume serializer; SELECT user_id/volume_type; persist volume_type
│                           #     on create; GetDefaultVolumeType → __DEFAULT__
├── keystone/
│   └── auth.go             # F5: (defensive) substituteURLTemplates also rewrites http(s)://localhost:
├── neutron/
│   └── network.go          # F4: honor shared / router:external / tenant_id filters; emit router:external
└── glance/
    └── images.go           # F6: preserve owner on create (all visibilities); owner-or-admin delete authz;
                            #     rows-affected → 404; 403 for unauthorized

migrations/
├── 064_fix_cinder_volume_alias_localhost.up.sql    # F5: reseed volume alias …015 to http://lightstack: (s3 …016 already config-overridden)
└── 064_fix_cinder_volume_alias_localhost.down.sql  # revert …015 to its localhost seed

# Tests (alongside sources + existing harnesses)
internal/cinder/volumes_test.go        # F9 serializer field/default assertions
internal/neutron/network_test.go       # F4 filter/disjointness assertions
internal/glance/images_test.go         # F6 authz + rows-affected assertions
internal/keystone/*_test.go            # F5 catalog-resolution (no localhost) assertions
test/                                  # bash + openstackclient integration; SPEC-018 runbooks (stub + real)
```

**Structure Decision**: Reuse the existing single-binary layout — no new packages or projects (Art. VI,
Simplicity). Each finding is confined to one handler file; the only cross-cutting artifact is migration `064`.

## Complexity Tracking

> No constitution violations — section intentionally empty.
