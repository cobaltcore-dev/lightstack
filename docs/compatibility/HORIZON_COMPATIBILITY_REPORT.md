# Horizon Dashboard Compatibility Report

**Ticket**: [cobaltcore-dev/cloud-storage#482](https://github.com/cobaltcore-dev/cloud-storage/issues/482)
**Date**: stub pass 2026-07-22; real (graded) pass 2026-07-23
**Verdict**: **Both passes complete.** O3K's OpenStack **APIs work end-to-end** — the real pass booted a
**libvirt VM from a Ceph-RGW image**, gave it real Neutron networking + metadata/cloud-init SSH-key
injection, and **`ssh`'d in**; the serial console shows a real boot. **But the Horizon dashboard is not
usable for resource management**: every create/write workflow is broken — no Create-Network button (F3),
dead Create-Volume modal (F8), Volumes list **500s** once a volume exists (F9), Launch wizard can't select
a network (F7); plus the network lists **twice** (F4), image delete is a no-op (F6), the noVNC console is
unwired (F1), and the overview limits widget errors (F2). Read/list paths render; **write paths don't**.
**Net: O3K is API/CLI-compatible; the Horizon UI is _not_ a drop-in for resource management.** This
refutes SPEC-002's ungrounded "100% compatible" claim. **9 findings (F1–F9).**

> This report is filled from the two host passes documented in
> [`../specs/018-horizon-dashboard-verification/HOST_TESTING.md`](../specs/018-horizon-dashboard-verification/HOST_TESTING.md)
> (stub) and
> [`HOST_TESTING_REAL_MODE.md`](../specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md)
> (real, authoritative). It **supersedes** the ungrounded claims in
> [`../HORIZON_FULL_COMPATIBILITY_REPORT.md`](../HORIZON_FULL_COMPATIBILITY_REPORT.md)
> (SPEC-002's "100% compatible", never grounded in a browser-level check) — a superseded banner there now
> points here, leaving one authoritative statement (SC-003).

> **Update — 2026-07-24 (SPEC-019 / cloud-storage#532):** findings **F4, F5, F6, F9 are FIXED** and
> re-verified in **both** stub and real mode (runbooks under
> [`../specs/019-horizon-compat-api-fixes/`](../specs/019-horizon-compat-api-fixes/)). The create/write-UI
> cluster (**F2/F3/F7/F8**, #533) and the noVNC console (**F1**, #534) remain open. Net: the read/list panels
> now render (volumes no longer 500), the volume catalog is reachable, networks list once, and image delete
> works via the API — but Horizon is still not a drop-in for UI-driven resource *creation* (see #533). Each
> fixed finding is tagged ✅ inline; see the updated Follow-ups.

## Environment

| Field | Stub pass | Real pass (graded) |
|---|---|---|
| O3K build/commit | SPEC-018 branch as submitted in PR o3k#77 | (same; image `docker.io/library/o3k:dev`) |
| Horizon version | quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble | (same) |
| Deployment | `deployments/docker-compose-horizon.yml`, **rootless Podman** (`HORIZON_HTTP_PORT=8080`) | k3s (root) + `charts/o3k` realMode + `deployments/horizon-chart/` |
| Compute | n/a (stub) | libvirt/KVM |
| Image storage | Glance `local` backend → `o3k-images` compose volume (**not** stub) | Ceph RGW (S3), Rook on minikube |
| Volume storage | Cinder `local` backend → `o3k-volumes` compose volume (**not** stub) | node-local `/var/lib/o3k/volumes` (no RBD) |
| Browser access | `http://localhost:8080/dashboard` | Horizon NodePort `:30080` on the k3s node; O3K ClusterIP (`endpointHost=o3k-o3k`, Horizon calls it server-side) |

> No commit SHA is pinned above on purpose: the branch is rebased before merge, so any pre-merge SHA
> stops resolving. The durable reference is PR o3k#77 plus the two `HOST_TESTING*.md` runbooks it carries.

**F2 re-test (2026-08-25).** The F2 real-mode cell above was left `_TBC_` by the original real pass and
was settled by a targeted re-run, not a full third pass. Environment: same k3s node, `charts/o3k` in
real mode (libvirt real, neutron `iptables`, cinder local) + `deployments/horizon-chart` at NodePort
`:30080`, against a **fresh** PostgreSQL so quota rows were the migration-seeded defaults (zero
`hard_limit=0` rows — a poisoned quota row still returns HTTP 200 with a zero limit, so a dirty database
can fake this finding). Two deliberate deviations from `HOST_TESTING_REAL_MODE.md`: `glance.storageMode`
was `local` rather than Ceph RGW/S3, and the Overview was read by an authenticated HTTP scrape rather
than a browser — F2 involves no image, VM or object-storage path, and Horizon renders the message
server-side, so neither affects the verdict.

## Method

Two manual, runbook-driven passes (no automated browser suite). `test/horizon_dashboard_access_test.sh`
and `test/horizon_compat_test.sh` are used as supporting API-level evidence. The stub stack came up
green (o3k / horizon / postgres `healthy`; the memcached container has no healthcheck and the novnc
container reports unhealthy — irrelevant, the console is not-exercised in stub).

## Results — per workflow

| # | Workflow | Stub | Real (graded) | Evidence | Finding |
|---|---|---|---|---|---|
| 1 | Sign-in | **working** | **working** | Real (2026-07-23): login → Project Overview renders; Domain `Default`. | — |
| 2 | Project switching | **partial** | **working (switcher); re-scope not exercised** | Real: switcher present + correct (`Default`/`default`), but only the `default` project is seeded → nothing to switch to (fixture limitation, not a Horizon fault). F2 limits-error **reproduces in real** (2026-08-25), root-caused to O3K's Neutron quota-details omitting `reserved` — see F2. | F2 |
| 3 | Create network | **not working** | **not working** | **F3+F4 reproduce in real (2026-07-23)**: Project → Network → Networks still has **no Create button** (only Admin does); an Admin-created net lists **twice** in Project. Neutron-vs-UI dup source: see F4. F3's missing button root-caused 2026-08-25 to the same `reserved` gap as F2. | F3, F4 |
| 4 | Create volume | **not working** | **not working** (different cause) | Real (2026-07-23): volumev3 catalog is **clean** (`o3k-o3k:8776`) and CLI `volume create` → `available` — Cinder API works and F5's stub `localhost:8776` is **not** on Horizon's v3 path. Empty panel renders; **Create Volume button opens no dialog** (F8, create-UI cluster, not the endpoint). And once a volume exists the panel **500s** (`TemplateSyntaxError` in `get_rows`) → **F9**. | F5, F8, F9 |
| 5 | Launch instance (+SSH) | **not working** (UI) | UI **not working** (F7); **API/real compute WORKS** | Real (2026-07-23): UI wizard can't select a network (F7). But CLI `server create` → **ACTIVE**, `power_state Running`, real **libvirt VM booted from the RGW image**, `addresses=test-net=10.0.0.77`, `key_name`+`security_groups` present. Real compute + RGW-image boot **proven**; **`ssh cirros@10.0.0.77` succeeds** (`uid=1000` — cloud-init key injection + default-SG tcp/22 + reachable fixed IP all work). Console → T7. | F7 |
| 6 | Instance console (noVNC) | not-exercised | noVNC **not working** (F1) / serial log **works** | Real (2026-07-23): `console log show vm1` returns real cirros boot + EC2 metadata + `vm1 login:` (serial console **works**). noVNC Console **tab** → **"Unable to load Console" + "Console currently not available"** (F1 confirmed — no guest). | F1 |

_Verdicts: working / partial / not-working / not-exercised (stub-only cell) / pending._

## Acceptance criteria (ticket #482)

| Criterion | Result |
|---|---|
| Login + project switching verified in real UI | **Login ✅** (real). Project switching: switcher present + correct, but only `default` is seeded → re-scope not demonstrable (fixture limit, not a fault). |
| Resource management: launch / network / volume | **All create/write via the UI FAIL** — network ❌ (F3 no button, F4 dup), volume ❌ (F8 dead modal, F9 list 500s), launch ❌ (F7 can't select a network). Via **API/CLI all succeed** — real VM boots from the RGW image, gets an IP, SSH works. |
| Report: per workflow working / not working (+ gaps) | ✅ this document |
| Report published to `docs/` | ✅ |

## Findings

### F1 — noVNC console shows no guest (major) — _confirmed real (2026-07-23)_
- **Cause**: synthetic `http://localhost:6080` console URL + a random, unpersisted token + a
  fabricated VNC port (never read from libvirt) + an unwired noVNC proxy (`internal/nova/console.go`;
  `research.md` U1).
- **Impact**: the Console tab cannot display a VM. In real it shows **"Unable to load Console" +
  "Console currently not available"** — the returned URL points at `localhost`, so the browser never
  reaches the proxy at all. The serial Console Log works (verified real: real cirros boot output +
  `vm1 login:`).
- **Workaround**: `openstack console log show <server>`.
- **Follow-up**: wire real noVNC (token store + libvirt VNC-port readback + real proxy + configurable URL).

### F2 — Project Overview: "Unable to retrieve limits information" (medium) — _confirmed real (2026-08-25)_
- **Cause**: O3K's Neutron quota-details response omits `reserved`. Upstream Neutron returns
  `{limit, used, reserved}` per resource (`neutron/db/quota/driver.py`, `get_detailed_project_quotas`)
  and Horizon reads `detail['used'] + detail['reserved']` **unconditionally**
  (`openstack_dashboard/usage/quotas.py`, `_get_tenant_network_usages`), but O3K's `GetQuotaDetails`
  (`internal/neutron/network.go`) returns only `{limit, used}` for all seven resources — so the lookup
  raises `KeyError: 'reserved'`. Horizon catches the Nova and Cinder limit calls individually, each with
  its own message; the Neutron call has **no** `try/except`, so the error reaches the bare
  `except Exception` in `openstack_dashboard/usage/views.py` and renders as
  **"Unable to retrieve limits information. Details 'reserved'"** — the outer catch-all, which is why the
  message names no service. **Not a Nova fault**: `GET /v2.1/limits` returns 200 with all 13 keys Horizon
  reads, and Cinder's `GET /v3/{project}/limits` returns 200 with all 6. This supersedes the earlier
  "Nova limits/quota" attribution.
- **Impact**: the Overview usage panel errors on **every** load, not only after a project switch (the real
  pass hit it immediately post-login, with no switch). Project switching itself succeeds; the damage is a
  degraded first impression on the landing page.
- **Follow-up**: add `"reserved": 0` to each resource in `GetQuotaDetails` (`internal/neutron/network.go`).
  O3K holds no reservations, so a constant `0` is correct, and it covers all three keys
  `_get_tenant_network_usages` reads (`limit`, `used`, `reserved`). Product-code change — out of scope for
  this verification-only spec, and **not yet verified by re-test**.
- **Evidence (2026-08-25)**: rendered Overview HTML carries
  `Error: Unable to retrieve limits information. Details 'reserved'`, and the Horizon container's apache
  error log logs `horizon.exceptions WARNING  Recoverable error: 'reserved'` on every Overview load.
  `_get_tenant_network_usages` is the only site in `usage/quotas.py` that reads `'reserved'` from a
  response dict, which is what rules out the Nova and Cinder paths.

### F3 — No "Create Network" in the Project panel (medium) — _stub + real; root-caused 2026-08-25_
- **Cause**: **the same missing `reserved` key as F2.** Horizon's `CreateNetwork.allowed()`
  (`openstack_dashboard/dashboards/project/networks/tables.py`) calls
  `quotas.tenant_quota_usages(request, targets=('network',))` — the Neutron quota path — before rendering
  the action. That call raises `KeyError: 'reserved'` (see F2), so the action is never added and the
  Project → Network → Networks table ships without it. Admin → Network → Networks uses a table that does
  **not** gate on tenant quota, which is precisely why the button survives there. Note the action's own
  logic would have kept the button *enabled* on a mere quota miss (`allowed()` returns `True`
  unconditionally and defaults `available` to 1) — it disappears only because the lookup **raises**.
- **Impact**: unchanged — a project user cannot create a network from the standard Project panel, the
  ticket's named workflow location.
- **Evidence (2026-08-25)**: `/dashboard/project/networks/` renders only
  `networks__action_filter_project_networks`, and the string "Create Network" is absent;
  `/dashboard/admin/networks/` renders `networks__action_create` and the string is present. The
  `'reserved'` KeyError is logged on **every** project panel load (networks / volumes / instances one
  each, Overview two), yet only Networks loses its action — Volumes and Instances gate on the Cinder and
  Nova quota paths, which both return 200.
- **Follow-up**: the same one-key fix as F2 — add `"reserved": 0` to each resource in `GetQuotaDetails`
  (`internal/neutron/network.go`). Expected to close **F2 and F3 together**; not yet verified by re-test.

### F4 — Duplicate network listing in the Project panel (medium) — _stub + real (2026-07-23)_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
- **Cause**: a single network shows **twice** in Project → Network → Networks. Ruled out: **not** a Neutron
  API dup (`openstack network list` → **once**) and **not** a catalog dup (`catalog show network` → a single
  clean `public: http://o3k-o3k:9696`). So the duplication is in **how Horizon builds the Project panel** —
  it merges networks from several list queries (project-owned + `shared=true` + `router:external=true`), and
  O3K's Neutron most likely **isn't honoring those list filters**, so the same net is returned by more than
  one sub-query and rendered twice.
- **Impact**: confusing/incorrect network list (both passes).
- **Follow-up**: verify O3K Neutron `GET /v2.0/networks` honors the `shared` / `router:external` / project
  filters (a filtered query should not return a plain project net); compare against upstream Neutron.

### F5 — Volume (Cinder) catalog endpoint advertises `localhost` (major) — _stub_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
- **Cause**: migration `040` seeds the volume endpoint (`…013`) as `http://localhost:8776/v3`; migration
  `052` rewrites `…013` to `http://o3k:8776/v3`, but migration `054_cinder_volume_alias` adds a **second**
  volume endpoint (`…015`) hardcoded to `http://localhost:8776/v3`, which `052` (running earlier) never
  rewrites. Horizon resolves the aliased endpoint → `localhost:8776`, unreachable from the Horizon
  container. The runtime host-substitution only rewrites the `http://o3k:` prefix, so a `localhost`-seeded
  URL can't be rescued.
- **Real (2026-07-23)**: on Horizon's v3 path the endpoint is **clean** — `catalog show volumev3` =
  `o3k-o3k:8776`, the Volumes panel lists, and CLI `volume create` → `available`. So the `localhost:8776`
  alias (`…015`) is **off Horizon's path in real** (it sits on a non-`volumev3` service). The real Volumes
  failure — Create button opens no dialog — is the **create-UI** issue **F8**, not this endpoint.
- **Impact**: **stub** — the whole Volumes panel is dead (list + create). **Real** — list works; only the
  create modal is dead (→ F8).
- **Workaround**: none via the UI (CLI works).
- **Follow-up**: seed the alias endpoint (`…015`) with the configurable host (not `localhost`), and/or
  make catalog host-substitution rewrite `localhost:` too. Verify against #481 Terraform (volume path).

### F6 — Glance image delete is a no-op (medium) — _stub_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
- **Cause**: `openstack image delete <id>` returns success (no error) but the image stays in
  `image list`. Two explicitly-deleted images (`70c6ee27…`, `32d8ea8e…`) reappeared alongside a freshly
  created one — three `cirros` images after two deletes.
- **Impact**: images can't be removed via CLI or the Horizon Images panel; the delete action appears to
  succeed but leaves the image behind.
- **Follow-up**: inspect the Glance DELETE handler (stub mode) — it likely returns 2xx without removing
  the row. (Commit `ce2cf14` previously touched "image delete idempotency"; re-verify.) Confirm whether
  it recurs in real mode (RGW-backed).

### F7 — Launch Instance not completable via the dashboard (major) — _stub + real (2026-07-23)_
- **Cause**: the Instances panel has **no "Launch Instance" button** (the wizard is only reachable via
  Project → Compute → Images → Launch). In that wizard the **Networks step won't let you select a
  network**, so the required step never validates and Launch stays disabled — no instance can be
  launched through the UI.
- **Real (2026-07-23)**: reproduces — the Networks step shows **"Available 2"** (the single `test-net`
  duplicated, per F4) but the table renders **no selectable rows**, so no network can be allocated. The
  NG wizard can't render/select O3K's networks (same data-rendering family as F9's volume table).
- **Contrast (API works)**: `openstack server create --image cirros --flavor m1.small --network test-nw`
  → server ACTIVE with `addresses=test-nw=192.168.0.201`; the Instances panel then renders it (so
  list/read and the `addresses` regression-guard pass; the #524 `key_name`/`security_groups` readback is
  present).
- **Impact**: the ticket's core "launch an instance via the dashboard" workflow cannot be completed.
- **Possible common root — partly answered (2026-08-25)**: the quota/limits theory holds for F2 and F3
  (one root: Neutron quota-details omits `reserved`), but **not** for this finding. The Instances table's
  launch action *is* present in the server-rendered HTML (`instances__action_launch-ng`), and it gates on
  the Nova quota path, which returns 200. So F7 is not a casualty of the quota error. Caveat: the
  re-test read HTML over HTTP and cannot execute JavaScript, so whatever hides or breaks the control in a
  real browser remains unmeasured here.
- **Follow-up**: (1) why the NG wizard's Networks step won't select an O3K network; (2) why Launch/Create
  action buttons are absent on Project panels (quota/limits/policy).

### F8 — Horizon resource-*creation* UIs are broken while the APIs work (major) — _stub + real (2026-07-23)_
- **Pattern**: across panels, O3K's APIs work (proven by CLI — network / volume / server all create fine),
  but Horizon's **create/write UIs fail**: Project → Networks has **no Create button** (F3); Project →
  Volumes Create button **opens no dialog** (T4); the Launch-Instance wizard **can't complete** / no Launch
  button on Instances (F7); and the Overview **limits widget errors** (F2). Read paths — panels render,
  lists, server-detail — work.
- **Impact**: a developer **cannot create resources through the dashboard** — the ticket's core "resource
  management in the UI" goal fails, even though every one of those operations succeeds via the API/CLI.
- **Likely common root — confirmed in part, narrowed in part (2026-08-25)**: F2 and F3 **do** share a
  single root — O3K's Neutron quota-details response omits `reserved`, which makes every
  `tenant_quota_usages` call raise (see F2). That is the "visible tip" this section predicted, and one
  key fixes both. But it does **not** extend to F7 or the volume-create modal: their create actions are
  present in the server-rendered HTML (`instances__action_launch-ng`, `volumes__action_create`) and they
  gate on the Nova and Cinder quota paths, which return 200 with every key Horizon reads. Those two are
  separate, client-side failures.
- **Follow-up**: **two** investigations, not one. (1) Add `"reserved": 0` in `GetQuotaDetails`
  (`internal/neutron/network.go`) — closes F2 + F3. (2) Separately, why the Volumes modal never opens and
  the Launch wizard cannot validate; both are JS-level and need a browser, not an API-level trace.

### F9 — Volumes panel 500s (TemplateSyntaxError) once a volume exists (major) — _real (2026-07-23)_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
- **Cause**: with `test-vol-cli` present, `GET /dashboard/project/volumes/` throws `TemplateSyntaxError`
  in `horizon/tables/base.py:1948 get_rows` rendering `horizon/common/_data_table.html`. O3K's Cinder
  volume response is **heavily null-populated** — `openstack volume show` returns `type`/`volume_type_id`,
  `availability_zone`, `encrypted`, `os-vol-tenant-attr:tenant_id`, `user_id`, `os-vol-host-attr:host`,
  `multiattach`, `consumes_quota`, `shared_targets` all **`null`** — and Horizon's volume **table** can't
  render a row from it (a column derived from one of those nulls raises). Empty panel renders fine; a
  populated one 500s.
- **Impact**: the Volumes panel is unusable whenever any volume exists — a **read/list** break on top of
  the create-UI break (F8). Distinct from F5 (the v3 endpoint is clean in real).
- **Follow-up**: the full Django traceback is in the Horizon **container's apache error log** (pod stdout
  only shows kolla/apache startup): `kubectl -n o3k exec deploy/horizon-o3k-horizon -- sh -c 'tail -80 /var/log/apache2/*error*'`.
  Pinpoint the failing column (likely `encrypted` / `volume_type` / `availability_zone` being null), then
  have O3K's Cinder populate those fields to non-null defaults (cf. upstream Cinder's volume schema).

## Notes & scope

- RBD / block Ceph out of scope (v1); real Ceph = RGW/S3 only.
- Automated browser suite out of scope; verification is manual/runbook-driven.
- Object storage / Swift panels out of scope (no Swift API; o3k#15).
- Stub environment note: Horizon's host port was parametrized (`HORIZON_HTTP_PORT`) to run under
  rootless Podman (`:8080`); not an O3K issue.

## Conclusion

Both passes are complete, and the result is unambiguous.

**What works.** O3K's OpenStack **API/CLI surface is compatible** and its **real backends work end-to-end**:
sign-in and the read/list dashboard panels render, and via the API the real pass **booted a libvirt/KVM VM
from a Ceph-RGW image**, attached it to real Neutron networking with a reachable fixed IP, had
**metadata/cloud-init inject the SSH key**, honored the **default security group** (tcp/22), and served a
real **serial console** — SSH login succeeded (`uid=1000(cirros)`). Volume and server creation succeed via
CLI. This is the exact backend chain the stub pass could not exercise, and it passed.

**What doesn't.** The **Horizon dashboard is not usable for resource management.** Every create/write
workflow through the UI fails: no Create-Network button in the Project panel (F3), a dead Create-Volume
modal (F8), the Volumes list itself **500s** once a volume exists (F9), and the Launch-Instance wizard
**can't select a network** (F7). Alongside: networks list **twice** (F4), image delete is a **no-op** (F6),
the **noVNC console** is unwired (F1), and the overview **limits widget errors** (F2). The common thread
(F8): Horizon's create flows and table renderers depend on quota/limits/type metadata and fully-populated
resource fields that O3K under-serves — so buttons hide, modals don't open, wizard steps won't validate,
and populated tables crash, even though the underlying APIs work. Two of those are now pinned precisely
(2026-08-25): **F2 and F3 are the same one-key defect** — O3K's Neutron quota-details omits `reserved`, so
every `tenant_quota_usages` call raises, erroring the Overview widget and deleting the Project
Create-Network action. The dead volume modal and the Launch wizard are **not** the same defect: their
buttons render and their quota paths are healthy, so they are separate client-side failures.

**Answer to #482.** O3K is a **drop-in for the OpenStack API/CLI**, but the **Horizon dashboard is _not_ a
drop-in for UI-driven resource management** as of the SPEC-018 branch in PR o3k#77. This **refutes the
prior `HORIZON_FULL_COMPATIBILITY_REPORT.md` "100% compatible / 342 endpoints" claim** — the endpoint
coverage it counted is real, but it does not translate into a working dashboard for creating resources.

**Follow-ups** (separate tickets — this is verify-not-fix scope, as SPEC-016 spun off #524):
- **F2 + F3 (highest value, and now a known one-line fix)** — add `"reserved": 0` to each resource in
  `GetQuotaDetails` (`internal/neutron/network.go`). Upstream Neutron returns `{limit, used, reserved}`
  and Horizon reads all three unconditionally; O3K omits `reserved`, so the Overview limits widget errors
  and the Project Create-Network action is dropped. Verified as the cause 2026-08-25; the fix itself is
  not yet re-tested.
- **F7 + volume-create modal (separate from the above)** — their create actions render server-side and the
  Nova/Cinder quota and limits calls return 200 with every key Horizon reads, so the quota theory does not
  explain them. Needs a browser-level (JavaScript) investigation, not an API trace.
- ✅ **F4, F5, F6, F9 — DONE (SPEC-019 / cloud-storage#532, 2026-07-24):** O3K Neutron now honors the
  `shared`/`router:external`/project list filters so each network lists once (F4); the `volume` catalog alias
  advertises the configured host, not `localhost` (F5); the Cinder volume response carries the full non-null
  upstream field set so Horizon's volume table renders instead of 500-ing (F9); and Glance image delete
  removes the image with owner-or-admin authorization, returning 404/403 appropriately (F6). Verified in
  **both** stub and real mode.
- **F8 cluster (open — #533)** — trace the Nova/Cinder/Neutron quota+limits+types calls Horizon makes
  when opening each create UI; one root fix likely re-enables F2/F3/F7 and the volume-create modal. *(Note:
  Horizon's row **Delete** buttons are part of this write-UI cluster — the F6 fix restores delete via the
  API/CLI, but the dashboard's delete action stays broken until #533.)*
- **F1 (open — #534)** — wire a real noVNC console (token store + libvirt VNC-port readback + real proxy + configurable URL).

**Reproducibility (SC-005 met).** Both passes are fully scripted in the two `HOST_TESTING*.md` runbooks;
the real pass reuses the SPEC-014 `deployments/rook/setup-rook-rgw.sh` fixture.
