# Horizon Dashboard Compatibility Report

**Ticket**: [cobaltcore-dev/cloud-storage#482](https://github.com/cobaltcore-dev/cloud-storage/issues/482)
**Date**: stub pass 2026-07-22; real (graded) pass 2026-07-23. **Re-verified after #532 (2026-07-24) and #533
(stub + real, 2026-07-27…29)** — the #533 re-verification ran on `feature/533` with the lightstack image built from
that branch (`docker.io/library/lightstack:spec020`); same Horizon image and topology as the original real pass.
A further **stub + real re-test on 2026-09-22** ran on the `feature/482`/`feature/532` base, which carries
neither #533's fixes nor its findings: it root-caused F4 and F7's Networks step, closed project switching as
**working** (with **domain** switching graded separately, and **not working**), and filed two more
findings (**F17**, **F18**).
**Verdict (revised 2026-07-29, after #532 and #533):** **the Horizon dashboard is usable for resource
management.** Create a network with a subnet, create a volume, and launch an instance — all through the
dashboard, verified in stub **and** real mode; the launched VM reaches ACTIVE on a real libvirt domain
booting from a Ceph-RGW image, and a key pair created in Horizon lets you **`ssh`** into it. Read/list panels
render, the Overview limits widget loads, networks list once, and image delete works. Project switching, long
recorded as `partial`, is **working** — demonstrated 2026-09-22 once a second project was seeded. **All nine
original #482 findings are now closed**: F1, the noVNC console, was fixed in #534 (SPEC-021). Lightstack's
API/CLI worked for every workflow exercised, except F6 (fixed by #532), F13 (fixed by #545) and the open F10.

> **What this verdict was, and why it changed.** As originally written (2026-07-23) it read: *"the Horizon
> dashboard is **not** usable for resource management … every create/write workflow is broken … Lightstack is
> API/CLI-compatible; the Horizon UI is *not* a drop-in for resource management."* That was accurate for the
> code as it stood — 8 of 9 findings were real and reproduced. #532 fixed the read/list breaks (F4/F5/F6/F9)
> and #533 the create/write cluster (F2/F3/F7/F8). The original assessment also **refuted SPEC-002's
> ungrounded "100% compatible" claim**, and that refutation stands: the claim was never grounded in a
> browser-level check, and the nine findings are what grounding it actually cost.

**20 findings total**: F1–F9 from this pass (#482), F10–F16 added by the #533 pass, F17–F18 by the 2026-09-22
re-test, F19–F20 by the 2026-11-03 host-OS pass (`feature/host-os`). F10–F16 keep the numbers they were published under
([cloud-storage#536](https://github.com/cobaltcore-dev/cloud-storage/issues/536) and its sub-issues; #537,
where several were first filed, was closed as superseded); the 2026-09-22 re-test filed its two on the
#482 branch as `F10`/`F11`, which collide with those, so they are **renumbered F17/F18 here**.

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

> **Update — 2026-07-29 (SPEC-020 / cloud-storage#533):** findings **F2, F3, F7, F8 are FIXED** and verified in
> **both** stub and real mode (runbooks under
> [`../specs/020-horizon-create-ui-enablement/`](../specs/020-horizon-create-ui-enablement/)). **The
> create/write UIs now work**: the dashboard creates a network with a subnet, opens the Create Volume dialog
> and creates a volume, and launches an instance that reaches **ACTIVE on a real libvirt domain**, booting
> cirros from the RGW-backed image. The Overview limits widget renders. Going further than the ticket asked,
> a key pair created **in Horizon** is injected via config-drive and **`ssh cirros@<ip>` succeeds** into the
> dashboard-built VM.
>
> **This changes the report's headline verdict**: with F1 (#534, noVNC) the only remaining #482 finding, the
> dashboard *is* usable for resource management. See the revised Verdict and Conclusion.
>
> **Updated 2026-07-31**: F1 is fixed too (SPEC-021 / #534), so **no #482 finding remains open**.
>
> **The "one shared root" hypothesis in F8 was wrong.** F2/F3 were the predicted quota-metadata root; **F8's
> actual root was a Cinder route-registration bug** and **F7's were three response-shape/request-parsing
> defects plus the boot-from-volume mapping**. Each surfaced only after its predecessor was fixed, because Horizon abandons a request at the
> first failure. Details under each finding.
>
> The pass also produced **seven new findings, F10–F16**, none of them regressions — **four** pre-existing
> defects (F10, F11, F13, F15), one gap by design (F12), one config-drift bug introduced and fixed within the pass (F14), and one
> limitation of the F7 fix, since fixed (F16). See *Findings from the #533 pass*.

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
REST proxy from a logged-in session — which is what root-caused F4/F7 and surfaced F17. The real half ran
on k3s (Lightstack `hostNetwork`, Horizon NodePort 30080, Glance→Ceph RGW) and reproduced **every** finding
identically to stub; F3, F4 and F17 were each re-confirmed mechanically (Create-Network action absent from
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
| 2a | Project switching | ✅ **working** (2026-09-22) | ✅ **working** (2026-09-22) | Was `partial` in both passes only because a single seeded project left nothing to switch to — a **fixture limitation, never a Horizon or Lightstack fault**. With a second project (`verify-two` + an admin role assignment) seeded, switching **re-scopes correctly in both passes**, confirmed in the browser 2026-09-22 and corroborated at the session level: `/dashboard/auth/switch/<id>/` → `302`, the page then reports `verify-two`, and `/dashboard/api/neutron/networks/` drops from 2 items to **0** (the fixtures belong to `default`, so an empty view *is* the re-scope). Both runbooks now seed the second project, so this is reproducible. The F2 limits-error still reproduced on that base, which predates #533's fix — it is fixed on this branch, see F2. | F2 |
| 2b | Domain switching | not-exercised | **not working** (2026-09-22, code-level) | The other half of FR-003 and of workflow-matrix row 2 ("switch project/**domain** in the header"), graded **separately from 2a**. `local_settings:79` sets `OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True`, so Horizon offers domain context and requests a domain-scoped token; Lightstack discards the `domain` scope while parsing and answers with a normal-looking **unscoped** token, so domain-context panels 403 or render empty and nothing reports that the scope was dropped (**F18**). Root-caused in code rather than observed in the browser, and the parsing gap is mode-independent, so the stub pass would fail identically — not-working rather than unexercised. | F18 |
| 3 | Create network | ✅ **working** (#533) | ✅ **working** (#533) | **F3+F4 reproduced in real (2026-07-23)**: Project → Network → Networks had **no Create button** (only Admin did); an Admin-created net listed **twice** in Project — root-caused 2026-09-22 to `ListNetworks` ignoring the `shared` filter, fixed by #532 (see F4). F3's missing button root-caused 2026-08-25 to the same `reserved` gap as F2. **#533 (2026-07-29): FIXED** — Create Network present on the Project panel; `ui-net` + `ui-sub` created through the dialog and listed once, stub and real. | F3, F4 |
| 4 | Create volume | ✅ **working** (#533) | ✅ **working** (#533) | Real (2026-07-23): volumev3 catalog is **clean** (`lightstack-lightstack:8776`) and CLI `volume create` → `available` — Cinder API works and F5's stub `localhost:8776` is **not** on Horizon's v3 path. Empty panel renders; **Create Volume button opens no dialog** (F8, create-UI cluster, not the endpoint). And once a volume exists the panel **500s** (`TemplateSyntaxError` in `get_rows`) → **F9**. **#533 (2026-07-29): FIXED** — the dialog opens (root was the cinder `groups/detail` route, not metadata) and `ui-vol` reaches `available`, backed by a real qcow2 under `/var/lib/lightstack/volumes`. | F5, F8, F9 |
| 5 | Launch instance (+SSH) | ✅ **working** (#533) | ✅ **working via the UI** (#533) | Real (2026-07-23): UI wizard can't select a network (F7 — root-caused to F4's duplicate `id` plus three response-shape defects). But CLI `server create` → **ACTIVE**, `power_state Running`, real **libvirt VM booted from the RGW image**, `addresses=test-net=10.0.0.77`, `key_name`+`security_groups` present. Real compute + RGW-image boot **proven**; **`ssh cirros@10.0.0.77` succeeds** (`uid=1000` — cloud-init key injection + default-SG tcp/22 + reachable fixed IP all work). Console → T7. **#533 (2026-07-29): FIXED via the UI** — `ui-vm` launched from the wizard reaches **ACTIVE** on a real libvirt domain with an address on `spec020-net`; and with a key pair created **in Horizon**, `ssh cirros@<ip>` returns `uid=1000`. | F7 |
| 6 | Instance console (noVNC) | not-exercised | **works** (after #534) — serial log **works** | Real (2026-07-23): `console log show vm1` returns real cirros boot + EC2 metadata + `vm1 login:` (serial console **works**). noVNC Console **tab** → "Unable to load Console" (F1 confirmed — no guest). **Re-verified after #534/SPEC-021: the Console tab renders the live guest framebuffer and accepts keyboard input.** | F1 ✅ |

_Verdicts: working / partial / not-working / not-exercised (stub-only cell) / pending._

## Acceptance criteria (ticket #482)

| Criterion | Result |
|---|---|
| Login + project switching verified in real UI | **Both ✅** (real). Login verified 2026-07-23; project switching verified **2026-09-22**, once a second project (`verify-two`) was seeded — the switcher lists both and selecting the other **re-scopes the view**. The earlier `partial` was a fixture limit, not a fault. Seeding that project over the API works when `domain_id` is omitted or is a real UUID; the literal `domain_id: "default"` Horizon sends 500s on the base (**F13**) and is **fixed on this branch** (#545 / SPEC-022), where `POST /v3/projects --domain default` returns `201`, `GET /v3/auth/projects` lists both projects and a token re-scopes to the second. **Domain** switching is not part of this criterion — the ticket asks for project switching — and is graded separately as workflow **2b**, where it **fails** (F18). |
| Resource management: launch / network / volume | ✅ **All three now succeed through the UI** (#533, 2026-07-29, stub **and** real): network + subnet via the Create Network dialog; volume via the Create Volume dialog (`available`, real qcow2 on disk); instance via the Launch wizard (**ACTIVE**, real libvirt domain, booted from the RGW image). Beyond the criterion: a Horizon-created key pair yields working **`ssh`** into that VM. Originally: all three failed in the UI while succeeding via API/CLI. |
| Report: per workflow working / not working (+ gaps) | ✅ this document |
| Report published to `docs/` | ✅ |

## Findings

### F1 — noVNC console shows no guest (major) — _confirmed real 2026-07-23; re-confirmed 2026-09-22_ — ✅ **FIXED (SPEC-021 / #534, 2026-07-31)**
- **Cause**: synthetic `http://localhost:6080` console URL + a random, unpersisted token + a
  fabricated VNC port (never read from libvirt) + an unwired noVNC proxy (`internal/nova/console.go`;
  `research.md` U1).
- **Impact**: the Console tab cannot display a VM. In real it shows **"Unable to load Console" +
  "Console currently not available"** — the returned URL points at `localhost`, so the browser never
  reaches the proxy at all. The serial Console Log works (verified real: real cirros boot output +
  `vm1 login:`).
- **Re-confirmed (2026-09-22, real)**: still reproduced at the API level on the `feature/482`/`feature/532`
  base, which predates this branch — `POST /v2.1/servers/<id>/remote-consoles` (`compute 2.6`) returned
  `{"url": "http://localhost:6080/vnc_auto.html?token=<random>"}`, i.e. the synthetic `localhost` URL with an
  unpersisted token. Fixed here; see below.
- **Fix**: all four causes, plus one the ticket did not name.
  - **The relay is not written, it is configured.** The noVNC image already pinned in the verification
    stacks bundles websockify and its token plugins; a command override turns it into a real
    token-resolving proxy serving both the client page and the WebSocket relay. Verified before
    committing to the design: a good token yields `101 Switching Protocols` and `connecting to: …`, a
    bad one is closed with no response and no guest contacted. Lightstack gained **no** relay code and **no**
    new Go dependency. It ships in `charts/lightstack` as `consoleProxy`.
  - **Real console**: the guest's VNC unix socket, read from libvirt per request via
    `DomainGetXMLDesc(dom, 0)` — flags `0`, the *running* definition, because the inactive one carries no
    socket path yet. Never cached: libvirt creates a new socket on every start.
  - **Real token**: persisted in `console_auth_tokens` (migration 065) and projected into
    `/var/lib/lightstack/console-tokens/` for the proxy to read. Expiry is enforced by *deleting* that entry —
    websockify performs no expiry check — so a reconciler sweep is the mechanism, not housekeeping.
  - **Configurable URL**: `nova.console_proxy_base_url`, an **origin** only. This setting appeared in
    four operator guides for years while the binary ignored it entirely; their example values also named
    `vnc_auto.html`, a page that does not exist in noVNC 1.6.0.
  - **Not in the ticket, found while designing and in review**: the guest VNC listened on `0.0.0.0`, so the
    token was bypassable by anything that could reach the node. And a token that froze a TCP port could,
    after that port was freed and handed to the next guest, reach a **different tenant's** guest — which
    revoking on stop/start alone cannot prevent, because a guest can power itself off without any API call.
    The console is now a unix socket at the instance's own path, with no TCP listener: the proxy is the only
    route in, and a stale link can reach no other guest. Lifecycle operations still revoke, and a reconciler
    sweep revokes links whose guest stopped or restarted outside the API. `sharePolicy='force-shared'` additionally stops a
    second viewer evicting the first.
  - **A second, independent blocker, found only by fixing the first.** Lightstack's own Horizon config set
    `CONSOLE_TYPE = 'novnc'`. Horizon dispatches on that through `CONSOLES` in
    `project/instances/console.py`, whose keys are `AUTO`/`VNC`/`SPICE`/`RDP`/`SERIAL`/`MKS` — so
    `CONSOLES['novnc']` raised `KeyError`, became `NotAvailable`, and the tab reported *"Console is
    currently unavailable"* **without ever calling Nova**. With the backend broken too, the two failures
    were indistinguishable; repairing the backend simply moved the error up a layer. Now `'VNC'`.
    The same block also carried `NOVNC_PROXY_URL` and `CONSOLE_TYPES`, which **no upstream Horizon code
    reads** — invented settings that looked authoritative, with `NOVNC_PROXY_URL` still pointing at
    `localhost:6080`. Both removed.
- **Honest limits**: effective token validity is `TTL + reconcile interval` (10m30s at the defaults),
  not exactly 10m. Multi-node console is out of scope for v1. SPICE, RDP and interactive serial consoles
  are refused with a clear error rather than a fabricated URL.
- **Workaround (no longer needed, still supported)**: `openstack console log show <server>`.
- **Tracked**: [cloud-storage#534](https://github.com/cobaltcore-dev/cloud-storage/issues/534)

### F2 — Project Overview: "Unable to retrieve limits information" (medium) — _stub + real_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**
- **Cause**: the Overview usage/limits widget errors on load / after project switch.
- **Root (confirmed)**: the widget's **Neutron** quota-details call is the one that raised. Upstream Neutron
  returns `{limit, used, reserved}` per resource (`neutron/db/quota/driver.py`,
  `get_detailed_project_quotas`) and Horizon reads `detail['used'] + detail['reserved']` **unconditionally**
  (`openstack_dashboard/usage/quotas.py`, `_get_tenant_network_usages`), but `GetQuotaDetails`
  (`internal/neutron/network.go`) returned only `{limit, used}` for all seven resources → `KeyError:
  'reserved'`. Horizon wraps the Nova and Cinder limit calls each in its own `try/except`; the Neutron call
  has **none**, so the error reached the bare `except Exception` in `openstack_dashboard/usage/views.py` and
  rendered as the service-less **"Unable to retrieve limits information. Details 'reserved'"**. This
  supersedes the earlier "Nova limits/quota" attribution — `GET /v2.1/limits` returned 200 with all 13 keys
  Horizon reads, and Cinder's `GET /v3/{project}/limits` with all 6. Behind that one key the read surface was
  also incomplete and self-inconsistent: Nova's absolute-limits view carried its **own** default map with
  instances/cores/RAM at `100/200/512000` against a quota-set default of `10/20/51200` — ten times apart;
  Nova had no `os-quota-sets/{id}/detail`; Neutron had no `quotas/{id}/default` and no `.json`-suffixed
  details; Cinder served no usage view (its `?usage=true` parameter was ignored) and no `/defaults`, and its
  limits view ignored the `cinder_quotas` table entirely, so an admin's override had no effect on it.
- **Fix**: `"reserved": 0` on every resource in `GetQuotaDetails` — Lightstack holds no reservations, so a
  constant `0` is correct and it covers all three keys `_get_tenant_network_usages` reads — plus one
  limits helper per service as the single source of defaults (`resolveQuotaLimits`, `resolveCinderQuotaLimits`,
  `neutronQuotaLimits`) — Nova's and Cinder's overlaid with their quota tables, Neutron's code defaults only — so
  the limits and quota-set views cannot disagree; the missing `detail`/`default` variants added; Neutron
  advertises `quota_details`. Nova's inflated map was **latent** — migration 007's seed masked it for the
  seeded project, so only unseeded projects ever saw `100/200/512000`.
- **Verified**: stub + real — the widget renders Compute, Volume and Network groups. The Network group
  appearing shows that `GET /v2.0/quotas/{id}/details` now carries every key Horizon reads: Horizon 2025.2
  calls it without checking for `quota_details` and reads its `reserved` key. Advertising `quota_details` is
  for API fidelity only.
- **Still open**: reload **after a project switch** was never re-run with this fix in place. A second project
  can be seeded (see the acceptance table and F13, itself fixed here), so only the re-run is outstanding.
- **Evidence for the `reserved` root (2026-08-25)** — from a real-mode re-pass on the #482 branch, which does
  not carry this fix: the rendered Overview HTML carried
  `Error: Unable to retrieve limits information. Details 'reserved'`, and the Horizon container's apache error
  log logged `horizon.exceptions WARNING  Recoverable error: 'reserved'` on every Overview load.
  `_get_tenant_network_usages` is the only site in `usage/quotas.py` that reads `'reserved'` from a response
  dict, which is what rules out the Nova and Cinder paths.

### F3 — No "Create Network" in the Project panel (medium) — _stub + real_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**
- **Cause**: Project → Network → Networks lacks the Create Network button (only Admin → Network → Networks
  has it). Horizon hides it when the expected capability/policy/quota isn't advertised by Neutron.
- **Root (confirmed)**: **quota, not policy — and the same missing `reserved` key as F2.** Horizon's
  `CreateNetwork.allowed()` (`openstack_dashboard/dashboards/project/networks/tables.py`) calls
  `quotas.tenant_quota_usages(request, targets=('network',))` — the Neutron quota path — before rendering the
  action; that call raised `KeyError: 'reserved'` (see F2), so the action was never added and the table
  shipped without it. Admin → Network → Networks uses a table that does **not** gate on tenant quota, which is
  precisely why the button survived there. Note the action's own logic would have kept the button *enabled* on
  a mere quota miss (`allowed()` returns `True` unconditionally and defaults `available` to 1) — it
  disappeared only because the lookup **raised**. Upstream Neutron's default `create_network` rule is
  `rule:admin_or_project_member`, so a stock Horizon already permits a project member; grounding that against
  upstream policy before the fix is why no policy change was needed.
- **Fix**: the same quota surface as F2, plus `project_id` alongside `tenant_id` on the network object.
- **Verified**: stub + real — the button is present on the *Project* panel and the dialog creates a network
  with a subnet, listed exactly once (the #532 F4 dedup still holding).
- **Evidence for the `reserved` root (2026-08-25, same re-pass as F2)**: `/dashboard/project/networks/`
  rendered only `networks__action_filter_project_networks`, with the string "Create Network" absent, while
  `/dashboard/admin/networks/` rendered `networks__action_create` with the string present. The `'reserved'`
  KeyError was logged on **every** project panel load (networks / volumes / instances one each, Overview two),
  yet only Networks lost its action — Volumes and Instances gate on the Cinder and Nova quota paths, which
  both returned 200.

### F4 — Duplicate network listing in the Project panel (medium) — _stub + real; root-caused 2026-09-22_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
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
- **Fixed by #532** (SPEC-019, 2026-07-24; merged to `main` as `0a4dde0`): `buildNetworkListFilter` honours
  `shared` / `router:external` / `tenant_id` / `id` / `name`, making Horizon's sub-queries disjoint, and
  short-circuits `router:external=true` to an empty list. This report's passes ran on `feature/482`, which
  is the base of that chain and therefore predates the fix. The fix itself was re-verified in **both**
  stub and real mode — see [`../specs/019-horizon-compat-api-fixes/`](../specs/019-horizon-compat-api-fixes/).
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

### F5 — Volume (Cinder) catalog endpoint advertises `localhost` (major) — _stub_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
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
- **Follow-up (as filed 2026-07-23 — resolved 2026-07-24, see the update above and the Follow-ups below)**: seed the alias endpoint (`…015`) with the configurable host (not `localhost`), and/or
  make catalog host-substitution rewrite `localhost:` too. Verify against #481 Terraform (volume path).
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

### F6 — Glance image delete is a no-op (medium) — _stub_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
- **Cause**: `openstack image delete <id>` returns success (no error) but the image stays in
  `image list`. Two explicitly-deleted images (`70c6ee27…`, `32d8ea8e…`) reappeared alongside a freshly
  created one — three `cirros` images after two deletes.
- **Impact**: images can't be removed via CLI or the Horizon Images panel; the delete action appears to
  succeed but leaves the image behind.
- **Follow-up (as filed 2026-07-23 — resolved 2026-07-24, see the update above and the Follow-ups below)**: inspect the Glance DELETE handler (stub mode) — it likely returns 2xx without removing
  the row. (Commit `ce2cf14` previously touched "image delete idempotency"; re-verify.) Confirm whether
  it recurs in real mode (RGW-backed).
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

### F7 — Launch Instance not completable via the dashboard (major) — _stub + real (2026-07-23); Networks step also root-caused 2026-09-22_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**

> **Root: not one cause but four, and none of them quota.** Whatever first hid the Launch button no longer
> reproduces (see *Wizard entry point* below); the wizard itself failed for a chain of unrelated reasons, each
> invisible until the previous one was fixed — Horizon abandons a request at its first failure:
>
> 1. **Networks step empty** — the wizard expands a network's `subnets` **ids** against a separately-scoped
>    subnet list; `networkSubnetIDs` (no project predicate, unpaginated) and `ListSubnets` (project-scoped,
>    paginated) disagreed, so a referenced id could be unresolvable and render a null row. `ListSubnets` is
>    now scoped by the owning **network's** visibility. A unit test (`TestSubnetReferenceIntegrity`) caught
>    this before the browser did. The 2026-09-22 re-test, run on the base where F4 was still present, pinned
>    the *same* empty table to a second server-side cause — the duplicate network `id` makes AngularJS's
>    `track by` repeater throw. Both are real; this branch carries both fixes. Chain in the Networks-step
>    bullet below.
> 2. **Security-group step empty, 500** — `GET /v2.0/security-groups` omitted `security_group_rules`.
>    openstacksdk's `to_dict()` emits that key unconditionally, so "absent" arrives as **null**, and Horizon's
>    guard (`if 'security_group_rules' not in sg`) does not catch null → `'NoneType' object is not iterable`.
> 3. **Then `500 "'protocol'"`** — with the array present, Horizon read each rule's fields as `sgr['protocol']`
>    unconditionally; Lightstack emitted those keys only when non-NULL, and the seeded default group's egress rules
>    have a NULL protocol. **A null value is fine; an absent key is not.**
> 4. **Then `400 Security group <uuid> not found`** — once the SG list worked, the wizard started *sending*
>    security groups, as **ids** (`setFinalSpecSecurityGroups` pushes `securityGroup.id` when Neutron is
>    enabled). Lightstack resolved that field by name only. Upstream Nova with Neutron matches **name or UUID**.
>
> A fifth defect sat behind the Launch button itself: Horizon defaults **Create New Volume = Yes**, which
> sends the image inside `block_device_mapping_v2` (`source_type: image`) with an empty `imageRef`. Lightstack
> ignored that entry and errored the instance to **ERROR** with `invalid UUID length: 0`. It now creates the
> volume that entry asks for and boots from it — see **F16**.

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
- **Networks step — also root-caused (2026-09-22, stub) to F4.** Run on the base, where F4 was still
  present, the same empty table has a second, independent cause — the duplicate network — and it is
  server-side, not a browser defect of its own:
  1. `ListNetworks` ignores `shared`, so Horizon's project and shared sub-queries return the same row (**F4**).
  2. `GET /dashboard/api/neutron/networks/` therefore hands the wizard **two items with an identical `id`**
     (fetched live from a logged-in session; `subnets` is correctly populated on both, so the model's
     `net.subnets.length > 0` filter in `launch-instance-model.service.js` is *not* what drops them).
  3. The step renders `hz-dynamic-table` with `trackId: 'id'` (`network.controller.js`), which expands to
     `ng-repeat-start="item in items track by item[config.trackId]"` (`hz-dynamic-table.html:61`).
  4. AngularJS **throws** `ngRepeatMinErr('dupes', …)` on a duplicate track-by key (`angular.js:34028`),
     aborting row rendering — while the "Available" counter, which reads array length, still reports 2.

  That is exactly the signature the 2026-07-23 real pass recorded: **"Available 2" with no selectable rows**.
  Confirmable in the browser console as `[ngRepeat:dupes] … Duplicate key: <network-id>`. This branch carries
  **both** fixes — #532's filter fix removes the duplicate, #533's subnet scoping fixes root 1 — and the wizard
  completes, so neither is left resting on prediction.
- **Wizard entry point — historical, superseded** (recorded real 2026-07-23; re-checked 2026-08-25 and
  stub 2026-09-22): the original cause read *"the Instances panel has no **Launch Instance** button — the
  wizard is only reachable via Project → Compute → Images → Launch"*. It has not reproduced since: the
  2026-08-25 HTML check found the launch action present (`instances__action_launch-ng`, above), and on
  2026-09-22 the Launch-Instance **dialog did open** on the base too. Both later checks were stub /
  HTML-level while the original was a real-mode browser observation, so this is *not reproducing*, not
  *disproved* — either way it is no longer the current cause.
- **Follow-up**: (1) ~~why the NG wizard's Networks step won't select a Lightstack network~~ — **answered**:
  the duplicate `id` (F4) and the subnet-scoping mismatch (root 1); (2) ~~why Launch/Create action buttons are
  absent on Project panels~~ — **answered**: the quota `reserved` gap (F2/F3), the Launch half having been
  retired first (see *Wizard entry point* above).
- **Verified fixed**: stub + real. Real pass 2026-07-29 — `ui-vm` **ACTIVE**, address on `spec020-net`, a real
  libvirt domain (`instance-<hex>`), and the serial console shows a genuine cirros boot from the RGW-backed
  image. Both follow-up questions above are answered by the roots in the box.
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

### F8 — Horizon resource-*creation* UIs are broken while the APIs work (major) — _stub + real (2026-07-23)_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**

> **Root: a Cinder route-registration bug — the "shared metadata root" hypothesis below was wrong for this
> finding.** The Create Volume button appeared but did nothing, with **no traceback in Horizon and no error in the browser**.
> Capturing the modal fetch itself was the only way to see it:
>
> ```
> GET /dashboard/project/volumes/create/  →  200, Content-Length: 0
> X-Horizon-Location: /dashboard/project/volumes/
> Set-Cookie: messages=… "Unable to retrieve the volume group list.…failed to get group (HTTP 500)"
> ```
>
> `CreateForm._populate_group_choices` calls `cinder.group_list`, which python-cinderclient issues as
> `GET /v3/groups/detail`. Lightstack registered only `groups/:id`, so gin matched `"detail"` as a group id and
> `GetGroup` answered **500**. Horizon converts any `group_list` error into a redirect, so the modal returned
> an empty 200 with `X-Horizon-Location` and the browser navigated instead of opening a dialog — a button that
> looks inert. `groups/detail` is now registered (beside `groups/:id`; the order is irrelevant, gin prefers the
> static segment), and the group routes also exist under
> `/v3/{project_id}`.
>
> **A wrong diagnosis is recorded here deliberately.** The first investigation concluded the cause was
> oslo.policy scope enforcement, from an `openstack_auth` warning and a `volume:create` DEBUG line. Checking
> `openstack_auth` at `stable/2025.2` disproved all three links: `domain_id` is set in the policy credentials
> **unconditionally**; the "domain scoped token" warning comes from a path `check()` only consults for
> `scope == 'identity'`; and the `enforce:` DEBUG line is logged on every evaluation, pass or fail. The lesson
> is procedural — the failure was invisible in the browser and in Horizon's logs; it showed in the HTTP response
> between them, and in Lightstack's log as a `500` on `GET /v3/groups/detail`.

- **Note**: Lightstack's `/v3/extensions` (absent, 404) was also added during this work. It was a genuine missing
  endpoint that Horizon's create-volume form probes, but it was **not** the blocker.
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
  *paths*, are what break the UI — now covers F2, F3, F7 **and** F17, and the **volume-create modal was then the
  only create-UI failure left without a root cause** — since found, see the box at the top of this finding.
- **Follow-up**: **two** investigations, not one. (1) Add `"reserved": 0` in `GetQuotaDetails`
  (`internal/neutron/network.go`) — closes F2 + F3. (2) ~~why the Volumes modal never opens; that one is
  JS-level and needs a browser~~ — **answered**: an API-level `500` on `GET /v3/groups/detail` (see the box above).
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

### F9 — Volumes panel 500s (TemplateSyntaxError) once a volume exists (major) — _real (2026-07-23)_ — ✅ **FIXED (SPEC-019 / #532, 2026-07-24)**
- **Cause**: with `test-vol-cli` present, `GET /dashboard/project/volumes/` throws `TemplateSyntaxError`
  in `horizon/tables/base.py:1948 get_rows` rendering `horizon/common/_data_table.html`. Lightstack's Cinder
  volume response is **heavily null-populated** — `openstack volume show` returns `type`/`volume_type_id`,
  `availability_zone`, `encrypted`, `os-vol-tenant-attr:tenant_id`, `user_id`, `os-vol-host-attr:host`,
  `multiattach`, `consumes_quota`, `shared_targets` all **`null`** — and Horizon's volume **table** can't
  render a row from it (a column derived from one of those nulls raises). Empty panel renders fine; a
  populated one 500s.
- **Impact**: the Volumes panel is unusable whenever any volume exists — a **read/list** break on top of
  the create-UI break (F8). Distinct from F5 (the v3 endpoint is clean in real).
- **Follow-up (as filed 2026-07-23 — resolved 2026-07-24, see the update above and the Follow-ups below)**: the full Django traceback is in the Horizon **container's apache error log** (pod stdout
  only shows kolla/apache startup): `kubectl -n lightstack exec deploy/horizon-lightstack-horizon -- sh -c 'tail -80 /var/log/apache2/*error*'`.
  Pinpoint the failing column (likely `encrypted` / `volume_type` / `availability_zone` being null), then
  have Lightstack's Cinder populate those fields to non-null defaults (cf. upstream Cinder's volume schema).
- **Tracked**: [cloud-storage#532](https://github.com/cobaltcore-dev/cloud-storage/issues/532)

## Findings from the #533 pass (F10–F16)

Surfaced while fixing F2/F3/F7/F8, in stub and real mode. **None is a regression from #532 or #533** —
**F10–F13 and F15** are pre-existing and reproduce on the base commit; **F14** was introduced *and* fixed
inside the pass (on `0a4dde0` both `local_settings` copies carried `CSRF_COOKIE_HTTPONLY = True`, so there
was no drift to inherit); **F16** was a limitation of the F7 fix and is fixed.

### F10 — `POST /v2.0/ports` 500s: bridge in the root namespace, TAP attached in a project namespace (major) — _real_ — ❌ **open**
- **Cause**: `CreateNetwork` creates the Linux bridge in the **root** netns, deliberately, "for libvirt access"
  (`internal/neutron/network.go:489`). `CreatePort` then attaches the TAP **inside** `light-ns-<project>`
  (`ports.go:98`) — a namespace that is never created. Result:
  `failed to attach tap-… to bridge br-… in namespace light-ns-…: Device does not exist`.
- **Confirmed on the host**: the `br-*` bridges are present in the root namespace and `ip netns list` shows only
  k3s's own `cni-*` entries — no `light-ns-*`.
- **Why the dashboard still works**: Nova builds the domain's interface itself and never calls `CreatePort`,
  which is why an instance reaches ACTIVE with an address while explicit `port create` fails.
- **Also breaks**: `test/contract/neutron`'s `TestPortTopologyData_Contract`; and it is the likely root of the
  guest's `169.254.169.254 request failed` / `no interfaces have a carrier` console lines.

### F11 — Horizon cannot attach a router gateway: Lightstack models no external networks (medium) — _real_ — ❌ **open**
- **Cause**: `router:external` is hard-coded `false` on every network object
  (`internal/neutron/network.go:527,709,814`) and `GET /v2.0/networks?router:external=true` deliberately returns
  `[]` (`networkListDecision`, `network.go:563`). Horizon's router **Set Gateway** dialog populates its dropdown from exactly that
  filter, so it is always empty.
- **Impact**: north-south egress for a guest cannot be configured from the dashboard. The CLI path works —
  `UpdateRouter` accepts `external_gateway_info` directly (`router.go:68`) — so this is a UI-surface gap, not a
  missing capability.

### F12 — No Horizon UI for EC2 credentials (low) — _real_ — ❌ **open by design**
- Lightstack implements the EC2-credentials API and `/v3/s3tokens` (`internal/keystone/handlers.go:48,78-81`), but
  Horizon exposes no panel for OS-EC2, so tenant S3 credentials are CLI-only. Upstream Horizon is the same;
  recorded so the tenant-object-storage workflow isn't mistaken for a defect.

### F13 — `POST /v3/projects` returns 500 (major) — _stub + real_ — ✅ **FIXED (SPEC-022 / #545, 2026-08-07)**
- **Impact**: a project could not be created the way *Horizon* creates one — the Identity panel's own
  create-project path. It was read at first as the reason project switching had never been demonstrated; the
  2026-09-22 re-test disproved that (a second project seeded with `domain_id` omitted, or with
  `--domain Default`, which resolves the name to a UUID), narrowing the 500 to a **non-UUID `domain_id`**.
- **Cause** (the guess in the original entry was right): `CreateProject`
  (`internal/keystone/handlers.go:448`) defaulted only the *empty* `domain_id`, then passed any supplied
  string straight into a `uuid` column. `domain_id: "default"` — what every OpenStack client sends, because
  upstream Keystone's Default domain literally carries `default` as its id — failed at the database and
  surfaced as a generic `computeFault`. `POST /v3/users` rejected the same value cleanly with a `400`, so the
  reference implementation was already in the same file.
- **Fix**: one shared resolver, used by project, user and group creation *and* by domain retrieval, resolving
  the Default domain **by name** rather than by a hard-coded id (migrations 009 and 037 seed that name with
  different ids). An unknown or disabled domain now answers `400` naming the field; a duplicate project name
  answers `409` instead of a `500`. `GET /v3/domains/default` resolves, and `GET /v3/domains?name=` filters.
- **Verified — API (2026-08-07, stub stack built from `feature/545`)**: `POST /v3/projects --domain default`
  → **`201`**; `GET /v3/auth/projects` — the call that populates Horizon's project switcher — returns **both**
  projects; re-scoping a token to the second returns **`201`** with `token.project.name` set to it.
- **UI — blocked by F19 until 2026-11-03**: creating a project *through the Identity panel* could not be
  exercised before then: the Create Project modal never opened, for a reason of our own making (`_member_` as
  Horizon's default role, F19), not a Lightstack API fault. Re-verification is listed under F19. Nothing else waits on it — project switching was demonstrated in the browser on
  2026-09-22 (acceptance table), and F2's residual (Overview widget across a switch) is a pending re-run
  rather than a blocked check.

### F14 — `local_settings` drift between the compose config and the Helm chart (medium) — _real_ — ✅ **FIXED in-pass**
- **Cause**: `charts/horizon/files/local_settings` is **generated** from
  `deployments/horizon-config/local_settings` by `make horizon-chart-sync`, and **real mode reads the chart
  copy**. A hand-edit to the compose copy left the two disagreeing. (The chart copy sat at
  `deployments/horizon-chart/files/local_settings` when this was found; #534 relocated it.)
- **Symptom**: with the chart still carrying `CSRF_COOKIE_HTTPONLY = True`, Project → Compute → Key Pairs →
  **Delete Key Pair** silently did nothing. Django logged `Forbidden (CSRF token missing.)`; Lightstack never saw the
  request. Access-log tell: a **`200` with a non-empty body** on a `DELETE` where success is `204` empty.
- **The CSRF effect is selective, which made it confusing**: with `True` still in place, `POST /api/nova/keypairs/`
  returned **201** (import *and* generate) and the Angular launch wizard **succeeded** — only the delete failed.
  Some Angular paths obtain the token other than from `document.cookie`. Do not infer a control works because a
  neighbouring one does.
- **Fix**: `make horizon-chart-sync`; both copies now `False`. Upstream Horizon does not set the flag and Django
  documents it as offering "no practical protection".

### F15 — `test/contract/nova` is stub-only and unusable as a real-mode gate (medium) — _real_ — ❌ **open**
- The gophercloud suite assumes server create yields an instantly-`ACTIVE` instance. Under real libvirt its wait
  loops expire, actions answer `409 instance must be active` instead of `202`/`400`, and the accumulated 30s
  waits exceed the suite's own `-timeout 5m` (panic in `TestRemoveSecurityGroup_Contract`). Green in stub mode
  the same day. `make test-contract` is therefore excluded from the real-mode runbook.

### F16 — Boot-from-new-volume was adopted, not implemented (low) — _stub + real_ — ✅ **fixed**
- Horizon's launch wizard defaults **Create New Volume = Yes** and sends the image via
  `block_device_mapping_v2` (`source_type: image`, `destination_type: volume`) with an empty `imageRef`. The F7
  fix **adopted that entry as the boot image** so the wizard's default path reached ACTIVE, but it did **not**
  materialize a Cinder volume: `volume_size` and `delete_on_termination` on that entry were ignored and no
  volume appeared under Project → Volumes.
- **Fixed (SPEC-020 FR-025):** the entry now creates a volume from the image, sized by `volume_size` (or the
  flavor's disk) and never below the image's floor, and the server boots from it. `delete_on_termination`
  decides whether the volume goes with the server. A request that cannot get its volume is refused (`400`, or
  `503` when the volume cannot be created) and never falls back to an image boot. A node-local volume made
  from an RGW-backed image first fetches the image's base onto the node, which also fixed `volume create
  --image` for such images.

### Cosmetic observations (not findings)
- The seeded Cinder volume type is named **`ceph-rbd`** ("Ceph RBD backed volumes") even when
  `cinder.storageMode: local`. Misleading in the UI — it sent one investigation down a Ceph path before the
  qcow2 under `/var/lib/lightstack/volumes` disproved it. `GET /v3/types/default` correctly reports `__DEFAULT__`.
- `POST /v2/images` stored `disk_format: raw` for an image created with `--disk-format qcow2`. Noticed in
  passing, not investigated.

## Findings from the 2026-09-22 re-test (F17–F18)

Filed on the `feature/482`/`feature/532` base — which carries neither #533's fixes nor its findings — as
`F10`/`F11`, and renumbered here: the #533 pass had already published F10–F16 under
[cloud-storage#536](https://github.com/cobaltcore-dev/cloud-storage/issues/536) /
[#537](https://github.com/cobaltcore-dev/cloud-storage/issues/537).

### F17 — Security-group list 500s: `security_group_rules` omitted (major) — _stub + real (2026-09-22)_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**
_Found independently on a base that predates the fix. It is the same defect as **F7's root 2**, which is why
it carries a fix here without a separate change._
- **Cause (pre-fix)**: `ListSecurityGroups` (`internal/neutron/ports.go`) returned each group **without a**
  `security_group_rules` key — only `id` / `name` / `tenant_id` / `description` / timestamps. Upstream
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
- **Fix**: `ListSecurityGroups` emits `security_group_rules` per group (and adds `project_id`) — on this
  branch (`internal/neutron/ports.go`), verified stub + real by the #533 pass as root 2 of F7. The
  2026-09-22 re-test itself ran on the base and could not observe the fix.
- **Tracked**: [cloud-storage#533](https://github.com/cobaltcore-dev/cloud-storage/issues/533)

### F18 — Domain-scoped token requests are silently downgraded to unscoped (medium) — _code-level (2026-09-22)_
- **Cause**: `ScopeField` (`internal/keystone/auth.go:103-113`) carries only a `Project` member, and its
  `UnmarshalJSON` (`:116-141`) decodes only `"project"` plus the `"unscoped"` string form. A request of the
  form `{"scope":{"domain":{...}}}` therefore unmarshals to `ScopeField{IsUnscoped:false, Project:nil}` —
  the domain is discarded during parsing. Both auth paths then fall through without an error: password
  auth (`:249`) requires `Project != nil` and so reaches `// Otherwise: unscoped token (no project, no
  roles)` (`:319`); token re-scoping (`AuthenticateToken`, `:448-451`) matches **neither** branch, leaving
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

## Findings from the 2026-11-03 host-OS pass (F19–F20)

Found while walking the Horizon UI lifecycle (`docs/HOST_TESTING_OPENSUSE.md`, C7b) on k3s, real mode.
F19 is a defect in Lightstack's own dashboard configuration; F20 is the API gap it had been hiding.

### F19 — Horizon's default role (`_member_`) names a role Lightstack never seeds (major) — _config-level, mode-independent (found real, 2026-11-03)_ — ✅ **FIXED in-pass (config)**
- **Cause**: both `local_settings` copies (`deployments/horizon-config/local_settings:65` and its generated
  chart copy) set `OPENSTACK_KEYSTONE_DEFAULT_ROLE = "_member_"`, since `16cfb26` (2026-03-13). Lightstack seeds
  `admin`/`member`/`reader` (`migrations/002_seed_data.up.sql:20-24`, a week older) and no migration has ever
  seeded `_member_`, the legacy member role older Keystone releases created. Horizon resolves the setting
  against an unfiltered `GET /v3/roles`, by id or name, once per process (`openstack_dashboard/api/keystone.py`,
  `get_default_role`), and caches the miss. Upstream Horizon's own default is `member`
  (`openstack_dashboard/defaults.py`); so was the first Horizon config in this repo (`7b15418`, 2026-03-07) and
  so are the five other copies in the tree. The value was wrong from the day it was written, and no runbook had
  driven Create Project, Manage Members or Create User through the browser (018 scoped the Identity panels out).
- **Symptom 1 — the modal never opens**: Identity → Projects → **Create Project** (and **Manage Members** /
  **Manage Groups** on an existing project) bounce silently: the button clicks, the page re-renders, no modal.
  The members step's `__init__` raises `Could not find default role "_member_" in Keystone`, which Horizon folds
  into "Unable to retrieve user list" and a redirect to the list (`identity/projects/workflows.py`,
  `UpdateProjectMembersAction`). Access-log tell: `GET /dashboard/identity/create` → **`200`** with
  `X-Horizon-Location: /dashboard/identity/` and a `messages` `Set-Cookie`; decode that cookie (strip the
  leading `.`, urlsafe-base64, zlib) and the text is in it. This is why F13's UI path stayed unverified: the
  request never reached Lightstack.
- **Symptom 2 — silent, and the worse one**: Identity → Users → **Create User** preselects its Role from the
  same setting (`identity/users/views.py`, `CreateView.get_initial`: `role_id = getattr(default_role, "id",
  None)`) and sorts the choices by **id**. With nothing resolved, the dropdown landed on `admin` (`…0003`, the
  lowest seeded id), so a user created through the dashboard with a primary project was granted **admin**
  unless the operator changed the field. Nothing reported it. Derived from the 2025.2 source and the seeded
  ids; not reproduced in a browser.
- **Fix**: `member` in `deployments/horizon-config/local_settings`, then `make horizon-chart-sync` (both
  copies; `make horizon-chart-check` and `helm unittest charts/horizon` green). A SPEC-020 **FR-018a**
  correction — a misconfiguration in Lightstack's own deployment artifacts, justified against upstream,
  compensating for no missing API — the second after F14's `CSRF_COOKIE_HTTPONLY`. `docs/HORIZON_DEPLOYMENT.md`
  corrected with it; the RGW fixtures' accepted-roles lists dropped the dead `_member_`
  (`docs/OBJECT_STORAGE_S3.md` never had it). Horizon caches the resolved role per process, so the pod must
  restart: the chart's `checksum/config` annotation rolls it on `helm upgrade`; the compose stack needs its
  `horizon` container recreated.
- **Status**: fix applied and synced 2026-11-03. Browser re-verification is the next C7b run: (1) Create
  Project opens and creates the project; (2) Manage Members → add `admin` as `member` → Save; (3) Create User
  shows `member` preselected. (2) arms F20 the moment a Keystone group exists.

### F20 — Group role assignments on a project are unrouted (low) — _code-level (2026-11-03)_ — ❌ **open**
- **Cause**: `internal/keystone/handlers.go` routes group CRUD and `/groups/:id/users/:user_id` only. There is
  no `PUT`/`GET`/`DELETE /v3/projects/{p}/groups/{g}/roles[/{r}]`, and `GET /v3/role_assignments` emits `user`
  entries only. SPEC-023 (`spec.md:290`) puts group-based role assignments out of scope, so this is a documented
  non-feature rather than a regression.
- **Impact**: masked until F19, because the modal never opened. Horizon's project workflows run a groups step.
  Create Project calls `add_group_role` only for groups the operator ticks; **Manage Members → Save** on an
  existing project calls `group_list(…, project=…)`, which runs `roles_for_group` →
  `GET /v3/projects/{p}/groups/{g}/roles` for **every group in the domain** → `404` → "Failed to modify project
  groups", *after* the member changes were already applied. Nothing seeds a group, so a stock install never
  hits it; the first `POST /v3/groups` arms it. Derived from the 2025.2 source and the route table, not
  exercised.
- **Workaround**: grant roles to users, not groups; do not create Keystone groups on a deployment managed
  through Horizon.
- **Tracked**: no ticket yet.

## Notes & scope

- RBD / block Ceph out of scope (v1); real Ceph = RGW/S3 only.
- Automated browser suite out of scope; verification is manual/runbook-driven.
- Object storage / Swift panels out of scope (no Swift API; lightstack#15).
- Stub environment note: Horizon's host port was parametrized (`HORIZON_HTTP_PORT`) to run under
  rootless Podman (`:8080`); not an Lightstack issue.

## Conclusion

Both passes are complete, and the result is unambiguous.

**What works.** Every OpenStack **API/CLI operation** exercised here succeeds (operation-level
exceptions: CLI image delete, F6, fixed by #532; project create, F13, fixed by #545; and the open F10 — and see the "Superseded"
paragraph below for four response-shape deviations), and the
**real backends work end-to-end**:
sign-in and the read/list dashboard panels render, and via the API the real pass **booted a libvirt/KVM VM
from a Ceph-RGW image**, attached it to real Neutron networking with a reachable fixed IP, had
**metadata/cloud-init inject the SSH key**, honored the **default security group** (tcp/22), and served a
real **serial console** — SSH login succeeded (`uid=1000(cirros)`). Volume and server creation succeed via
CLI. This is the exact backend chain the stub pass could not exercise, and it passed.

**What doesn't (as of 2026-07-31).** Of the #482 findings, **none** — F1, the noVNC console, was the last one
open and is fixed by #534 (SPEC-021): the Console tab renders the live guest. Outside that set, the #533
pass's own **F10–F12 and F15** stay open, as does **F18** (domain-scoped tokens, #549) — **F13** is fixed
here by #545; **F17** — Horizon's security-group proxy 500-ing because the list omits
`security_group_rules` — is fixed here, as root 2 of F7. The 2026-11-03 host-OS pass added **F19** — the
dashboard's default role named a role that never existed, a configuration defect of our own, fixed in-pass —
and **F20** (group role assignments unrouted, open, low).

**Superseded — what didn't work at the time of writing (2026-07-23).** Every create/write workflow through the
UI failed: no Create-Network button (F3), a dead Create-Volume modal (F8), the Volumes list **500**-ing once a
volume existed (F9), the Launch wizard unable to select a network (F7); plus networks listing **twice** (F4),
the volume endpoint advertising `localhost` (F5), image delete a **no-op** (F6), the overview **limits widget** erroring (F2), and Horizon's security-group proxy
**500**-ing (F17 — filed later, present all along). #532 and #533 fixed all nine.

**On the "common thread" this report proposed.** F8 argued the create UIs shared one root — quota/limits/type
metadata Horizon reads up front. That held for **F2 and F3 only**, and for those two it held exactly: a
2026-08-25 re-pass pinned them to a **single missing key** — Lightstack's Neutron quota-details omitted
`reserved`, so every `tenant_quota_usages` call raised, erroring the Overview widget and deleting the Project
Create-Network action. **F8's real root was a Cinder route-registration bug** (`groups/detail` was never registered,
so `groups/:id` matched `detail` and `group_list` 500'd and Horizon redirected the modal away), and **F7's were three
response-shape/request-parsing defects** plus the boot-from-volume BDM — neither is a quota defect, and both
create actions rendered fine. The hypothesis was reasonable and it did direct the work usefully — but it
understated the size of the job, and each defect was invisible until its predecessor was fixed, because
Horizon abandons a request at the first failure. Anyone reading F8's "one fix likely re-enables several"
should treat it as a starting hypothesis, not a finding.

**Answer to #482.** Lightstack is a drop-in for the OpenStack API/CLI — the open F10 (real-mode port create)
aside — **and, after #532 and #533, the Horizon
dashboard is usable for UI-driven resource management** — create a network, create a volume, launch an
instance, then SSH into it, all from the dashboard, in stub and real mode — and, after #534, watch it boot on
the Console tab. **All nine original #482 findings are closed.** The remaining gaps are the pre-existing
defects F10, F11 and F15, which this verification surfaced but did not cause, F12 (no EC2-credential UI,
by design), and F18 (domain-scoped tokens, #549); F13, the fourth of those pre-existing defects, is fixed
here by #545.

This does not rehabilitate the prior `HORIZON_FULL_COMPATIBILITY_REPORT.md` **"100% compatible / 342
endpoints"** claim. That claim was never grounded in a browser-level check, and the eighteen findings in this
report are what grounding it cost — endpoint coverage alone did not, and does not, imply a working dashboard.

**Follow-ups** (separate tickets — this is verify-not-fix scope, as SPEC-016 spun off #524). **F1–F9 and F17**
are tracked under [cloud-storage#531](https://github.com/cobaltcore-dev/cloud-storage/issues/531) — whose body
scopes itself to F1–F9, split into #532 / #533 / #534, with F17 (filed as `F10` on the #482 branch) folded into
#533 on 2026-09-22. **F18** is tracked under
[cloud-storage#549](https://github.com/cobaltcore-dev/cloud-storage/issues/549), which sits **outside** the #531
umbrella; the #533 pass's F10, F11, F13 and F15 are tracked in the sub-issues of
[cloud-storage#536](https://github.com/cobaltcore-dev/cloud-storage/issues/536) — F10/F11 in #546, F13 in #545 (fixed here),
F15 in #548 ([#537](https://github.com/cobaltcore-dev/cloud-storage/issues/537), where several
were first filed, was closed as superseded). F16, filed there for #547, is fixed by #533 (FR-025).
- ✅ **F4, F5, F6, F9 — DONE (SPEC-019 / cloud-storage#532, 2026-07-24):** networks list once (F4); the `volume`
  catalog alias advertises the configured host (F5); the Cinder volume response carries the full non-null
  upstream field set (F9); Glance image delete removes the image with owner-or-admin authorization (F6).
- ✅ **F2, F3, F7, F8 — DONE (SPEC-020 / cloud-storage#533, 2026-07-29):** the quota/limits read surface is
  complete and self-consistent (one defaults source per service, all `detail`/`default` variants, real usage
  counters, and `reserved` on every quota-details resource); Cinder serves `groups/detail` and
  `os-quota-sets`, plus a new `/v3/extensions`; security groups carry their rules array with the full key set;
  server create resolves security groups by **id or name**; networks carry `project_id` with resolvable
  `subnets`. Verified in **both** modes. *(The row **Delete** buttons noted under the old F6 follow-up work
  too — F14 explains why one of them appeared not to.)*
- ✅ **F17 — DONE with the same change** (`ListSecurityGroups` emits `security_group_rules`), which is why the
  2026-09-22 re-test found it open on the base and fixed on this branch.
- **F1 (FIXED — #534 / SPEC-021)** — a real noVNC console: persisted+revocable token store, libvirt VNC-socket
  readback per request, a token-resolving proxy built by configuring the already-pinned noVNC image rather than
  writing a relay, and a configurable origin. Also closed two things the ticket did not name — the guest VNC
  listened on `0.0.0.0` (token bypassable) and console-port reuse could point a stale token at another tenant's
  guest; both are gone with the move to per-instance unix sockets.
- **F10, F11, F12, F15 (open)** — pre-existing, surfaced by the #533 pass; see *Findings from the #533 pass*.
  **F13** (`POST /v3/projects` 500 on a non-UUID `domain_id`) was the highest value of that set — the Identity
  panel's own create-project path — and is fixed here (#545 / SPEC-022, 2026-08-07). Of what remains, **F10**
  (`POST /v2.0/ports` 500) is the highest value, per #546: it is also the likely root of the guest's
  metadata-service reachability failures.
- **F18 (open — [cloud-storage#549](https://github.com/cobaltcore-dev/cloud-storage/issues/549))** — refuse
  domain-scoped token requests instead of silently returning an unscoped token (already implemented on
  `feature/549`).
- ✅ **F16 — DONE (SPEC-020 FR-025 / cloud-storage#533):** the launch wizard's default *Create New Volume*
  creates the volume it asks for and boots from it, instead of booting the image directly.

**Reproducibility (SC-005 — self-reproduced 2026-09-22).** Both passes are fully scripted in the two
`HOST_TESTING*.md` runbooks (the documented procedure of FR-014); the real pass reuses the SPEC-014
`deployments/rook/setup-rook-rgw.sh` fixture. Both runbooks were **re-run end-to-end on 2026-09-22** (T033)
and every recorded verdict reproduced; the drift that run exposed is folded into this report. SC-005 is
worded for an *independent* reader, so this is self-reproduction — the strongest evidence available from
the authoring side, not formal confirmation.
