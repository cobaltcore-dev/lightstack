# Horizon Dashboard Compatibility Report

**Ticket**: [cobaltcore-dev/cloud-storage#482](https://github.com/cobaltcore-dev/cloud-storage/issues/482)
**Date**: stub pass 2026-07-22; real (graded) pass 2026-07-23; stub **and** real re-test 2026-09-22 (root-caused F4/F7, added F10 and F11, project switching closed as **working** — **domain** switching graded separately and **not working**, F11)
**Verdict**: **Both passes complete.** Lightstack's OpenStack **APIs work end-to-end** — the real pass booted a
**libvirt VM from a Ceph-RGW image**, gave it real Neutron networking + metadata/cloud-init SSH-key
injection, and **`ssh`'d in**; the serial console shows a real boot. **But the Horizon dashboard is not
usable for resource management**: every create/write workflow is broken — no Create-Network button (F3),
dead Create-Volume modal (F8), Volumes list **500s** once a volume exists (F9), Launch wizard can't select
a network (F7); plus the network lists **twice** (F4), image delete is a no-op (F6), the noVNC console is
unwired (F1), and the overview limits widget errors (F2). Read/list paths render; **write paths don't**.
**Net: every API/CLI _operation_ exercised here succeeds — CRUD via `openstack` works end-to-end — but
several API _responses_ deviate from upstream, and **seven of the eleven** findings are Lightstack API defects
rather than Horizon ones: Neutron quota-details omits `reserved` (F2/F3), Neutron ignores the
`shared`/`router:external`/project list filters (F4 — which is also the proven root of F7's dead Networks
step), the Cinder volume response leaves `type` / `availability_zone` / `encrypted` null (F9),
`openstack image delete` is a no-op (F6), the Neutron security-group list omits
`security_group_rules`, 500-ing Horizon's SG proxy (F10), and Keystone discards the `domain` scope so
a domain-scoped token request silently returns an **unscoped** token (F11). So "the APIs
work, the UI doesn't" is too clean: the write _paths_ work, while the response _shapes_ are what break
Horizon. The Horizon UI is _not_ a drop-in for resource management.** This
refutes SPEC-002's ungrounded "100% compatible" claim. **11 findings (F1–F11).**

> This report is filled from the two host passes documented in
> [`../specs/018-horizon-dashboard-verification/HOST_TESTING.md`](../specs/018-horizon-dashboard-verification/HOST_TESTING.md)
> (stub) and
> [`HOST_TESTING_REAL_MODE.md`](../specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md)
> (real, authoritative). It **supersedes** the ungrounded claims in
> [`../HORIZON_FULL_COMPATIBILITY_REPORT.md`](../HORIZON_FULL_COMPATIBILITY_REPORT.md)
> (SPEC-002's "100% compatible", never grounded in a browser-level check) — a superseded banner there now
> points here, leaving one authoritative statement (SC-003).

## Environment

| Field | Stub pass | Real pass (graded) |
|---|---|---|
| Lightstack build/commit | SPEC-018 branch as submitted in PR lightstack#77 | (same; image `docker.io/library/lightstack:dev`) |
| Horizon version | quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble | (same) |
| Deployment | `deployments/docker-compose-horizon.yml`, **rootless Podman** (`HORIZON_HTTP_PORT=8080`) | k3s (root) + `charts/lightstack` realMode + `deployments/horizon-chart/` |
| Compute | n/a (stub) | libvirt/KVM |
| Image storage | Glance `local` backend → `lightstack-images` compose volume (**not** stub) | Ceph RGW (S3), Rook on minikube |
| Volume storage | Cinder `local` backend → `lightstack-volumes` compose volume (**not** stub) | node-local `/var/lib/lightstack/volumes` (no RBD) |
| Browser access | `http://localhost:8080/dashboard` | Horizon NodePort `:30080` on the k3s node; Lightstack ClusterIP (`endpointHost=lightstack-lightstack`, Horizon calls it server-side) |

> No commit SHA is pinned above: the SHAs each pass ran against were not captured at the time, and the
> branch has been rebased and amended since, so they can no longer be recovered. (A pre-merge commit does
> stay fetchable via `refs/pull/77/head` even after a force-push — what is missing is the record, not the
> object.) The durable reference is therefore PR lightstack#77 plus the two `HOST_TESTING*.md` runbooks it
> carries, read together with the pass dates above. Future passes should record `git rev-parse HEAD` and
> the built image ID at pass time, so a later report can pin the tested build exactly.

**F2 re-test (2026-08-25).** The F2 real-mode cell above was left `_TBC_` by the original real pass and
was settled by a targeted re-run, not a full third pass. Environment: same k3s node, `charts/lightstack` in
real mode (libvirt real, neutron `iptables`, cinder local) + `deployments/horizon-chart` at NodePort
`:30080`, against a **fresh** PostgreSQL so quota rows were the migration-seeded defaults (zero
`hard_limit=0` rows — a poisoned quota row still returns HTTP 200 with a zero limit, so a dirty database
can fake this finding). Two deliberate deviations from `HOST_TESTING_REAL_MODE.md`: `glance.storageMode`
was `local` rather than Ceph RGW/S3, and the Overview was read by an authenticated HTTP scrape rather
than a browser — F2 involves no image, VM or object-storage path, and Horizon renders the message
server-side, so neither affects the verdict.

## Method

Two manual, runbook-driven passes (no automated browser suite), plus a **2026-09-22 re-test in both modes**
that drove the Launch-Instance wizard with a network and an active image present and read Horizon's own
REST proxy from a logged-in session — which is what root-caused F4/F7 and surfaced F10. The real half ran
on k3s (Lightstack `hostNetwork`, Horizon NodePort 30080, Glance→Ceph RGW) and reproduced **every** finding
identically to stub; F3, F4 and F10 were each re-confirmed mechanically (Create-Network action absent from
`/dashboard/project/networks/` but present in `/dashboard/admin/networks/`; the networks proxy returning two
items with one `id`; the security-group proxy returning 500). F5 again did **not** recur on Horizon's path —
the real catalog is `volumev3 → lightstack-lightstack:8776`, with the stale `volume → localhost:8776` alias
sitting off it.
`test/horizon_dashboard_access_test.sh` and `test/horizon_compat_test.sh` are used as supporting
API-level evidence. The stub stack came up
green (lightstack / horizon / postgres `healthy`; the memcached container has no healthcheck and the novnc
container reports unhealthy — irrelevant, the console is not-exercised in stub).

## Results — per workflow

| # | Workflow | Stub | Real (graded) | Evidence | Finding |
|---|---|---|---|---|---|
| 1 | Sign-in | **working** | **working** | Real (2026-07-23): login → Project Overview renders; Domain `Default`. | — |
| 2a | Project switching | **working** (2026-09-22) | **working** (2026-09-22) | Was `partial` in both passes only because a single seeded project left nothing to switch to — a **fixture limitation, never a Horizon or Lightstack fault**. With a second project (`verify-two` + an admin role assignment) seeded, switching **re-scopes correctly in both passes**, confirmed in the browser 2026-09-22 and corroborated at the session level: `/dashboard/auth/switch/<id>/` → `302`, the page then reports `verify-two`, and `/dashboard/api/neutron/networks/` drops from 2 items to **0** (the fixtures belong to `default`, so an empty view *is* the re-scope). Both runbooks now seed the second project, so this is reproducible. Unrelated: the F2 limits-error still **reproduces in real** on the Overview panel — see F2. | F2 |
| 2b | Domain switching | not-exercised | **not working** (2026-09-22, code-level) | The other half of FR-003 and of workflow-matrix row 2 ("switch project/**domain** in the header"), graded **separately from 2a**. `local_settings:79` sets `OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True`, so Horizon offers domain context and requests a domain-scoped token; Lightstack discards the `domain` scope while parsing and answers with a normal-looking **unscoped** token, so domain-context panels 403 or render empty and nothing reports that the scope was dropped (**F11**). Root-caused in code rather than observed in the browser, and the parsing gap is mode-independent, so the stub pass would fail identically — not-working rather than unexercised. | F11 |
| 3 | Create network | **not working** | **not working** | **F3+F4 reproduce in real (2026-07-23)**; F4 root-caused 2026-09-22 to `ListNetworks` ignoring the `shared` filter (fixed on `feature/532`, not on `main`). Project → Network → Networks still has **no Create button** (only Admin does); an Admin-created net lists **twice** in Project. Neutron-vs-UI dup source: see F4. F3's missing button root-caused 2026-08-25 to the same `reserved` gap as F2. | F3, F4 |
| 4 | Create volume | **not working** | **not working** (different cause) | Real (2026-07-23): volumev3 catalog is **clean** (`lightstack-lightstack:8776`) and CLI `volume create` → `available` — Cinder API works and F5's stub `localhost:8776` is **not** on Horizon's v3 path. Empty panel renders; **Create Volume button opens no dialog** (F8, create-UI cluster, not the endpoint). And once a volume exists the panel **500s** (`TemplateSyntaxError` in `get_rows`) → **F9**. | F5, F8, F9 |
| 5 | Launch instance (+SSH) | **not working** (UI) | UI **not working** (F7, root = F4); **API/real compute WORKS** | Real (2026-07-23): UI wizard can't select a network (F7). But CLI `server create` → **ACTIVE**, `power_state Running`, real **libvirt VM booted from the RGW image**, `addresses=test-net=10.0.0.77`, `key_name`+`security_groups` present. **Re-confirmed 2026-09-22** (CLI, after the UI wizard failed again): `vm1` **ACTIVE**, `power_state Running`,
`addresses=test-net=10.0.0.57`, `key_name=verifier-key`, `security_groups=default` — and the **Instances panel
renders it** (`/dashboard/project/instances/` → 200 with the row present), so a *populated* instances table is
fine where a populated volumes table 500s (F9). Real compute + RGW-image boot **proven**; **`ssh cirros@10.0.0.77` succeeds** (`uid=1000` — cloud-init key injection + default-SG tcp/22 + reachable fixed IP all work). Console → T7. | F7 |
| 6 | Instance console (noVNC) | not-exercised | noVNC **not working** (F1) / serial log **works** | Real (2026-07-23): `console log show vm1` returns real cirros boot + EC2 metadata + `vm1 login:` (serial console **works**). noVNC Console **tab** → **"Unable to load Console" + "Console currently not available"** (F1 confirmed — no guest). | F1 |

_Verdicts: working / partial / not-working / not-exercised (stub-only cell) / pending._

## Acceptance criteria (ticket #482)

| Criterion | Result |
|---|---|
| Login + project switching verified in real UI | **Both ✅** (real). Login verified 2026-07-23. Project switching verified **2026-09-22** once a second project was seeded — the switcher lists both and selecting the other **re-scopes the view**. The earlier `partial` was a fixture limit (one seeded project), not a fault. **Domain** switching is not part of this criterion — the ticket asks for project switching — and is graded separately as workflow **2b**, where it **fails** (F11). |
| Resource management: launch / network / volume | **All create/write via the UI FAIL** — network ❌ (F3 no button, F4 dup), volume ❌ (F8 dead modal, F9 list 500s), launch ❌ (F7 can't select a network — root-caused 2026-09-22 to F4). Via **API/CLI all succeed** — real VM boots from the RGW image, gets an IP, SSH works. |
| Report: per workflow working / not working (+ gaps) | ✅ this document |
| Report published to `docs/` | ✅ |

## Findings

### F1 — noVNC console shows no guest (major) — _confirmed real 2026-07-23; re-confirmed 2026-09-22_
- **Cause**: synthetic `http://localhost:6080` console URL + a random, unpersisted token + a
  fabricated VNC port (never read from libvirt) + an unwired noVNC proxy (`internal/nova/console.go`;
  `research.md` U1).
- **Impact**: the Console tab cannot display a VM. In real it shows **"Unable to load Console" +
  "Console currently not available"** — the returned URL points at `localhost`, so the browser never
  reaches the proxy at all. The serial Console Log works (verified real: real cirros boot output +
  `vm1 login:`).
- **Re-confirmed (2026-09-22, real)**: unchanged at the API level —
  `POST /v2.1/servers/<id>/remote-consoles` (`compute 2.6`) returns
  `{"url": "http://localhost:6080/vnc_auto.html?token=<random>"}`, i.e. still the synthetic `localhost`
  URL with an unpersisted token.
- **Workaround**: `openstack console log show <server>`.
- **Follow-up**: wire real noVNC (token store + libvirt VNC-port readback + real proxy + configurable URL).
- **Tracked**: [cloud-storage#534](https://github.com/cobaltcore-dev/cloud-storage/issues/534)

### F2 — Project Overview: "Unable to retrieve limits information" (medium) — _confirmed real (2026-08-25)_
- **Cause**: Lightstack's Neutron quota-details response omits `reserved`. Upstream Neutron returns
  `{limit, used, reserved}` per resource (`neutron/db/quota/driver.py`, `get_detailed_project_quotas`)
  and Horizon reads `detail['used'] + detail['reserved']` **unconditionally**
  (`openstack_dashboard/usage/quotas.py`, `_get_tenant_network_usages`), but Lightstack's `GetQuotaDetails`
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
  Lightstack holds no reservations, so a constant `0` is correct, and it covers all three keys
  `_get_tenant_network_usages` reads (`limit`, `used`, `reserved`). Product-code change — out of scope for
  this verification-only spec, and **not yet verified by re-test**.
- **Evidence (2026-08-25)**: rendered Overview HTML carries
  `Error: Unable to retrieve limits information. Details 'reserved'`, and the Horizon container's apache
  error log logs `horizon.exceptions WARNING  Recoverable error: 'reserved'` on every Overview load.
  `_get_tenant_network_usages` is the only site in `usage/quotas.py` that reads `'reserved'` from a
  response dict, which is what rules out the Nova and Cinder paths.
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

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
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

### F4 — Duplicate network listing in the Project panel (medium) — _stub + real; root-caused 2026-09-22_
- **Cause — proven (2026-09-22)**: `ListNetworks` (`internal/neutron/network.go:477`) **ignores the `shared`
  and `router:external` list filters**. It parses only `id` and `name` (added for the Terraform provider,
  SPEC-009 / SC-007) and always queries `WHERE (project_id = $1 OR shared = true)`. Horizon's
  `network_list_for_tenant` (`openstack_dashboard/api/neutron.py:1604`) builds the panel by **concatenating
  separate queries** — its own comment at :1628 lists them as (1) `shared=True`, (2) project non-shared,
  (3) external non-shared non-project. Because Lightstack answers (1) and (2) identically, the same row comes
  back from both and is rendered twice. (Correctly ruled out earlier: not a raw Neutron dup —
  `openstack network list` → once — and not a catalog dup.)
- **Evidence (2026-09-22, stub **and** real)**: identical in both modes — this is a plain SQL/handler defect
  with no mode-dependent path. With one non-shared project network present,
  ```
  GET /v2.0/networks?shared=True           -> 1 ["test-net"]   # test-net is NOT shared
  GET /v2.0/networks?shared=False          -> 1 ["test-net"]
  GET /v2.0/networks?router:external=True  -> 1 ["test-net"]   # Lightstack models no external nets
  GET /v2.0/networks?name=nonexistent      -> 0 []             # `name` IS honoured
  ```
  and Horizon's own proxy `GET /dashboard/api/neutron/networks/` returns **two items with the same `id`**
  (verified from a logged-in session against stub `:8080` and again against real-mode k3s `:30080`).
- **Impact**: confusing/incorrect network list (both passes) — and, per **F7**, the duplicate `id` is what
  empties the Launch-Instance wizard's Networks table.
- **Fixed on `feature/532`** (`9e17dfc`, not yet on `main`): `buildNetworkListFilter` honours
  `shared` / `router:external` / `tenant_id` / `id` / `name`, making Horizon's sub-queries disjoint, and
  short-circuits `router:external=true` to an empty list. This report's passes ran on `feature/482`, which
  is the base of that chain and therefore predates the fix.
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

### F5 — Volume (Cinder) catalog endpoint advertises `localhost` (major) — _stub_
- **Cause**: migration `040` seeds the volume endpoint (`…013`) as `http://localhost:8776/v3`; migration
  `052` rewrites `…013` to `http://lightstack:8776/v3`, but migration `054_cinder_volume_alias` adds a **second**
  volume endpoint (`…015`) hardcoded to `http://localhost:8776/v3`, which `052` (running earlier) never
  rewrites. Horizon resolves the aliased endpoint → `localhost:8776`, unreachable from the Horizon
  container. The runtime host-substitution only rewrites the `http://lightstack:` prefix, so a `localhost`-seeded
  URL can't be rescued.
- **Real (2026-07-23)**: on Horizon's v3 path the endpoint is **clean** — `catalog show volumev3` =
  `lightstack-lightstack:8776`, the Volumes panel lists, and CLI `volume create` → `available`. So the `localhost:8776`
  alias (`…015`) is **off Horizon's path in real** (it sits on a non-`volumev3` service). The real Volumes
  failure — Create button opens no dialog — is the **create-UI** issue **F8**, not this endpoint.
- **Impact**: **stub** — the whole Volumes panel is dead (list + create). **Real** — list works; only the
  create modal is dead (→ F8).
- **Workaround**: none via the UI (CLI works).
- **Follow-up**: seed the alias endpoint (`…015`) with the configurable host (not `localhost`), and/or
  make catalog host-substitution rewrite `localhost:` too. Verify against #481 Terraform (volume path).
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

### F6 — Glance image delete is a no-op (medium) — _stub_
- **Cause**: `openstack image delete <id>` returns success (no error) but the image stays in
  `image list`. Two explicitly-deleted images (`70c6ee27…`, `32d8ea8e…`) reappeared alongside a freshly
  created one — three `cirros` images after two deletes.
- **Impact**: images can't be removed via CLI or the Horizon Images panel; the delete action appears to
  succeed but leaves the image behind.
- **Follow-up**: inspect the Glance DELETE handler (stub mode) — it likely returns 2xx without removing
  the row. (Commit `ce2cf14` previously touched "image delete idempotency"; re-verify.) Confirm whether
  it recurs in real mode (RGW-backed).
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

### F7 — Launch Instance not completable via the dashboard (major) — _stub + real; Networks step root-caused 2026-09-22_
- **Cause**: in the Launch-Instance wizard the **Networks step won't let you select a network**, so the
  required step never validates and Launch stays disabled — no instance can be launched through the UI.
  Root-caused 2026-09-22 to the duplicate network listing (**F4**) — see the Networks-step bullet below.
  An earlier "no Launch Instance button" cause recorded 2026-07-23 has **not** reproduced since; it is
  kept as history under "Wizard entry point" below and is no longer a current cause.
- **Real (2026-07-23)**: reproduces — the Networks step shows **"Available 2"** (the single `test-net`
  duplicated, per F4) but the table renders **no selectable rows**, so no network can be allocated. The
  NG wizard can't render/select Lightstack's networks. **Root-caused 2026-09-22 — see the Networks-step
  bullet below: this is F4.**
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
- **Networks step — root-caused (2026-09-22, stub): this is F4.** The empty table is a browser-side
  *symptom* of a server-side defect — the duplicate network — not a defect of its own:
  1. `ListNetworks` ignores `shared`, so Horizon's project and shared sub-queries return the same row (**F4**).
  2. `GET /dashboard/api/neutron/networks/` therefore hands the wizard **two items with an identical `id`**
     (fetched live from a logged-in session; `subnets` is correctly populated on both, so the model's
     `net.subnets.length > 0` filter in `launch-instance-model.service.js` is *not* what drops them).
  3. The step renders `hz-dynamic-table` with `trackId: 'id'` (`network.controller.js`), which expands to
     `ng-repeat-start="item in items track by item[config.trackId]"` (`hz-dynamic-table.html:61`).
  4. AngularJS **throws** `ngRepeatMinErr('dupes', …)` on a duplicate track-by key (`angular.js:34028`),
     aborting row rendering — while the "Available" counter, which reads array length, still reports 2.

  That is exactly the signature the 2026-07-23 real pass recorded: **"Available 2" with no selectable rows**.
  Confirmable in the browser console as `[ngRepeat:dupes] … Duplicate key: <network-id>`. Since `feature/532`
  removes the duplicate (see F4), the Networks step is **expected** to populate once that lands — this has
  **not** been re-tested and must be re-verified, because it does not by itself prove the rest of the wizard
  validates (the Security Groups step is separately broken — see **F10**).
- **Wizard entry point — historical, superseded** (recorded real 2026-07-23; re-checked 2026-08-25 and
  stub 2026-09-22): the original cause read *"the Instances panel has no **Launch Instance** button — the
  wizard is only reachable via Project → Compute → Images → Launch"*. It has not reproduced since: the
  2026-08-25 HTML check found the launch action present (`instances__action_launch-ng`, above), and on
  2026-09-22 the Launch-Instance **dialog did open**. Both later checks were stub / HTML-level while the
  original was a real-mode browser observation, so this is *not reproducing*, not *disproved* — either
  way it is no longer the current cause. The launch still cannot complete, for the Networks reason above.
- **Follow-up**: (1) ~~why the NG wizard's Networks step won't select a Lightstack network~~ — **answered
  above (= F4)**; re-test after `feature/532` merges; (2) why **Create** action buttons are absent on
  Project panels (quota/limits/policy) — the Launch half of this question is retired (see "Wizard entry
  point" above); the Create-Network half is **F3**.
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

### F8 — Horizon resource-*creation* UIs are broken while the APIs work (major) — _stub + real (2026-07-23)_
- **Pattern**: across panels, Lightstack's APIs work (proven by CLI — network / volume / server all create fine),
  but Horizon's **create/write UIs fail**: Project → Networks has **no Create button** (F3); Project →
  Volumes Create button **opens no dialog** (T4); the Launch-Instance wizard **can't complete** / no Launch
  button on Instances (F7); and the Overview **limits widget errors** (F2). Read paths — panels render,
  lists, server-detail — work.
- **Impact**: a developer **cannot create resources through the dashboard** — the ticket's core "resource
  management in the UI" goal fails, even though every one of those operations succeeds via the API/CLI.
- **Likely common root — confirmed in part, narrowed in part (2026-08-25)**: F2 and F3 **do** share a
  single root — Lightstack's Neutron quota-details response omits `reserved`, which makes every
  `tenant_quota_usages` call raise (see F2). That is the "visible tip" this section predicted, and one
  key fixes both. But it does **not** extend to F7 or the volume-create modal: their create actions are
  present in the server-rendered HTML (`instances__action_launch-ng`, `volumes__action_create`) and they
  gate on the Nova and Cinder quota paths, which return 200 with every key Horizon reads. Those two are
  separate failures.
- **Update (2026-09-22) — F7 is _not_ a client-side failure after all.** Its Networks step is a
  **second Lightstack response defect**: `ListNetworks` ignores the `shared` list filter, so Horizon's merged
  sub-queries hand the wizard the same network twice and AngularJS's `track by item.id` repeater throws on
  the duplicate key (full chain in F7; root is F4). So this section's thesis — response *shapes*, not write
  *paths*, are what break the UI — now covers F2, F3, F7 **and** F10, and the **volume-create modal is the
  only create-UI failure left without a root cause**.
- **Follow-up**: **two** investigations, not one. (1) Add `"reserved": 0` in `GetQuotaDetails`
  (`internal/neutron/network.go`) — closes F2 + F3. (2) Separately, why the Volumes modal never opens;
  that one is JS-level and needs a browser, not an API-level trace.
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

### F9 — Volumes panel 500s (TemplateSyntaxError) once a volume exists (major) — _real (2026-07-23)_
- **Cause**: with `test-vol-cli` present, `GET /dashboard/project/volumes/` throws `TemplateSyntaxError`
  in `horizon/tables/base.py:1948 get_rows` rendering `horizon/common/_data_table.html`. Lightstack's Cinder
  volume response is **heavily null-populated** — `openstack volume show` returns `type`/`volume_type_id`,
  `availability_zone`, `encrypted`, `os-vol-tenant-attr:tenant_id`, `user_id`, `os-vol-host-attr:host`,
  `multiattach`, `consumes_quota`, `shared_targets` all **`null`** — and Horizon's volume **table** can't
  render a row from it (a column derived from one of those nulls raises). Empty panel renders fine; a
  populated one 500s.
- **Impact**: the Volumes panel is unusable whenever any volume exists — a **read/list** break on top of
  the create-UI break (F8). Distinct from F5 (the v3 endpoint is clean in real).
- **Follow-up**: the full Django traceback is in the Horizon **container's apache error log** (pod stdout
  only shows kolla/apache startup): `kubectl -n lightstack exec deploy/horizon-lightstack-horizon -- sh -c 'tail -80 /var/log/apache2/*error*'`.
  Pinpoint the failing column (likely `encrypted` / `volume_type` / `availability_zone` being null), then
  have Lightstack's Cinder populate those fields to non-null defaults (cf. upstream Cinder's volume schema).
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

### F10 — Security-group list 500s: `security_group_rules` omitted (major) — _stub + real (2026-09-22)_
_(This report's own F-series. Unrelated to the `F10` of the closed #537, which #546 absorbed.)_
- **Cause**: `ListSecurityGroups` (`internal/neutron/ports.go:659`) returns each group **without a
  `security_group_rules` key** — only `id` / `name` / `tenant_id` / `description` / timestamps. Upstream
  Neutron always includes it (an empty list at minimum). openstacksdk turns the missing key into `None`,
  and Horizon's `SecurityGroup.__init__` (`openstack_dashboard/api/neutron.py:277`) iterates it
  unconditionally — `sg['rules'] = [SecurityGroupRule(rule, sg_dict) for rule in sg['security_group_rules']]`
  — so `GET /dashboard/api/network/securitygroups/` raises and returns **HTTP 500**:
  ```
  File "openstack_dashboard/api/neutron.py", line 277, in __init__
      sg['rules'] = [SecurityGroupRule(rule, sg_dict)
  TypeError: 'NoneType' object is not iterable
  ```
- **Evidence (2026-09-22)**: **stub** — apache error log + access log show
  `"GET /dashboard/api/network/securitygroups/ HTTP/1.1" 500`, referer `/dashboard/project/instances/`,
  with the traceback above. **Real (k3s, NodePort 30080)** — the same proxy returns **HTTP 500** from a
  logged-in session. The raw API confirms the omission in both: `GET /v2.0/security-groups` returns the
  `default` group with no `security_group_rules` member.
- **Impact**: the Launch-Instance wizard's **Security Groups step has no data**. The wizard swallows the
  failure (`securityGroup.query().then(onGetSecurityGroups, noop)` in `launch-instance-model.service.js`),
  so it does not block the dialog — which is why this stayed invisible behind F7 until the wizard was
  driven with a network present. The Instances panel issues the same call.
- **Fixed on `feature/533`** (`0981678`, not yet on `main`): `ListSecurityGroups` emits
  `security_group_rules` per group (and adds `project_id`). Not re-tested here.
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

### F11 — Domain-scoped token requests are silently downgraded to unscoped (medium) — _code-level (2026-09-22)_
- **Cause**: `ScopeField` (`internal/keystone/auth.go:55-65`) carries only a `Project` member, and its
  `UnmarshalJSON` (`:68-92`) decodes only `"project"` plus the `"unscoped"` string form. A request of the
  form `{"scope":{"domain":{...}}}` therefore unmarshals to `ScopeField{IsUnscoped:false, Project:nil}` —
  the domain is discarded during parsing. Both auth paths then fall through without an error: password
  auth (`:201`) requires `Project != nil` and so reaches `// Otherwise: unscoped token (no project, no
  roles)` (`:263`); token re-scoping (`AuthenticateToken`, `:382-385`) matches **neither** branch, leaving
  `projectID` empty. Each returns a normal success with a token carrying no project and no roles.
- **Impact**: **domain switching cannot work**, and it fails without a diagnostic. `local_settings:79`
  sets `OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True`, so Horizon offers domain context and will request
  a domain-scoped token; Lightstack answers with an unscoped one, so the domain-context panels 403 or
  render empty while the response itself reports success. Nothing tells the caller the scope was ignored.
  It does **fail closed** — the token grants less than asked, never more. Domains exist as a *resource*
  (`GET`/`POST /v3/domains`, `internal/keystone/handlers.go:115-120`), so this is a scoping gap, not a
  missing-entity gap — seeding a second domain would not help.
- **Workaround**: none in the dashboard. Use project scoping; domain-level administration is not
  available through Horizon.
- **Follow-up**: refuse the request instead of silently downgrading it. `feature/549` (SPEC-023 FR-009)
  already implements exactly this — it adds a top-level `Domain` to `ScopeField` *"parsed only so it can
  be refused"*, on the reasoning that `role_assignments` binds a user to a role within a project, so there
  is nothing a domain-scoped token could authorize. Not on `main` and not re-tested here.
- **Tracked**: [cloud-storage#549](https://github.com/cobaltcore-dev/cloud-storage/issues/549)

## Notes & scope

- RBD / block Ceph out of scope (v1); real Ceph = RGW/S3 only.
- Automated browser suite out of scope; verification is manual/runbook-driven.
- Object storage / Swift panels out of scope (no Swift API; lightstack#15).
- Stub environment note: Horizon's host port was parametrized (`HORIZON_HTTP_PORT`) to run under
  rootless Podman (`:8080`); not an Lightstack issue.

## Conclusion

Both passes are complete, and the result is unambiguous.

**What works.** Every OpenStack **API/CLI operation** exercised here succeeds (one operation-level
exception: CLI image delete, F6 — and see "What doesn't" for four response-shape deviations), and the
**real backends work end-to-end**:
sign-in and the read/list dashboard panels render, and via the API the real pass **booted a libvirt/KVM VM
from a Ceph-RGW image**, attached it to real Neutron networking with a reachable fixed IP, had
**metadata/cloud-init inject the SSH key**, honored the **default security group** (tcp/22), and served a
real **serial console** — SSH login succeeded (`uid=1000(cirros)`). Volume and server creation succeed via
CLI. This is the exact backend chain the stub pass could not exercise, and it passed.

**What doesn't.** The **Horizon dashboard is not usable for resource management.** Every create/write
workflow through the UI fails: no Create-Network button in the Project panel (F3), a dead Create-Volume
modal (F8), the Volumes list itself **500s** once a volume exists (F9), and the Launch-Instance wizard
**can't select a network** (F7). Alongside: networks list **twice** (F4), image delete is a **no-op** (F6),
the **noVNC console** is unwired (F1), the overview **limits widget errors** (F2), and Horizon's
security-group proxy **500s** (F10). The common thread
(F8): Horizon's create flows and table renderers depend on quota/limits/type metadata and fully-populated
resource fields that Lightstack under-serves — so buttons hide, modals don't open, wizard steps won't validate,
and populated tables crash, even though the underlying APIs work. Two of those are now pinned precisely
(2026-08-25): **F2 and F3 are the same one-key defect** — Lightstack's Neutron quota-details omits `reserved`, so
every `tenant_quota_usages` call raises, erroring the Overview widget and deleting the Project
Create-Network action. The dead volume modal and the Launch wizard are **not** the same defect: their
buttons render and their quota paths are healthy, so they are separate failures with their own causes. A
third was pinned on **2026-09-22**, and it turned out to be server-side after all: **F7's Networks step is
F4** — Lightstack ignores the `shared` list filter, so
Horizon's merged sub-queries hand the wizard the same network twice, and AngularJS `ngRepeat … track by
item.id` throws on the duplicate key and renders zero rows. That leaves the dead volume modal as the one
create-UI failure still without a root cause.

**Answer to #482.** Lightstack is a **drop-in for OpenStack API/CLI _operations_** — every CRUD path
exercised here works — but **not yet a faithful reproduction of upstream API _responses_**: F2/F3 (Neutron
quota-details omits `reserved`), F4 (Neutron list filters ignored — and with it F7), F9 (Cinder volume
fields null), F6 (image delete no-op) and F10 (security-group list omits `security_group_rules`) are all
API-side defects. The **Horizon dashboard is _not_ a drop-in for UI-driven
resource management** as of the SPEC-018 branch in PR lightstack#77, and those response gaps are the
reason why. This **refutes the
prior `HORIZON_FULL_COMPATIBILITY_REPORT.md` "100% compatible / 342 endpoints" claim** — the endpoint
coverage it counted is real, but it does not translate into a working dashboard for creating resources.

**Follow-ups** (separate tickets — this is verify-not-fix scope, as SPEC-016 spun off #524). **F1–F10** are tracked under [cloud-storage#531](https://github.com/cobaltcore-dev/cloud-storage/issues/531) — whose body scopes itself to F1–F9, split into #532 / #533 / #534, with F10 folded into #533 on 2026-09-22. **F11** is tracked under [cloud-storage#549](https://github.com/cobaltcore-dev/cloud-storage/issues/549), which sits **outside** the #531 umbrella:
- **F2 + F3 (highest value, and now a known one-line fix, [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533))** — add `"reserved": 0` to each resource in
  `GetQuotaDetails` (`internal/neutron/network.go`). Upstream Neutron returns `{limit, used, reserved}`
  and Horizon reads all three unconditionally; Lightstack omits `reserved`, so the Overview limits widget errors
  and the Project Create-Network action is dropped. Verified as the cause 2026-08-25; the fix itself is
  not yet re-tested.
- **F7 — answered 2026-09-22, and it is F4** (fix already on `feature/532`, `9e17dfc`; not on `main`).
  Honouring the `shared` / `router:external` filters in `ListNetworks` makes Horizon's sub-queries disjoint,
  which removes the duplicate `id` that makes AngularJS's `track by` repeater throw. **Re-test required** —
  removing the duplicate is necessary but not proven sufficient for the wizard to complete.
- **F10 — fix already on `feature/533`** (`0981678`; not on `main`): `ListSecurityGroups` must emit
  `security_group_rules`. Not re-tested.
- **Volume-create modal (still open, [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533))** — its create action renders server-side and the
  Cinder quota and limits calls return 200 with every key Horizon reads, so the quota theory does not
  explain it. Needs a browser-level (JavaScript) investigation, not an API trace.
- **F9** ([cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)) — populate Lightstack's Cinder volume response (null `type`/`availability_zone`/`encrypted`/…) so
  Horizon's volume table renders.
- **F4** ([cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)) — make Lightstack Neutron honor the `shared`/`router:external`/project list filters (no double-listing).
- **F1** ([cloud-storage#534](https://github.com/cobaltcore-dev/cloud-storage/issues/534)) — wire a real noVNC console (token store + libvirt VNC-port readback + real proxy + configurable URL).
- **F6** ([cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)) — fix Glance image delete (currently a no-op).
- **F11** ([cloud-storage#549](https://github.com/cobaltcore-dev/cloud-storage/issues/549)) — refuse domain-scoped token requests instead of silently returning an unscoped token (already implemented on `feature/549`).

**Reproducibility (SC-005 — self-reproduced 2026-09-22).** Both passes are fully scripted in the two
`HOST_TESTING*.md` runbooks (the documented procedure of FR-014); the real pass reuses the SPEC-014
`deployments/rook/setup-rook-rgw.sh` fixture. Both runbooks were **re-run end-to-end on 2026-09-22** (T033)
and every recorded verdict reproduced; the drift that run exposed is folded into this report. SC-005 is
worded for an *independent* reader, so this is self-reproduction — the strongest evidence available from
the authoring side, not formal confirmation.
