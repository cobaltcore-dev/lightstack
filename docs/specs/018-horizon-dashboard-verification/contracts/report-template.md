# Contract: Compatibility Report Template

**Feature**: SPEC-018 | **Date**: 2026-07-22

Skeleton for `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, mirroring `docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md`. Filled from host results (both passes). This is the graded deliverable (FR-008/011/013/015).

```markdown
# Horizon Dashboard Compatibility Report

**Ticket**: cobaltcore-dev/cloud-storage#482
**Date**: <YYYY-MM-DD>
**Verdict**: <e.g. "5/6 workflows working; console not working (F1)">

Supersedes the ungrounded claims in `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md`
(that document is <archived | linked as historical>). This report is grounded in a
reproducible procedure — see the runbooks under
`docs/specs/018-horizon-dashboard-verification/`.

## Environment

| Field | Stub pass | Real pass (graded) |
|---|---|---|
| Lightstack build/commit | … | … |
| Horizon version | quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble | (same) |
| Deployment | docker-compose-horizon.yml | k3s (root) + charts/lightstack realMode |
| Compute | n/a (stub) | libvirt/KVM |
| Image storage | n/a (stub) | Ceph RGW (S3), Rook on minikube |
| Volume storage | n/a (stub) | node-local /var/lib/lightstack/volumes (no RBD) |
| Browser access | localhost:80 | Horizon NodePort; Lightstack ClusterIP (endpointHost=<release>-lightstack) |

## Method

One paragraph: two manual passes; point at HOST_TESTING.md (stub) and
HOST_TESTING_REAL_MODE.md (real); note test/horizon_*.sh used as API-level evidence.

## Results — per workflow

| # | Workflow | Stub | Real (graded) | Evidence | Finding |
|---|---|---|---|---|---|
| 1 | Sign-in |  |  |  |  |
| 2 | Project switching |  |  |  |  |
| 3 | Create network |  |  |  |  |
| 4 | Create volume |  |  |  |  |
| 5 | Launch instance (+SSH) |  |  |  |  |
| 6 | Instance console (noVNC) | not-exercised | **not-working** | tab loads, no guest | F1 |

(Verdicts: working / partial / not-working / not-exercised.)

## Acceptance criteria (ticket #482)

| Criterion | Result |
|---|---|
| Login + project switching verified in real UI | … |
| Resource management: launch instance / create network / create volume | … |
| Report: per workflow working / not working (+ gaps) | ✅ this document |
| Report published to docs/ | ✅ |

## Findings

### F1 — noVNC console shows no guest (severity: major)
- **Cause**: synthetic `http://localhost:6080` console URL + unpersisted token + no libvirt VNC-port readback + an unwired noVNC proxy (`internal/nova/console.go`).
- **User impact**: the dashboard Console tab cannot display a VM; only the serial Console Log works (real mode).
- **Workaround**: `openstack console log show <server>`.
- **Follow-up**: <new ticket> — wire real noVNC (token store + libvirt VNC-port readback + real proxy + configurable URL).

### F2 … (as discovered)

## Notes & scope

- RBD/block Ceph out of scope (v1); real Ceph = RGW/S3 only.
- Automated browser suite out of scope; verification is manual/runbook-driven.
- Object storage / Swift panels out of scope (no Swift API; lightstack#15).

## Conclusion

<summary + link to follow-up tickets>
```
