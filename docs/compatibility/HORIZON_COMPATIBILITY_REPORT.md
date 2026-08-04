# Horizon Dashboard Compatibility Report

**Ticket**: [cobaltcore-dev/cloud-storage#482](https://github.com/cobaltcore-dev/cloud-storage/issues/482)
**Date**: stub pass 2026-07-22; real (graded) pass 2026-07-23. **Re-verified after #532 (2026-07-24) and #533
(stub + real, 2026-07-27…29)** — the #533 re-verification ran on `feature/533` with the o3k image built from
that branch (`docker.io/library/o3k:spec020`); same Horizon image and topology as the original real pass.
**Verdict (revised 2026-07-29, after #532 and #533):** **the Horizon dashboard is usable for resource
management.** Create a network with a subnet, create a volume, and launch an instance — all through the
dashboard, verified in stub **and** real mode; the launched VM reaches ACTIVE on a real libvirt domain
booting from a Ceph-RGW image, and a key pair created in Horizon lets you **`ssh`** into it. Read/list panels
render, the Overview limits widget loads, networks list once, and image delete works. **All nine #482 findings
are now closed**: F1, the noVNC console, was fixed in #534 (SPEC-021). O3K's API/CLI surface was compatible
throughout.

> **What this verdict was, and why it changed.** As originally written (2026-07-23) it read: *"the Horizon
> dashboard is **not** usable for resource management … every create/write workflow is broken … O3K is
> API/CLI-compatible; the Horizon UI is *not* a drop-in for resource management."* That was accurate for the
> code as it stood — 8 of 9 findings were real and reproduced. #532 fixed the read/list breaks (F4/F5/F6/F9)
> and #533 the create/write cluster (F2/F3/F7/F8). The original assessment also **refuted SPEC-002's
> ungrounded "100% compatible" claim**, and that refutation stands: the claim was never grounded in a
> browser-level check, and the nine findings are what grounding it actually cost.

**16 findings total**: F1–F9 from this pass (#482), F10–F16 added by the #533 pass.

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
> defects**. Each surfaced only after its predecessor was fixed, because Horizon abandons a request at the
> first failure. Details under each finding.
>
> The pass also produced **seven new findings, F10–F16**, none of them regressions — **five** pre-existing
> defects (F10–F13, F15), one config-drift bug introduced and fixed within the pass (F14), and one documented
> limitation of the F7 fix (F16). See *Findings from the #533 pass*.

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
| 2 | Project switching | **partial** | **working (switcher); re-scope still not exercised** | Real: switcher present + correct (`Default`/`default`), but only the `default` project is seeded → nothing to switch to (fixture limitation, not a Horizon fault). F2 limits-error in real: _TBC_. | F2 |
| 3 | Create network | ✅ **working** (#533) | ✅ **working** (#533) | **F3+F4 reproduce in real (2026-07-23)**: Project → Network → Networks still has **no Create button** (only Admin does); an Admin-created net lists **twice** in Project. Neutron-vs-UI dup source: see F4. **#533 (2026-07-29): FIXED** — Create Network present on the Project panel; `ui-net` + `ui-sub` created through the dialog and listed once, stub and real. | F3, F4 |
| 4 | Create volume | ✅ **working** (#533) | ✅ **working** (#533) | Real (2026-07-23): volumev3 catalog is **clean** (`o3k-o3k:8776`) and CLI `volume create` → `available` — Cinder API works and F5's stub `localhost:8776` is **not** on Horizon's v3 path. Empty panel renders; **Create Volume button opens no dialog** (F8, create-UI cluster, not the endpoint). And once a volume exists the panel **500s** (`TemplateSyntaxError` in `get_rows`) → **F9**. **#533 (2026-07-29): FIXED** — the dialog opens (root was the cinder `groups/detail` route, not metadata) and `ui-vol` reaches `available`, backed by a real qcow2 under `/var/lib/o3k/volumes`. | F5, F8, F9 |
| 5 | Launch instance (+SSH) | ✅ **working** (#533) | ✅ **working via the UI** (#533) | Real (2026-07-23): UI wizard can't select a network (F7). But CLI `server create` → **ACTIVE**, `power_state Running`, real **libvirt VM booted from the RGW image**, `addresses=test-net=10.0.0.77`, `key_name`+`security_groups` present. Real compute + RGW-image boot **proven**; **`ssh cirros@10.0.0.77` succeeds** (`uid=1000` — cloud-init key injection + default-SG tcp/22 + reachable fixed IP all work). Console → T7. **#533 (2026-07-29): FIXED via the UI** — `ui-vm` launched from the wizard reaches **ACTIVE** on a real libvirt domain with an address on `spec020-net`; and with a key pair created **in Horizon**, `ssh cirros@<ip>` returns `uid=1000`. | F7 |
| 6 | Instance console (noVNC) | not-exercised | **works** (after #534) — serial log **works** | Real (2026-07-23): `console log show vm1` returns real cirros boot + EC2 metadata + `vm1 login:` (serial console **works**). noVNC Console **tab** → "Unable to load Console" (F1 confirmed — no guest). **Re-verified after #534/SPEC-021: the Console tab renders the live guest framebuffer and accepts keyboard input.** | F1 ✅ |

_Verdicts: working / partial / not-working / not-exercised (stub-only cell) / pending._

## Acceptance criteria (ticket #482)

| Criterion | Result |
|---|---|
| Login + project switching verified in real UI | **Login ✅** (real). Project switching: **partially verified** (2026-08-07). The blocker is gone — `POST /v3/projects` no longer 500s (**F13**, fixed in #545/SPEC-022) — and the mechanism behind the switcher is proven at the API: a second project creates with `--domain default`, `GET /v3/auth/projects` lists both, and a token re-scopes to the second. The **switch through the dashboard is still undemonstrated**; it needs a Horizon pass, which this unblocks rather than performs. Originally logged as a fixture limit; the #533 pass found the actual blocker. |
| Resource management: launch / network / volume | ✅ **All three now succeed through the UI** (#533, 2026-07-29, stub **and** real): network + subnet via the Create Network dialog; volume via the Create Volume dialog (`available`, real qcow2 on disk); instance via the Launch wizard (**ACTIVE**, real libvirt domain, booted from the RGW image). Beyond the criterion: a Horizon-created key pair yields working **`ssh`** into that VM. Originally: all three failed in the UI while succeeding via API/CLI. |
| Report: per workflow working / not working (+ gaps) | ✅ this document |
| Report published to `docs/` | ✅ |

## Findings

### F1 — noVNC console shows no guest (major) — ✅ **FIXED (SPEC-021 / #534, 2026-07-31)**
- **Cause**: synthetic `http://localhost:6080` console URL + a random, unpersisted token + a
  fabricated VNC port (never read from libvirt) + an unwired noVNC proxy (`internal/nova/console.go`;
  `research.md` U1).
- **Impact**: the Console tab cannot display a VM. In real it shows **"Unable to load Console" +
  "Console currently not available"** — the returned URL points at `localhost`, so the browser never
  reaches the proxy at all. The serial Console Log works (verified real: real cirros boot output +
  `vm1 login:`).
- **Fix**: all four causes, plus one the ticket did not name.
  - **The relay is not written, it is configured.** The noVNC image already pinned in the verification
    stacks bundles websockify and its token plugins; a command override turns it into a real
    token-resolving proxy serving both the client page and the WebSocket relay. Verified before
    committing to the design: a good token yields `101 Switching Protocols` and `connecting to: …`, a
    bad one is closed with no response and no guest contacted. O3K gained **no** relay code and **no**
    new Go dependency. It ships in `charts/o3k` as `consoleProxy`.
  - **Real port**: read from libvirt per request via `DomainGetXMLDesc(dom, 0)` — flags `0`, the
    *running* definition, because `DomainXMLInactive` still reports `port='-1'` and would reproduce the
    original bug. Never cached: libvirt reassigns the port on every start.
  - **Real token**: persisted in `console_auth_tokens` (migration 065) and projected into
    `/var/lib/o3k/console-tokens/` for the proxy to read. Expiry is enforced by *deleting* that entry —
    websockify performs no expiry check — so a reconciler sweep is the mechanism, not housekeeping.
  - **Configurable URL**: `nova.console_proxy_base_url`, an **origin** only. This setting appeared in
    four operator guides for years while the binary ignored it entirely; their example values also named
    `vnc_auto.html`, a page that does not exist in noVNC 1.6.0.
  - **Not in the ticket, found while designing**: the guest VNC listened on `0.0.0.0`, so the token was
    bypassable by anything that could reach the node. It now binds loopback, making the proxy the only
    route in. And because libvirt hands out console ports sequentially, a token surviving a stop could
    later resolve to a **different tenant's** guest — so stop/start/reboot/suspend/resume/delete all
    revoke. `sharePolicy='force-shared'` additionally stops a second viewer evicting the first.
  - **A second, independent blocker, found only by fixing the first.** O3K's own Horizon config set
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

### F2 — Project Overview: "Unable to retrieve limits information" (medium) — _stub_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**
- **Cause**: the Overview usage/limits widget's call (Nova limits/quota) errors on load / after project switch.
- **Root (confirmed)**: the quota/limits read surface was incomplete and self-inconsistent. Nova's
  absolute-limits view carried its **own** default map with instances/cores/RAM at `100/200/512000` against a
  quota-set default of `10/20/51200` — ten times apart; Nova had no `os-quota-sets/{id}/detail`; Neutron had
  no `quotas/{id}/default` and no `.json`-suffixed details; Cinder served no usage view (its `?usage=true`
  parameter was ignored) and no `/defaults`, and its limits view ignored the `cinder_quotas` table entirely, so an admin's override had no
  effect on it.
- **Fix**: one `resolve*QuotaLimits` helper per service as the single source of defaults, overlaid with the
  quota table, so the limits and quota-set views cannot disagree; the missing `detail`/`default` variants
  added; Neutron advertises `quota_details`. Nova's inflated map was **latent** — migration 007's seed masked
  it for the seeded project, so only unseeded projects ever saw `100/200/512000`.
- **Verified**: stub + real — the widget renders Compute, Volume and Network groups. The Network group
  appearing is itself the proof that Horizon took the `quota_details` path.
- **Still open**: reload **after a project switch** was never verified. The blocker is cleared — a second
  project can now be created (**F13**, fixed in #545/SPEC-022) — so this is a pending Horizon check rather
  than a blocked one.

### F3 — No "Create Network" in the Project panel (medium) — _stub_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**
- **Cause**: Project → Network → Networks lacks the Create Network button (only Admin → Network → Networks
  has it). Horizon hides it when the expected capability/policy/quota isn't advertised by Neutron.
- **Root (confirmed)**: **quota, not policy.** Upstream Neutron's default `create_network` rule is
  `rule:admin_or_project_member`, so a stock Horizon already permits a project member — the control was
  hidden because the network-quota fetch failed. Grounded against upstream policy before the fix, which is
  why no policy change was needed.
- **Fix**: the same quota surface as F2, plus `project_id` alongside `tenant_id` on the network object.
- **Verified**: stub + real — the button is present on the *Project* panel and the dialog creates a network
  with a subnet, listed exactly once (the #532 F4 dedup still holding).

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

### F7 — Launch Instance not completable via the dashboard (major) — _stub + real (2026-07-23)_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**

> **Root: not one cause but four, and none of them quota.** The quota work restored the Launch button and the
> wizard, but the wizard then failed for a chain of unrelated reasons, each invisible until the previous one
> was fixed — Horizon abandons a request at its first failure:
>
> 1. **Networks step empty** — the wizard expands a network's `subnets` **ids** against a separately-scoped
>    subnet list; `networkSubnetIDs` (no project predicate, unpaginated) and `ListSubnets` (project-scoped,
>    paginated) disagreed, so a referenced id could be unresolvable and render a null row. `ListSubnets` is
>    now scoped by the owning **network's** visibility. A unit test (`TestSubnetReferenceIntegrity`) caught
>    this before the browser did.
> 2. **Security-group step empty, 500** — `GET /v2.0/security-groups` omitted `security_group_rules`.
>    openstacksdk's `to_dict()` emits that key unconditionally, so "absent" arrives as **null**, and Horizon's
>    guard (`if 'security_group_rules' not in sg`) does not catch null → `'NoneType' object is not iterable`.
> 3. **Then `500 "'protocol'"`** — with the array present, Horizon read each rule's fields as `sgr['protocol']`
>    unconditionally; O3K emitted those keys only when non-NULL, and the seeded default group's egress rules
>    have a NULL protocol. **A null value is fine; an absent key is not.**
> 4. **Then `400 Security group <uuid> not found`** — once the SG list worked, the wizard started *sending*
>    security groups, as **ids** (`setFinalSpecSecurityGroups` pushes `securityGroup.id` when Neutron is
>    enabled). O3K resolved that field by name only. Upstream Nova with Neutron matches **name or UUID**.
>
> A fifth defect sat behind the Launch button itself: Horizon defaults **Create New Volume = Yes**, which
> sends the image inside `block_device_mapping_v2` (`source_type: image`) with an empty `imageRef`. O3K
> ignored that entry and errored the instance to **ERROR** with `invalid UUID length: 0`. It now adopts the
> entry as the boot image — see **F16** for the limitation.

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
- **Verified fixed**: stub + real. Real pass 2026-07-29 — `ui-vm` **ACTIVE**, address on `spec020-net`, a real
  libvirt domain (`instance-<hex>`), and the serial console shows a genuine cirros boot from the RGW-backed
  image. Both follow-up questions above are answered by the four roots in the box.

### F8 — Horizon resource-*creation* UIs are broken while the APIs work (major) — _stub + real (2026-07-23)_ — ✅ **FIXED (SPEC-020 / #533, 2026-07-29)**

> **Root: a Cinder route-registration bug — the "shared metadata root" hypothesis below was wrong for this
> finding.** The Create Volume button appeared but did nothing, with **no traceback and no 500** anywhere.
> Capturing the modal fetch itself was the only way to see it:
>
> ```
> GET /dashboard/project/volumes/create/  →  200, Content-Length: 0
> X-Horizon-Location: /dashboard/project/volumes/
> Set-Cookie: messages=… "Unable to retrieve the volume group list.…failed to get group (HTTP 500)"
> ```
>
> `CreateForm._populate_group_choices` calls `cinder.group_list`, which python-cinderclient issues as
> `GET /v3/groups/detail`. O3K registered only `groups/:id`, so gin matched `"detail"` as a group id and
> `GetGroup` answered **500**. Horizon converts any `group_list` error into a redirect, so the modal returned
> an empty 200 with `X-Horizon-Location` and the browser navigated instead of opening a dialog — a button that
> looks inert. `groups/detail` is now registered **before** `groups/:id`, and the group routes also exist under
> `/v3/{project_id}`.
>
> **A wrong diagnosis is recorded here deliberately.** The first investigation concluded the cause was
> oslo.policy scope enforcement, from an `openstack_auth` warning and a `volume:create` DEBUG line. Checking
> `openstack_auth` at `stable/2025.2` disproved all three links: `domain_id` is set in the policy credentials
> **unconditionally**; the "domain scoped token" warning comes from a path `check()` only consults for
> `scope == 'identity'`; and the `enforce:` DEBUG line is logged on every evaluation, pass or fail. The lesson
> is procedural — the failure was invisible in both the browser and O3K's log, and visible only in the HTTP
> response between them.

- **Note**: O3K's `/v3/extensions` (absent, 404) was also added during this work. It was a genuine missing
  endpoint that Horizon's create-volume form probes, but it was **not** the blocker.
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

## Findings from the #533 pass (F10–F16)

Surfaced while fixing F2/F3/F7/F8, in stub and real mode. **None is a regression from #532 or #533** —
**F10–F13 and F15** are pre-existing and reproduce on the base commit; **F14** was introduced *and* fixed
inside the pass (on `71aa00d` both `local_settings` copies carried `CSRF_COOKIE_HTTPONLY = True`, so there
was no drift to inherit); **F16** is a documented limitation of the F7 fix.

### F10 — `POST /v2.0/ports` 500s: bridge in the root namespace, TAP attached in a project namespace (major) — _real_ — ❌ **open**
- **Cause**: `CreateNetwork` creates the Linux bridge in the **root** netns, deliberately, "for libvirt access"
  (`internal/neutron/network.go:484`). `CreatePort` then attaches the TAP **inside** `light-ns-<project>`
  (`ports.go:98`) — a namespace that is never created. Result:
  `failed to attach tap-… to bridge br-… in namespace light-ns-…: Device does not exist`.
- **Confirmed on the host**: the `br-*` bridges are present in the root namespace and `ip netns list` shows only
  k3s's own `cni-*` entries — no `light-ns-*`.
- **Why the dashboard still works**: Nova builds the domain's interface itself and never calls `CreatePort`,
  which is why an instance reaches ACTIVE with an address while explicit `port create` fails.
- **Also breaks**: `test/contract/neutron`'s `TestPortTopologyData_Contract`; and it is the likely root of the
  guest's `169.254.169.254 request failed` / `no interfaces have a carrier` console lines.

### F11 — Horizon cannot attach a router gateway: O3K models no external networks (medium) — _real_ — ❌ **open**
- **Cause**: `router:external` is hard-coded `false` on every network object
  (`internal/neutron/network.go:517,693,766`) and `GET /v2.0/networks?router:external=true` deliberately returns
  `[]` (`network.go:636`). Horizon's router **Set Gateway** dialog populates its dropdown from exactly that
  filter, so it is always empty.
- **Impact**: north-south egress for a guest cannot be configured from the dashboard. The CLI path works —
  `UpdateRouter` accepts `external_gateway_info` directly (`router.go:68`) — so this is a UI-surface gap, not a
  missing capability.

### F12 — No Horizon UI for EC2 credentials (low) — _real_ — ❌ **open by design**
- O3K implements the EC2-credentials API and `/v3/s3tokens` (`internal/keystone/handlers.go:48,78-81`), but
  Horizon exposes no panel for OS-EC2, so tenant S3 credentials are CLI-only. Upstream Horizon is the same;
  recorded so the tenant-object-storage workflow isn't mistaken for a defect.

### F13 — `POST /v3/projects` returns 500 (major) — _stub + real_ — ✅ **FIXED (SPEC-022 / #545, 2026-08-07)**
- **Impact**: a second project could not be created, so **project switching had never been demonstrable** — the
  #482 criterion above, originally attributed to a fixture limitation. It also blocked verifying the Overview
  widget across a project switch (F2's residual) and the tenant-object-storage flow.
- **Cause** (the guess in the original entry was right): `CreateProject` defaulted only the *empty*
  `domain_id`, then passed any supplied string straight into a `uuid` column. `domain_id: "default"` — what
  every OpenStack client sends, because upstream Keystone's Default domain literally carries `default` as its
  id — failed at the database and surfaced as a generic `computeFault`. `POST /v3/users` rejected the same
  value cleanly with a `400`, so the reference implementation was already in the same file.
- **Fix**: one shared resolver, used by project, user and group creation *and* by domain retrieval, resolving
  the Default domain **by name** rather than by a hard-coded id (migrations 009 and 037 seed that name with
  different ids). An unknown or disabled domain now answers `400` naming the field; a duplicate project name
  answers `409` instead of a `500`. `GET /v3/domains/default` resolves, and `GET /v3/domains?name=` filters.
- **Verified — API (2026-08-07, stub stack built from `feature/545`)**: `POST /v3/projects --domain default`
  → **`201`**; `GET /v3/auth/projects` — the call that populates Horizon's project switcher — returns **both**
  projects; re-scoping a token to the second returns **`201`** with `token.project.name` set to it. So the
  mechanism behind the switcher is proven end to end.
- **Still unverified — UI**: the switch *through the dashboard* has not been exercised, because that needs a
  Horizon pass. This fix unblocks the #482 criterion rather than satisfying it; the same applies to F2's
  residual (Overview widget across a switch) and the tenant-object-storage flow.

### F14 — `local_settings` drift between the compose config and the Helm chart (medium) — _real_ — ✅ **FIXED in-pass**
- **Cause**: `deployments/horizon-chart/files/local_settings` is **generated** from
  `deployments/horizon-config/local_settings` by `make horizon-chart-sync`, and **real mode reads the chart
  copy**. A hand-edit to the compose copy left the two disagreeing.
- **Symptom**: with the chart still carrying `CSRF_COOKIE_HTTPONLY = True`, Project → Compute → Key Pairs →
  **Delete Key Pair** silently did nothing. Django logged `Forbidden (CSRF token missing.)`; O3K never saw the
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

### F16 — Boot-from-new-volume is adopted, not implemented (low) — _stub + real_ — ⚠️ **documented limitation**
- Horizon's launch wizard defaults **Create New Volume = Yes** and sends the image via
  `block_device_mapping_v2` (`source_type: image`, `destination_type: volume`) with an empty `imageRef`. O3K now
  **adopts that entry as the boot image** so the wizard's default path reaches ACTIVE — but it does **not**
  materialize a Cinder volume, so `volume_size` and `delete_on_termination` on that entry are ignored and no
  volume appears under Project → Volumes. Real boot-from-new-volume belongs with SPEC-009's storage work.

### Cosmetic observations (not findings)
- The seeded Cinder volume type is named **`ceph-rbd`** ("Ceph RBD backed volumes") even when
  `cinder.storageMode: local`. Misleading in the UI — it sent one investigation down a Ceph path before the
  qcow2 under `/var/lib/o3k/volumes` disproved it. `GET /v3/types/default` correctly reports `__DEFAULT__`.
- `POST /v2/images` stored `disk_format: raw` for an image created with `--disk-format qcow2`. Noticed in
  passing, not investigated.

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

**Superseded — what didn't work at the time of writing (2026-07-23).** Every create/write workflow through the
UI failed: no Create-Network button (F3), a dead Create-Volume modal (F8), the Volumes list **500**-ing once a
volume existed (F9), the Launch wizard unable to select a network (F7); plus networks listing **twice** (F4),
image delete a **no-op** (F6), and the overview **limits widget** erroring (F2). #532 and #533 fixed all eight.

**On the "common thread" this report proposed.** F8 argued the create UIs shared one root — quota/limits/type
metadata Horizon reads up front. That held for **F2 and F3 only**. **F8's real root was a Cinder
route-registration bug** (`groups/detail` shadowed by `groups/:id`, so `group_list` 500'd and Horizon redirected
the modal away), and **F7's were three response-shape/request-parsing defects** plus the boot-from-volume BDM.
The hypothesis was reasonable and it did direct the work usefully — but it understated the size of the job, and
each defect was invisible until its predecessor was fixed, because Horizon abandons a request at the first
failure. Anyone reading F8's "one fix likely re-enables several" should treat it as a starting hypothesis, not a
finding.

**Answer to #482.** O3K is a drop-in for the OpenStack API/CLI **and, after #532 and #533, the Horizon
dashboard is usable for UI-driven resource management** — create a network, create a volume, launch an instance,
then SSH into it, all from the dashboard, in stub and real mode — and, after #534, watch it boot on the Console
tab. **All nine #482 findings are closed.** The remaining gaps are the pre-existing defects **F10, F11, F12 and
F15**, which this verification surfaced but did not cause; **F13** was fixed in #545 (SPEC-022, 2026-08-07).

This does not rehabilitate the prior `HORIZON_FULL_COMPATIBILITY_REPORT.md` **"100% compatible / 342
endpoints"** claim. That claim was never grounded in a browser-level check, and the sixteen findings in this
report are what grounding it cost — endpoint coverage alone did not, and does not, imply a working dashboard.

**Follow-ups:**
- ✅ **F4, F5, F6, F9 — DONE (SPEC-019 / cloud-storage#532, 2026-07-24):** networks list once (F4); the `volume`
  catalog alias advertises the configured host (F5); the Cinder volume response carries the full non-null
  upstream field set (F9); Glance image delete removes the image with owner-or-admin authorization (F6).
- ✅ **F2, F3, F7, F8 — DONE (SPEC-020 / cloud-storage#533, 2026-07-29):** the quota/limits read surface is
  complete and self-consistent (one defaults source per service, all `detail`/`default` variants, real usage
  counters); Cinder serves `groups/detail` and `os-quota-sets`, plus a new `/v3/extensions`; security groups
  carry their rules array with the full key set; server create resolves security groups by **id or name**;
  networks carry `project_id` with resolvable `subnets`. Verified in **both** modes. *(The row **Delete**
  buttons noted under the old F6 follow-up work too — F14 explains why one of them appeared not to.)*
- **F1 (FIXED — #534 / SPEC-021)** — a real noVNC console: persisted+revocable token store, libvirt VNC-port
  readback per request, a token-resolving proxy built by configuring the already-pinned noVNC image rather than
  writing a relay, and a configurable origin. Also closed two things the ticket did not name — the guest VNC
  listened on `0.0.0.0` (token bypassable) and console-port reuse could point a stale token at another tenant's
  guest.
- **F10, F11, F12, F15 (open)** — pre-existing, surfaced by the #533 pass; see *Findings from the #533 pass*.
  **F13** (`POST /v3/projects` 500) was the highest value of that set — the reason project switching had never
  been demonstrated, and a blocker on two other checks — and was fixed in #545 (SPEC-022, 2026-08-07). Of what
  remains, **F10** (`POST /v2.0/ports` 500) is the highest value, per #546: it is also the likely root of the
  guest's metadata-service reachability failures.
- **F16** — boot-from-new-volume is adopted rather than implemented; belongs with SPEC-009 storage work.

**Reproducibility (SC-005 met).** Both passes are fully scripted in the two `HOST_TESTING*.md` runbooks;
the real pass reuses the SPEC-014 `deployments/rook/setup-rook-rgw.sh` fixture.
