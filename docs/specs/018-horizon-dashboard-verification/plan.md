# Implementation Plan: Verify Horizon dashboard compatibility

**Branch**: `018-horizon-dashboard-verification` (git branch: `feature/482`) | **Date**: 2026-07-22 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/018-horizon-dashboard-verification/spec.md`

## Summary

Prove that an **unmodified upstream OpenStack Horizon dashboard** (kolla Flamingo 2025.2) works against O3K for the six developer workflows — sign-in, project switching, launch instance (+SSH liveness), create network, create volume, and the instance console — and publish a graded, reproducible compatibility report under `docs/`.

The work is **verification, not product code**: no Go changes, no new Go modules, no database schema/migration. It produces (1) a reproducible host runbook covering two manual passes, (2) a dedicated verification-scoped **Horizon Helm chart** (so the real pass can run Horizon on the same k3s cluster as a real-mode O3K), and (3) the compatibility report, which supersedes/reconciles the pre-existing ungrounded `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md`. It mirrors SPEC-016 (Terraform verification): harness + runbook + report in `docs/compatibility/`.

Two passes:
- **Portable (stub)** — `deployments/docker-compose-horizon.yml` as-is; laptop-grade contract check.
- **Graded (real, authoritative)** — real-mode O3K on root/privileged **k3s** (libvirt/KVM VMs), **Rook/Ceph on minikube** serving image storage over **RGW/S3** (no RBD), Horizon on the same k3s cluster via the verification chart, browser reached via NodePort.

## Technical Context

**Language/Version**: No product code. Verification tooling only — Helm 3.12+ (new verification chart; existing `charts/o3k`), Bash + `python-openstackclient` + `curl` (existing `test/horizon_*.sh`), and the unmodified upstream kolla Horizon image (Flamingo 2025.2, Python/Django — config-only, never patched).  
**Primary Dependencies**: docker-compose (stub pass); k3s (root/privileged) + `charts/o3k` in real mode; minikube + Rook/Ceph exposing an RGW (S3) endpoint; a new verification Horizon chart (kolla image + `horizon-config` ConfigMap + memcached + `bonigarcia/novnc`).  
**Storage**: No schema change, no migration. Real-mode backends: node-local `/var/lib/o3k` (VM disks, volumes), Ceph **RGW/S3** for Glance images. External PostgreSQL unchanged. **RBD not used.**  
**Testing**: Manual, runbook-driven browser verification for both passes; existing `test/horizon_*.sh` reused as supporting API-level evidence. **No automated browser suite** (out of scope).  
**Target Platform**: Linux for the graded pass (k3s + libvirt/KVM + root); the stub pass is portable (docker-compose, any host).  
**Project Type**: Verification / infrastructure + documentation. No changes to the O3K product binary or the product `charts/o3k`.  
**Performance Goals**: N/A (verification). Reproducibility targets only: stub pass → signed-in dashboard within 15 min on a clean host (SC-002); real pass reproducible via runbook (no fixed bound).  
**Constraints**: Unmodified upstream Horizon (config only); RGW/S3 only (no RBD); single-node graded eval; the product `charts/o3k` MUST NOT be modified (Horizon ships as a separate verification chart); no third-party OpenStack Horizon chart (openstack-helm's is ecosystem-coupled).  
**Scale/Scope**: 6 verified workflows, 2 passes, 1 report, 1 runbook, 1 verification Horizon chart.

**Unknowns resolved in Phase 0** (`research.md`):
- **U1 — Console viability**: does O3K's Nova implement the remote-console URL API, and can a noVNC proxy actually broker to a VM's VNC? (Determines whether the console verdict is achievable or predetermined "not working".)
- **U2 — Glance↔RGW wiring**: exact O3K/`charts/o3k` values to point Glance at an external Ceph RGW (S3) endpoint, and what Rook must provide (endpoint + credentials).
- **U3 — O3K exposure for Horizon**: must O3K be NodePort-exposed with a browser-reachable catalog host, or can it stay ClusterIP with Horizon proxying server-side? (Drives the catalog `endpoint_host`, `cors_allowed_origins`, and `OPENSTACK_KEYSTONE_URL` values.)

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

Evaluated against `.specify/memory/constitution.md` (O3K Constitution v1.0.0, six principles). This feature is a **verification** of the constitution itself, so alignment is strong:

| Principle | Assessment |
|---|---|
| **I. OpenStack Flamingo API Compatibility (NON-NEGOTIABLE)** | ✅ **This feature operationalizes it.** The constitution literally requires "Every feature must pass contract tests using real OpenStack clients (Horizon, …)" and "Horizon dashboard (Flamingo version) must work without modification." SPEC-018 is that verification for Horizon, against the pinned Flamingo 2025.2 image. |
| **II. Synchronous Operations** | ✅ N/A — no new operations; verification observes existing behavior. |
| **III. Fail-Fast Design** | ✅ N/A — no new product external dependencies. |
| **IV. Multi-Mode Architecture** | ✅ Directly exercised — the two passes verify **stub** and **real** modes; the graded pass proves real libvirt/RGW behavior. |
| **V. Project Isolation** | ✅ N/A for product code; project switching (US1) incidentally exercises project scoping in the UI. |
| **VI. Single Binary Deployment** | ✅ **No violation.** Horizon is upstream Django and was never part of the O3K binary; the verification Horizon chart is external infra deployed *alongside* O3K, not a service added to the binary. The product `charts/o3k` is untouched. |
| **Tech constraints / Forbidden patterns** | ✅ Target = Flamingo 2025.2 (matches the kolla image). No message queues / async / state machines / service discovery introduced. |
| **Quality gates (before merge)** | ✅ "Horizon dashboard tested", "both stub and real modes tested", "contract tests via real clients" — this feature *is* that gate for Horizon. No hardcoded secrets (JWT via env; the stub compose already uses `${O3K_JWT_SECRET:?…}`). |

**Result: PASS.** No violations; no Complexity Tracking entries required. The separate verification Horizon chart is explicitly the *simpler* alternative to modifying the product chart (adopting openstack-helm's chart would add far more coupling) — consistent with the "Simplicity over performance/complexity" governance rule.

**Post-design re-check (after Phase 1)**: still **PASS**. Research reinforced simplicity — O3K stays `ClusterIP` (no NodePort/CORS gymnastics; U3), Glance→RGW is config-only via existing chart values (no product change; U2), and the console gap (U1) is documented as a finding + follow-up rather than fixed here. No new dependencies, no schema/migration, product `charts/o3k` untouched. The only new artifact is the verification Horizon chart, which lives outside the product binary (Principle VI intact).

## Project Structure

### Documentation (this feature)

```text
docs/specs/018-horizon-dashboard-verification/
├── spec.md              # Feature spec (done)
├── plan.md              # This file
├── research.md          # Phase 0 — resolves U1/U2/U3
├── data-model.md        # Phase 1 — report/verdict schema + verification-environment matrix (docs, not DB)
├── quickstart.md        # Phase 1 — condensed deploy→verify for both passes
├── contracts/           # Phase 1 — workflow verification matrix + report template
│   ├── workflow-matrix.md
│   └── report-template.md
├── checklists/
│   └── requirements.md  # Spec-quality checklist (done, re-validated)
├── README.md            # (created in implementation) goal + where-things-live table (mirrors SPEC-016)
├── HOST_TESTING.md      # (created in implementation) stub-pass runbook (docker-compose)
├── HOST_TESTING_REAL_MODE.md  # (created in implementation) real-pass runbook (k3s + minikube/Rook-Ceph)
└── tasks.md             # Phase 2 (/cl.tasks — NOT created here)
```

### Source Code / artifacts (repository root)

No product source changes. New/changed artifacts:

```text
deployments/
├── docker-compose-horizon.yml      # REUSED as-is (stub pass) — no change
└── horizon-config/                 # REUSED verbatim as the ConfigMap source (real pass)

