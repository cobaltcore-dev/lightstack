# Phase 1 Data Model: Verify Horizon dashboard compatibility

**Feature**: SPEC-018 | **Date**: 2026-07-22

This is a **verification** feature — it introduces **no database schema and no migration**. The "entities" here are the **documentation artifacts** the verification produces. They define the shape of the report and the runbooks so the output is consistent and reproducible.

## Entity: Verification Environment

The concrete context a set of verdicts is valid for. One record per pass.

| Field | Type | Notes |
|---|---|---|
| `pass` | enum `stub` \| `real` | which verification pass |
| `o3k_build` | string | O3K version + git commit (e.g. `feature/482 @ <sha>`) |
| `horizon_version` | string | kolla image tag (e.g. `quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble`) |
| `deployment` | string | stub: `docker-compose-horizon.yml`; real: `k3s (root) + charts/o3k realMode` |
| `image_backend` | string | stub: n/a; real: `Ceph RGW (S3) via Rook on minikube` |
| `compute_backend` | string | stub: n/a; real: `libvirt/KVM` |
| `volume_backend` | string | stub: n/a; real: `node-local /var/lib/o3k/volumes` (no RBD) |
| `browser_access` | string | stub: `localhost:80`; real: `Horizon NodePort`; O3K stays ClusterIP (`endpointHost=<release>-o3k`) |
| `date` | date | when the pass was run |

## Entity: Workflow Verification Result

One record per verified workflow per pass. Aggregated into the report's verdict table.

| Field | Type | Notes |
|---|---|---|
| `workflow` | enum | `sign-in` \| `project-switch` \| `launch-instance` \| `create-network` \| `create-volume` \| `instance-console` |
| `pass` | enum `stub` \| `real` | which pass produced this verdict |
| `verdict` | enum | `working` \| `partial` \| `not-working` \| `not-exercised` (stub-only, for backend-dependent workflows) |
| `ui_action` | string | the Horizon action performed |
| `o3k_surface` | string | the O3K API surface exercised (see `contracts/workflow-matrix.md`) |
| `evidence` | string | observed behavior / screenshot / CLI cross-check |
| `finding_ref` | string \| null | link to a Finding (if not `working`) |

**Verdict rules**:
- `working` — the UI action completed and the resource/behavior is observable.
- `partial` — completes with a material limitation (documented).
- `not-working` — fails; the failing step is recorded.
- `not-exercised` — the stub pass cannot prove this (real VM / volume / console); real pass supplies the verdict.

**Known expected verdict** (from `research.md` U1): `instance-console` → `not-working` (real pass), with the serial Console Log noted as working.

## Entity: Finding

A discovered gap, mirroring SPEC-016's report findings. Every `partial`/`not-working` verdict links one.

| Field | Type | Notes |
|---|---|---|
| `id` | string | `F1`, `F2`, … |
| `severity` | enum | `blocker` \| `major` \| `minor` |
| `title` | string | short description |
| `cause` | string | root cause (cite code where known) |
| `user_impact` | string | what the developer cannot do |
| `workaround` | string \| null | if any |
| `follow_up` | string \| null | follow-up ticket ref (created after verification, as SPEC-016→#524) |

**Seed finding** (from U1): `F?` — noVNC console: synthetic `localhost:6080` URL + unpersisted token + no libvirt VNC-port readback + unwired generic proxy → console panel shows no guest. Impact: no in-browser VM console. Workaround: serial `console log show`. Follow-up: wire real console.

## Relationships

```
Verification Environment (1) ──< (N) Workflow Verification Result >── (0..1) Finding
```

The **Compatibility Report** (`docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`) renders: one Environment table per pass, one Workflow-Verification-Result table, and a Findings list — per `contracts/report-template.md`.
