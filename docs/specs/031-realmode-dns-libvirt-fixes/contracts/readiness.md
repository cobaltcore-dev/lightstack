# Contract: Readiness in real mode

**Feature**: `031-realmode-dns-libvirt-fixes` — amends SPEC-013 FR-001 for one case (spec Clarifications, FR-017)

## C-READY-1 — Inputs

| Mode | `GET /readyz` answers `200` when |
|---|---|
| stub | the database is reachable, every listener is bound, the pod is not draining (unchanged) |
| real | all of the above **and** the console socket directory has been prepared at least once |

## C-READY-2 — Stickiness

Once the preparation has succeeded, nothing about libvirt affects readiness again:

- a lost connection does not;
- a reconnect does not;
- a redo of the preparation that fails does not.

SPEC-013 FR-001 keeps holding for outages.

## C-READY-3 — The answer

| State | Status | Reason (text and JSON `reasons`) |
|---|---|---|
| not yet prepared | `503` | `hypervisor unavailable` |
| not yet prepared, database down | `503` | `database,hypervisor unavailable` |
| prepared | `200` | `OK` (unchanged) |

- No error detail, as the database reason (CONTRACT.md §2).
- The answer reads recorded state and makes no libvirt call, so the 800 ms per-check bound is never at stake.
- `GET|HEAD /healthcheck` mirrors it.

## C-READY-4 — Consequences an operator sees

| Situation | Effect |
|---|---|
| single node, not yet prepared | the Service has no endpoint: all six APIs are unreachable through it. The listeners still answer on the pod's own address — the node's, with host networking |
| preparation never succeeds | the startup probe (30 × 5 s by default) restarts the pod, ending in CrashLoopBackOff with the cause at the top of each start's log; `helm install --wait` fails |
| multi-node | only the affected node's pod leaves the Service |
| Docker Compose, bare metal | nothing gates on it; the Compose healthchecks probe `GET /v3`, not `/readyz` |