deployments/horizon-chart/          # NEW — verification-scoped Helm chart (name TBD in impl)
├── Chart.yaml
├── values.yaml                     # kolla image tag, o3k Service DNS, NodePort ports, memcached, novnc
└── templates/
    ├── configmap.yaml              # mounts horizon-config/* at the kolla config paths
    ├── horizon-deployment.yaml     # kolla horizon:2025.2 (unmodified image)
    ├── horizon-service.yaml        # NodePort (dashboard)
    ├── memcached-deployment.yaml   # + Service (sessions)
    └── novnc-deployment.yaml       # bonigarcia/novnc + Service (NodePort)

docs/compatibility/
└── HORIZON_COMPATIBILITY_REPORT.md # NEW — graded report; reconciles docs/HORIZON_FULL_COMPATIBILITY_REPORT.md

test/horizon_*.sh                   # REUSED as supporting API-level evidence (no functional change;
                                    #   optional: de-hardcode 10.2.199.101 in critical/full tests)
charts/o3k/                         # UNCHANGED (product chart; real mode uses existing realMode.* values)
```

**Structure Decision**: Verification feature — artifacts live under the feature's `docs/specs/018-.../` (spec, plan, research, runbooks, contracts) plus three repo-level additions: a **new verification Horizon Helm chart** under `deployments/` (kept out of the product `charts/o3k`), the **report** under `docs/compatibility/` (mirroring the Terraform report), and **reused** existing assets (`docker-compose-horizon.yml`, `horizon-config/`, `test/horizon_*.sh`). This mirrors SPEC-016's harness+runbook+report layout.

## Complexity Tracking

No constitution violations — table intentionally empty. (The one arguable "extra" — a separate Horizon chart — is the *simplicity-preserving* choice: it avoids modifying the product chart and avoids the heavier openstack-helm chart, per the research in Phase 0.)

## Phase 0 — Outline & Research

See `research.md`. Resolves U1 (console viability), U2 (Glance↔RGW wiring), U3 (O3K exposure/catalog/CORS). Method: read the O3K code (`internal/nova`, `internal/glance`, `internal/keystone`, `pkg/storage`, `pkg/hypervisor`), the chart values, and the Horizon `local_settings`; consolidate as Decision / Rationale / Alternatives.

## Phase 1 — Design & Contracts

- `data-model.md` — the report's verdict schema and the verification-environment matrix (documentation entities; no DB).
- `contracts/workflow-matrix.md` — per-workflow: the Horizon action → the O3K API surface it hits → the pass/partial/fail acceptance check.
- `contracts/report-template.md` — the report skeleton (environment table, verdict table, findings-with-IDs), mirroring `TERRAFORM_COMPATIBILITY_REPORT.md`.
- `quickstart.md` — condensed deploy→verify for both passes.
- Agent context updated via `update-agent-context.sh claude`.

## Phase 2 — Task planning (NOT executed here)

`/cl.tasks` will derive dependency-ordered tasks: build the Horizon chart → stand up the real environment (k3s + minikube/Rook-Ceph) → run both passes → write the report + runbooks → reconcile the prior report. Defects discovered become follow-up tickets (as SPEC-016 → #524).
