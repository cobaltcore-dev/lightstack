# Lightstack Specifications Registry

This directory holds Lightstack's feature specifications. Each spec lives in its own
`NNN-name/` directory and follows the shared internal pattern:
`spec.md` → `plan.md` → `tasks.md` → implementation, supported by `research.md`,
`data-model.md`, `contracts/`, `checklists/`, and host-validation runbooks.

This README is a **live registry**, not a snapshot: statuses below reflect the actual
state of the repository (merged commits, code references, contract tests), not the
`Status:` header inside each spec (those headers are written at authoring time and are
not updated after merge).

## SPEC-000: Priority Zero — Supersedes All

[SPEC-000: OpenStack API Compliance Framework](./000-api-compliance/README.md) is the
foundational, standing requirement: Lightstack must be indistinguishable from canonical
OpenStack to every unmodified client (OpenStack CLI, SDKs, Terraform provider, Horizon).
It is **priority zero and supersedes all other specifications** — every spec, every PR,
and every change must comply. It is continuous by nature and never "done"; it is
enforced through the contract suites under `test/contract/` and the compatibility
reports under `docs/compatibility/`.

## Spec Registry

Statuses: `draft`, `accepted`, `implemented`, `superseded`, `archived` (see
[Lifecycle](#lifecycle)). No spec is currently superseded or archived.

| # | Spec | One-line description | Status | Notes / evidence |
|---|------|----------------------|--------|------------------|
| 000 | [API Compliance Framework](./000-api-compliance/README.md) | Priority-zero standing framework: 100% OpenStack API compatibility, enforced by contract tests | accepted | Standing/continuous; contract suites (`test/contract/`) and compatibility reports exist, but compliance never terminates |
| 001 | [Modular Architecture Transformation](./001-modular-architecture/README.md) | Split the monolith into independently deployable per-service binaries | draft | Never started — no commits, no code references; overtaken in practice by the accepted single-binary direction (see ADR-001/ADR-002) |
| 002 | [Enhanced Authentication System](./002-authentication-enhancement/README.md) | OAuth2/OIDC, SAML, LDAP, application credentials, token revocation | draft | Partially delivered (application credentials in `internal/keystone/application_credentials.go`; RBAC contract tests from its T048); federated auth unimplemented |
| 003 | [Barbican — Key Management](./003-barbican/README.md) | Encryption key / secret management service | draft | Future service; no `internal/barbican` package exists |
| 004 | [Designate — DNS as a Service](./004-designate/README.md) | Multi-tenant DNS management | draft | Future service; no `internal/designate` package exists |
| 005 | [Octavia — Load Balancing](./005-octavia/README.md) | L4/L7 load balancing service | draft | Future service; no `internal/octavia` package exists |
| 006 | [v0.7.0 Foundation](./006-v070-foundation/README.md) | Code-quality foundation: DB injection, rate limiting, CI linting, Terraform conformance, route consolidation, pagination | accepted | Partially implemented: DI landed via SPEC-027; linting in CI; Terraform conformance harness merged; rate limiting (ADR-002) and Cinder 301 route consolidation still outstanding |
| 007 | [Helm Prerequisites](./007-helm-prereqs-bootstrap-catalog/spec.md) | Admin-password bootstrap + configurable service-catalog scheme | implemented | Merged (#50); 6 code references cite SPEC-007 |
| 008 | [Helm Charts](./008-helm-charts/spec.md) | Kubernetes deployment chart, single-node and multi-node | implemented | Chart merged (#54); lives in `charts/lightstack/`; multi-node post-conditions tracked separately |
| 009 | [Single-Node Real Mode (v1)](./009-single-node-real-mode/spec.md) | Real VMs end-to-end on one node: boot, networking, cloud-init, egress, volumes | implemented | Merged (#64); 81 code references; a few host-validation runbook tasks remain open in `tasks.md` |
| 010 | [Helm Teardown](./010-helm-teardown/spec.md) | Pre-delete hook for clean uninstall, no state leakage | implemented | Merged (#67); `internal/teardown/` exists |
| 011 | [Ensure Storage Subdirs](./011-ensure-storage-subdirs/spec.md) | Pre-create node-local storage subdirectories on real-mode deploy | implemented | Merged (#66) |
| 012 | [Integration Contract](./012-integration-contract/spec.md) | Helm values schema + status conventions | implemented | Merged (#69) |
| 013 | [Readiness/Liveness Probes](./013-readiness-liveness-probes/spec.md) | Honest readiness/liveness endpoints for k8s-native heartbeating | implemented | Merged (#70); `internal/health/` exists; 10 code references |
| 014 | [Ceph RGW (S3) Backend](./014-ceph-rgw-s3-backend/spec.md) | Glance image backend + tenant object storage on Ceph RGW | implemented | Merged; 31 code references |
| 015 | [Identity UUID 404](./015-identity-uuid-404/spec.md) | Return 404 (not 500) for malformed identity resource ids | implemented | Merged (#515) |
| 016 | [Terraform Provider Verification](./016-terraform-openstack-provider-verification/README.md) | Verify unmodified terraform-provider-openstack apply/destroy | implemented | Harness + report merged (#481); `docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md` |
| 017 | [Nova Server-Detail Read-back](./017-nova-server-detail-readback/spec.md) | Terraform-compat server-detail fields (`key_name`, `security_groups`) | implemented | Merged (#524); cited by `test/contract/server_readback_test.go` |
| 018 | [Horizon Dashboard Verification](./018-horizon-dashboard-verification/spec.md) | Verify unmodified Horizon against Lightstack; publish findings report | implemented | Merged (#482); `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` |
| 019 | [Horizon Compat API Fixes](./019-horizon-compat-api-fixes/spec.md) | Backend/response fixes for findings F4/F5/F6/F9 | implemented | Merged (#532) |
| 020 | [Horizon Create/UI Enablement](./020-horizon-create-ui-enablement/spec.md) | Enable Horizon create/write workflows (F2/F3/F7/F8) | implemented | Merged (#533); 58 code references |
| 021 | [Real noVNC Console](./021-real-novnc-console/spec.md) | A real graphical console via noVNC (finding F1) | implemented | Merged (#534); 57 code references; host-pass validation tasks remain open in `tasks.md` |
| 022 | [Project/Tenant Isolation](./022-project-tenant-isolation/spec.md) | Tenant lifecycle & isolation defect fixes (B2/B5/B8) | implemented | Merged (#545); EVIDENCE.md in spec dir |
| 023 | [Tenant Scoping & Authz](./023-tenant-scoping-authz/spec.md) | Tenant scoping and authorization model (P2/P3) | implemented | Merged (#549); 57 code references |
| 024 | [Neutron API Fidelity](./024-neutron-api-fidelity-realmode/spec.md) | Neutron API fidelity & real-mode networking fixes (B6/B9–B11, F10/F11) | implemented | Merged (#546); 60 code references |
| 025 | [Nova Gaps & Client Discovery](./025-nova-gaps-client-discovery/spec.md) | Ten defects: discovery self-links + compute answers that look successful | implemented | Merged; commit `3f6633d` |
| 025 | [SPEC-025 Follow-ups](./025-followup/spec.md) | Pseudo-ticket for the follow-ups found while implementing SPEC-025 | implemented | Merged on the same branch; commit `fb6443a` |
| 027 | [Database Pool Injection](./027-database-pool-injection/spec.md) | Inject `*pgxpool.Pool` through service constructors (implements ADR-001) | implemented | Merged; ADR-001 records "Implemented by SPEC-027" |

### Numbering anomalies

- **Duplicate `025`.** Two directories carry number 025: `025-nova-gaps-client-discovery`
  (the ticket proper) and `025-followup` (a pseudo-ticket for its follow-ups, tracked by
  a comment rather than an issue). Both stay as-is for history.
- **Missing `026`.** SPEC-026 was the o3k → lightstack rename (cloud-storage#575,
  commit `7ea8682`). Its spec directory was deliberately never tracked — the rename
  could not be applied to a spec describing the rename — so no `026-*` directory exists.

## Numbering Rules

- One number per spec. Numbers are **monotonic**: each new spec takes the next free
  number and never reuses or renumbers an existing one, even if earlier numbers are
  missing or duplicated (see anomalies above — they stay as-is for history).
- The **next free number is 028.**
- A spec directory is named `NNN-short-slug/` (lowercase, hyphenated), and the spec is
  cited as `SPEC-NNN` in code comments, commit messages, and ADRs.

## Lifecycle

A spec moves through at most these states; state changes are made by editing this
registry (the `Status:` header inside a spec records its state at authoring time):

1. **draft** — being specified or implemented. The spec exists, work may be underway,
   but nothing has shipped. Spec-local questions are resolved in the spec's
   `Clarifications` section.
2. **accepted** — the design is ratified (reviews passed, decisions locked, any ADRs
   recorded) but implementation is not complete. A spec whose scope is only partially
   delivered stays here until the remainder lands or is explicitly deferred.
3. **implemented** — the implementing PR(s) are merged to `main`. **The PR that
   implements a spec is what moves it to implemented**; the author updates this
   registry in that PR, citing the merge as evidence (commit/PR reference, code
   references to the spec's FR IDs, or contract-test citations).
4. **superseded** — replaced by another spec or ADR; the row names the successor.
5. **archived** — deliberately retired without a successor (e.g. abandoned direction);
   the directory moves to `docs/archive/` and the row names the reason.

SPEC-000 is a special case: as the standing priority-zero framework it remains
**accepted** permanently — compliance is continuously enforced, never "implemented once".

## How to Write a Spec

Copy [`_template/`](./_template/) into `docs/specs/NNN-short-slug/` and follow the
workflow described there (**specify → plan → tasks → implement → validate**). The
internal pattern, per directory:

| File | Purpose |
|------|---------|
| `spec.md` | The feature specification: problem, goals/non-goals, requirements with **FR-NNN ids** (these ids are how code comments and commit messages cite the spec — they are mandatory), success criteria, clarifications |
| `plan.md` | Implementation plan: technical approach, phases, sequencing, risks |
| `tasks.md` | Checklist of concrete tasks (`Tnnn`), test tasks written first (test-first), updated as work proceeds |
| `research.md` | External research and codebase findings that ground the design |
| `data-model.md` | Schema/entity changes the spec requires |
| `contracts/` | The API contracts the spec must satisfy, as Given/When/Then tables — these become the contract tests |
| `checklists/` | Per-requirement validation checklists |
| `quickstart.md`, `HOST_TESTING*.md` | Operator runbooks and host-validation passes (real-mode tests cannot run in CI) |

Related indexes:

- [CONTRACT-INDEX.md](./CONTRACT-INDEX.md) — which contract tests pin which spec.
- [ADR index](../adr/README.md) — architecture decision records; cross-cutting
  decisions that affect multiple specs live there, not in a spec.
