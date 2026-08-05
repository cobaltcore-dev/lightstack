# Contract: Workflow Verification Matrix

**Feature**: SPEC-018 | **Date**: 2026-07-22

The "contract" for this verification is the surface Horizon exercises against O3K per workflow, plus the acceptance check that decides the verdict. Expected verdicts are for the **graded (real) pass**; the stub pass proves the UI/API contract and marks backend-dependent items `not-exercised`.

Horizon reaches O3K **server-side** (Django pod → O3K, incl. the AngularJS `/api/` proxy) — see `research.md` U3. So all rows below are pod→O3K over the in-cluster catalog, except the console URL, which the browser opens directly.

| # | Workflow | Horizon UI action | O3K API surface (server-side) | Acceptance check | Expected (real) | Guard / notes |
|---|---|---|---|---|---|---|
| 1 | sign-in | Log in (user/pass/domain) → overview | `POST /v3/auth/tokens` (unscoped→scoped); catalog read | Overview renders; catalog has identity/compute/network/volumev3/image with a **public** endpoint each | working | catalog shape checked by `test/horizon_dashboard_access_test.sh` |
| 2 | project-switch | Switch project/domain in the header | Re-scoped `POST /v3/auth/tokens`; per-project resource lists | View re-scopes; project's resources shown | working | exercises Project Isolation (Principle V) |
| 3 | create-network | Project → Network → Networks → Create | `POST /v2.0/networks`, `POST /v2.0/subnets`; `GET /v2.0/networks` | Network+subnet listed; selectable in launch form | working | **regression-guard**: Neutron catalog URL `…:9696` with **no** `/v2.0` suffix (`test/horizon_critical_test.sh`; `docs/archive/HORIZON_FIXES.md`) |
| 4 | create-volume | Project → Volumes → Volumes → Create | `POST /v3/{project}/volumes`; `GET …/volumes` | Volume listed with a status | working | node-local backend (no RBD); attach is a separate/known-gap check |
| 5 | launch-instance | Project → Compute → Instances → Launch Instance (NG wizard) | wizard reads Glance images / Nova flavors / `os-keypairs` / Neutron nets; `POST /v2.1/servers`; `GET /servers/detail` | Instance created + listed with status; **then SSH into it** with the selected key | working | image lazily fetched from **RGW/S3** on boot (`EnsureLocalBase`, MD5-verified); **regression-guard**: server-detail includes `addresses` (`internal/nova/handlers.go`; #524 populated `key_name`+`security_groups`); SSH needs default SG tcp/22 + cirros cloud-init key |
| 6 | instance-console | Instance → Console tab (noVNC) | `POST /v2.1/servers/{id}/remote-consoles` (or `os-getVNCConsole`) → returns `http://localhost:6080/...`; browser opens it | noVNC viewer connects and shows the guest framebuffer | **not-working** | **predetermined** (`research.md` U1): synthetic localhost URL + unpersisted token + no VNC-port readback + an unwired noVNC proxy. Serial **Console Log** (`os-getConsoleOutput`) works in real mode → report as the working console-adjacent feature |

## Non-regression set (must still render)

From `docs/archive/HORIZON_FIXES.md` — a regression here breaks a whole panel and is reported `not-working`:
- Nova server-detail **`addresses`** field present (Instances panel).
- Neutron catalog URL **without** a doubled `/v2.0` (Networks/Routers panels).

## Out-of-matrix (opportunistic, non-gating)

Images, key pairs, security groups, floating IPs, routers, quotas — the existing `test/horizon_dashboard_test.sh` touches these; report opportunistically, they do not gate.
