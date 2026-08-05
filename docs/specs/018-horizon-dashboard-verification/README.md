# SPEC-018 — Verify Horizon dashboard compatibility

Tracking: [cloud-storage#482](https://github.com/cobaltcore-dev/cloud-storage/issues/482).
Verification-only (no product code): a Horizon deploy artifact + host runbooks + a graded report.
The Horizon analog of SPEC-016 (Terraform provider verification).

**Goal.** Prove an **unmodified** upstream OpenStack Horizon dashboard (kolla Flamingo 2025.2) works
against O3K for the six developer workflows — **sign-in, project switching, launch instance (+SSH),
create network, create volume, instance console** — and publish a reproducible, per-workflow
working / not-working / partial report under `docs/`.

Two manual passes: a **portable stub** pass (docker-compose, laptop-grade) and a **graded real**
pass (authoritative) — real-mode O3K on root k3s + Rook/Ceph-on-minikube RGW/S3, no RBD.

## Contents / where things live

| Artifact | Path | Tracked |
|---|---|:--:|
| Spec / plan / research / design | `spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/` | ✅ |
| Stub-pass runbook (docker-compose) | `HOST_TESTING.md` | ✅ |
| Real-pass runbook (k3s + minikube/Rook-Ceph) | `HOST_TESTING_REAL_MODE.md` | ✅ |
| Verification Horizon Helm chart | `deployments/horizon-chart/` | ✅ (new; **not** the product `charts/o3k`) |
| Reused dashboard config | `deployments/horizon-config/*` | ✅ (existing; copied into the chart's `files/`) |
| Reused stub stack | `deployments/docker-compose-horizon.yml` | ✅ (existing, as-is) |
| Graded report (deliverable) | `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` | ✅ (skeleton; verdicts written from host results) |
| Supporting API-level evidence | `test/horizon_*.sh` | ✅ (existing, reused) |

## Flow

1. **Stub pass** — follow `HOST_TESTING.md` (one `docker compose up`) → walk the workflows → note verdicts.
2. **Real pass** — follow `HOST_TESTING_REAL_MODE.md` (k3s real-mode O3K + Rook/Ceph RGW + this chart) →
   walk the workflows, SSH into the launched VM → note the **authoritative** verdicts.
3. Fill `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` (per `contracts/report-template.md`);
   reconcile/supersede the prior `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md`; open follow-ups for findings.

## Known result before you start

The **noVNC Console tab is a predetermined "not working"** (finding **F1**, `research.md` U1): O3K's Nova
returns a hardcoded `localhost:6080` console URL with an unpersisted token and no libvirt VNC-port
readback. The serial **Console Log** (`console log show`) works in real mode. US3's job is to *reproduce
and document* this, then spin off the fix (as SPEC-016 spun off #524).
