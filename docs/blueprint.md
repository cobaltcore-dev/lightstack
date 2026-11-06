# Lightstack Blueprint & OpenStack Compatibility Design

**Status**: Living document (cloud-storage#420; updated for SPEC-014 — cloud-storage#480)
**Created**: 2026-06-30
**Reference release**: OpenStack **2025.2 (Flamingo)**
**lightstack version**: v0.6.1 (2026-04-22) + SPEC-014 (Ceph RGW S3 backend, EC2/S3 tenant credentials — cloud-storage#480)
**Tracking**: [cloud-storage#420](https://github.com/cobaltcore-dev/cloud-storage/issues/420) (epic #413 — Lightstack for validation)

This is the **authoritative top-level** compatibility reference for lightstack. It serves two audiences:

1. **Engineers working on lightstack** — an accurate map of what is implemented, how, and where the edges are.
2. **Teams evaluating Lightstack as a devstack replacement** — an honest compatibility surface and an explicit list of gaps.

Deep per-endpoint detail and contract-test results live in
[`compatibility/OPENSTACK_COMPATIBILITY_REPORT.md`](compatibility/OPENSTACK_COMPATIBILITY_REPORT.md)
(linked appendix). Architecture rationale lives in [`ARCHITECTURE.md`](ARCHITECTURE.md). This document
is the single source of truth for *"what does Lightstack implement, and what doesn't it?"*

> **A note on numbers.** Earlier docs report Lightstack coverage as "104% (342/330 endpoints)." That figure
> counts raw HTTP routes (including URL aliases and version-discovery) against a hand-picked baseline.
> This blueprint counts **functional OpenStack API operations** under one stated rule (below) and reports
> honest per-resource coverage instead of a single percentage. The external epic (#413) assesses Lightstack at
> "~70% complete"; both views are reconciled in [§8 Known gaps](#8-known-gaps--limitations).

---

## Table of contents

1. [Service map (lightstack ↔ OpenStack)](#1-service-map)
2. [API surface & counting methodology](#2-api-surface--counting-methodology)
3. [Compatibility matrix (per service)](#3-compatibility-matrix)
4. [Storage backends](#4-storage-backends)
5. [Synchronous architecture & trade-offs](#5-synchronous-architecture--trade-offs)
6. [Terraform coverage](#6-terraform-coverage)
7. [sci-in-a-box integration surface](#7-sci-in-a-box-integration-surface)
8. [Known gaps & limitations](#8-known-gaps--limitations)

---

## 1. Service map

Lightstack runs as a single Go binary that starts independent HTTP servers, one per OpenStack service,
plus a metadata server and Kubernetes health endpoints. All share one PostgreSQL pool and one stateless
JWT auth path.

| Lightstack service | OpenStack counterpart | API version | Port | Catalog type | Status |
|---|---|---|---|---|---|
| Keystone | Identity | v3 | 35357 | `identity` | Core |
| Nova | Compute | v2.1 (microversions 2.1–2.90) | 8774 | `compute` | Core |
| Neutron | Network | v2.0 | 9696 | `network` | Core |
| Cinder | Block Storage | v3 | 8776 | `volumev3` | Core |
| Glance | Image | v2 | 9292 | `image` | Core |
| Placement | Placement | v1.0 | 8778 | `placement` | **Stub** (empty lists, for Horizon) |
| Metadata | Nova metadata / EC2 | — | 8775 | — (no auth) | Core |
| Health | (k8s probes) | — | on identity port | — | `/livez` `/readyz` `/healthcheck` |

There is **no `object-store` (Swift) service** in the catalog; Ceph RGW's S3 API is instead advertised
under a non-standard **`s3`** service type (migration 062) for tenant object storage — see
[§4](#4-storage-backends) and [§8](#8-known-gaps--limitations).

---

## 2. API surface & counting methodology

Endpoint counts in prior docs disagreed (README: 342; OPENSTACK_COMPATIBILITY_REPORT: per-service tallies
summing differently; raw route greps: higher still) because none stated *what they counted*. This blueprint
fixes one rule and re-derives everything from the route registrations in `cmd/lightstack/main.go` and
`internal/*/`.

**Counting rule.** A *functional operation* is a unique (HTTP method × canonical resource path). Excluded
from the functional count:

- **Version-discovery roots** (`GET /`, `GET /v3`, `GET /v2.1`, …) — informational, not resource operations.
- **Cinder project-scoped URL aliases** — Cinder registers most operations twice: a token-scoped form
  (`/v3/volumes`) and a project-in-path form (`/v3/{project_id}/volumes`). These are the *same* operation
  at two URLs; counted once. The project segment is decoration — every handler reads the caller's project
  from the token — so the two forms return the same collection for the same caller. Note the two sets are
  *not* yet identical: 13 operations are still registered only on the token-scoped form (see the note above
  `r.Group("/v3/:project_id")` in `internal/cinder/volumes.go`), so "same operation at two URLs" describes
  the operations present at both, not the whole surface.
- **PUT/PATCH duplicate registrations** bound to the same handler (lightstack accepts both for client tolerance;
  e.g. Neutron `networks`/`subnets`/`ports`/`security-groups`). Counted once, by the OpenStack-canonical verb.

Reported **separately** (not part of the five-service OpenStack API count): Placement (stub), Metadata, Health.

### Per-service counts (derived from source, v0.6.1 + SPEC-014)

> **Stale as of 2026-07-29 — counts predate SPEC-019 (#532) and SPEC-020 (#533).** Those features added
> functional operations not reflected below: Nova `os-quota-sets/{id}/detail`; Neutron `quotas/{id}/default`;
> Cinder `quota-sets/{id}?usage=true`, `/defaults`, `groups/detail` and a new `GET /v3/extensions`. They also
> added pure **aliases** that should *not* raise the functional count — Neutron's `quotas/{id}/details.json`
> and `quotas/{id}/default.json` (same handlers as `/details` and `/default`), Cinder's `os-quota-sets/*`
> spelling, and project-scoped duplicates of the group, quota, `/extensions` and `/os-availability-zone`
> routes. The
> numbers have deliberately **not** been re-derived here rather than estimated; recount from source with the
> methodology above before quoting them.

| Service | Raw routes registered | Functional operations | Notes |
|---|---:|---:|---|
| Keystone | 66 | **65** | −1 discovery (`GET /v3`); +6 SPEC-014 (OS-EC2 CRUD, `s3tokens`, `ec2tokens`); `GET /` on identity port lives in `main.go` |
| Nova | 74 | **71** | −2 discovery, −1 PUT/PATCH alias |
| Neutron | 100 | **94** | −2 discovery, −4 PUT/PATCH aliases |
| Cinder | 116 | **67** | −2 discovery, −2 PUT/PATCH, −45 project-scoped aliases |
| Glance | 38 | **38** | discovery roots live in `main.go` |
| **Five-service total** | **394** | **335** | |
| Placement (stub) | 5 | 3 | resource_providers / resource_classes / traits — empty lists |
| Metadata | 7 | 7 | EC2 + OpenStack formats, unauthenticated |
| Health | 4 | 3 | `/livez`, `/readyz`, `/healthcheck` (GET+HEAD) |
| **Total routes served** | **≈407** | | |

**Result:** Lightstack implements **~335 functional operations** across the five core OpenStack services (329 at
the v0.6.1 baseline + 6 from SPEC-014's EC2/S3 credential surface). That is the honest figure behind the
"330 baseline"; the previous "342 / 104%" came from counting raw routes (aliases + discovery) against it. Coverage is substantial — the per-resource detail is in
[§3](#3-compatibility-matrix), and what's genuinely *missing* is in [§8](#8-known-gaps--limitations).

> Cinder and Neutron functional counts are ±1–2 pending the full per-operation enumeration in §3.

---

## 3. Compatibility matrix

Per service, each canonical OpenStack **api-ref** resource group (2025.2) is mapped to lightstack's
implementation status, derived from the route registrations in `internal/<service>/`. Status legend:

- ✅ **Implemented** — operation(s) present, exercised by tests or clients
- ⚠️ **Partial** — group present but some operations or semantics missing
- ❌ **Missing** — not implemented
- 🅢 **Stub** — route exists but returns canned/empty data

Contract-test figures come from `test/contract/<service>/` (gophercloud) and the
[OPENSTACK_COMPATIBILITY_REPORT](compatibility/OPENSTACK_COMPATIBILITY_REPORT.md) appendix.
**Caveat:** contract tests are *not* CI-gated (they need a live instance), so pass counts are
point-in-time, and a few report claims overstate the current code — flagged inline.

### 3.1 Keystone — Identity v3 (65 functional operations)

Canonical group list from the OpenStack Identity v3 api-ref (core operations + CURRENT extensions).

| Canonical group (Identity v3) | Lightstack | Notes |
|---|:--:|---|
| Authentication & token mgmt | ⚠️ | `POST`/`GET`/`DELETE /v3/auth/tokens` + `GET /v3/auth/projects`. Missing `HEAD /auth/tokens` (check), `GET /auth/catalog`, `GET /auth/system`/`domains`, MFA auth-receipts |
| Users | ✅ | CRUD + `password` + `/users/:id/projects` + `/users/:id/groups` |
| Application credentials | ✅ | create / list / get / delete (+ `GET /application_credentials/:id`) |
| Credentials | ✅ | full CRUD |
| Groups | ✅ | CRUD + membership (`/groups/:id/users`, add/remove) |
| Projects | ✅ | CRUD |
| Project tags | ❌ | — |
| Domains | ✅ | CRUD |
| Domain configuration | ⚠️ | `GET /domains/:id/config` only (no set/delete, no group/option granularity) |
| Roles | ✅ | CRUD |
| Role assignments | ⚠️ | project↔user assign/unassign/list/check + `GET /role_assignments`. Missing group & domain-scoped assignments, role inferences/implied roles |
| System role assignments | ❌ | — |
| Service catalog & endpoints | ⚠️ | services CRUD; endpoints list/create/delete (no `GET`/`PATCH /endpoints/:id`). **Known bug:** catalog URL-template substitution (3 contract-test failures) |
| Policies | ❌ | — |
| Regions | ❌ | — |
| Unified limits (registered_limits / limits) | ❌ | — |
| OS-INHERIT | ❌ | — |
| OS-FEDERATION / OS-OAUTH1 / OS-OAUTH2 / OS-TRUST | ❌ | enterprise SSO / delegation |
| OS-EP-FILTER / OS-ENDPOINT-POLICY / OS-REVOKE | ❌ | — |
| OS-EC2 (ec2 credentials) | ✅ | create / list / get / delete `…/OS-EC2` — mints project-scoped S3 access/secret pairs; returns the secret (recoverable, matching Keystone) (SPEC-014) |
| S3 / EC2 signature validation | ✅ | public `POST /v3/s3tokens` + `POST /v3/ec2tokens` — verify S3 SigV2/V4 (and EC2) signatures for Ceph RGW keystone auth; self-validating (SPEC-014) |
| OS-PKI / OS-SIMPLE-CERT | ❌ | deprecated upstream |

**Contract tests** (`test/contract/keystone/`, 11 files): report records **52 pass / 3 fail (94.5%)**; the
3 failures are the catalog URL-substitution bug. Not CI-gated.

**Doc-vs-code discrepancies found:** the compat report also lists `HEAD /v3/auth/tokens` and
`GET /v3/auth/catalog` as implemented; neither is registered in the current code. The status column
above reflects the code, not the report.

**Validation verdict:** Lightstack implements the full **core IAM surface** that Terraform, Horizon, and the
OpenStack CLI exercise — token auth, users, projects, domains, groups, roles, project role-assignments,
service catalog, credentials, application credentials, and (SPEC-014) EC2/S3 credentials + S3/EC2
signature validation for Ceph RGW. Every ❌ above is an enterprise SSO / delegation / policy / limits
feature **outside the sci-in-a-box validation scope** — the one in-scope defect is the catalog
URL-substitution bug (see [§8](#8-known-gaps--limitations)).

### 3.2 Nova — Compute v2.1 (71 functional operations)

Microversion negotiation: advertises **min 2.1 / max 2.90** (`novaMinVersion` / `novaCurrentVersion`
in `handlers.go`) via `OpenStack-API-Version: compute …` (and legacy `X-OpenStack-Nova-API-Version`).
The bulk of compute behaviour rides on `POST /servers/{id}/action`, a single route that dispatches the
full action set (below).

| Compute v2.1 group | Lightstack | Notes |
|---|:--:|---|
| Servers (CRUD) | ✅ | list / detail / create / show / update (PUT + PATCH) / delete |
| Server actions (`/servers/{id}/action`) | ✅ | start, stop, reboot, pause/unpause, suspend/resume, shelve/shelveOffload/unshelve, lock/unlock, resize/confirmResize/revertResize, rebuild, **rescue** (no unrescue), evacuate, migrate, live-migrate, createImage (snapshot), createBackup, restore, changePassword, add/removeSecurityGroup, forceDelete, os-resetState, os-resetNetwork, + 5 console actions (`os-getVNCConsole` and `os-getConsoleOutput` serve; `os-getSerialConsole` / `os-getSPICEConsole` / `os-getRDPConsole` refuse by design — see Remote consoles) |
| Server metadata | ✅ | get / set / replace |
| Server tags | ✅ | list / replace / delete-all / add / delete |
| Server diagnostics | ✅ | `GET /servers/{id}/diagnostics` |
| Instance actions (os-instance-actions) | ✅ | list / show |
| Remote consoles | ✅ | `POST /remote-consoles` and the legacy `os-getVNCConsole` action return a working noVNC URL at a configurable origin (`nova.console_proxy_base_url`), carrying a persisted, revocable token; the guest's VNC socket is read from the running libvirt domain per request. The console is a unix socket at the instance's own path, with no TCP listener, so the token-validating proxy is the only route in and a stale link can reach no other guest; every endpoint-moving operation (stop/start/reboot/suspend/resume/shelve/delete/forceDelete) revokes, and a sweep revokes links whose guest stopped or restarted outside the API. `GET /os-console-auth-tokens/{token}` is served for compatibility. Other types (`xvpvnc`, SPICE, RDP, interactive serial) are **refused** with a clear error rather than a fabricated URL, and stub mode refuses with 501. Serial console log (`os-getConsoleOutput`) works in real mode. Multi-node console is not supported in v1 (SPEC-021 / SPEC-018 F1, fixed). |
| Volume attachments | ⚠️ | list / show / attach / detach — no `PUT` (swap/update) |
| Interface attachments | ⚠️ | list / attach / detach — no show-one `GET …/os-interface/{port_id}` |
| Server migrations | ✅ | `os-migrations` list; per-server list / show / delete (abort) / action |
| Flavors (CRUD) | ✅ | list / detail / create / show / delete |
| Flavor extra-specs | ✅ | full CRUD |
| Flavor access (os-flavor-access) | ✅ | list + add/removeTenantAccess (via flavor action) |
| Keypairs | ✅ | list / create / show / delete |
| Limits | ✅ | `GET /limits` |
| Quota sets (os-quota-sets) | ✅ | get / update / defaults / **detail** (SPEC-020) |
| Usage (os-simple-tenant-usage) | ✅ | list / show |
| Availability zones | ✅ | list / detail |
| Hypervisors (os-hypervisors) | ✅ | list / detail / statistics (synthetic in stub mode) |
| Aggregates (os-aggregates) | ✅ | CRUD + action |
| Server groups (os-server-groups) | ✅ | list / create / show / delete |
| Images (Nova → Glance proxy) | ✅ | `GET /images`, `/images/detail` (for Horizon) |
| Services (os-services) | ⚠️ | `GET` list only — no enable / disable / force-down |
| Server password / Server IPs / Server topology | ❌ | see §8.2 |
| Quota class sets / Extensions / Assisted volume snapshots | ❌ | see §8.2 |
| External events / os-agents / audit-log / os-hosts | ❌ | see §8.2 |
| Nova-network proxies (os-networks, os-floating-ips, os-fixed-ips, os-security-groups) | ❌ | deprecated — superseded by Neutron (§8.2) |

**Contract tests** (`test/contract/nova/`, 26 files): README reports **77/82 (~94%)** passing; not CI-gated;
the RBAC test ships as `.broken`.

**Doc-vs-code discrepancies found:** the compat report states a microversion ceiling of **2.103** — the
code constant is **2.90**; and it lists **unrescue** as implemented — only **rescue** is wired. Its
"70/76" is a code-review estimate; the route-derived functional count is **71** (§2).

**Validation verdict:** Nova covers the entire compute surface the validation path exercises — full server
lifecycle and action set, flavors, keypairs, metadata, volume & interface attach, quotas, AZ,
server groups, aggregates, migrations. The **interactive remote console** works too as of SPEC-021: the
returned noVNC URL resolves a persisted, revocable token to the guest's own unix socket, verified in a
browser against a live framebuffer (✅ §3.2; SPEC-018 F1 fixed). Its one limit is that the token projection
is node-local, so multi-node console is unsupported in v1. The sc007 Terraform scenario (boot a server with keypair + user-data, attach a volume,
associate a floating IP) is fully supported. Every other ❌/⚠️ is admin / legacy / enterprise
(see [§8.2](#82-nova--not-implemented-endpoints)); none block validation.

### 3.3 Neutron — Network v2.0 (94 functional operations)

Lightstack covers the full L2/L3 + security surface plus many common extensions; the gaps are advanced /
enterprise networking extensions and some partial management surfaces.

| Neutron v2.0 group | Lightstack | Notes |
|---|:--:|---|
| Networks | ✅ | CRUD (PUT + PATCH) |
| Subnets | ✅ | CRUD (PUT + PATCH) |
| Ports | ✅ | CRUD (PUT + PATCH) |
| Security groups | ✅ | CRUD |
| Security group rules | ✅ | list / create / show / delete (rules are immutable upstream — no update, correct) |
| Routers | ✅ | CRUD + `add_router_interface` / `remove_router_interface` |
| Floating IPs | ✅ | CRUD |
| FIP port forwarding | ✅ | CRUD (nested under `/floatingips/{id}/port_forwardings`) |
| Subnet pools | ✅ | CRUD |
| Address scopes | ✅ | CRUD |
| RBAC policies | ✅ | CRUD |
| Trunks | ✅ | CRUD + `add_subports` / `remove_subports` |
| QoS policies | ✅ | CRUD |
| QoS rules | ⚠️ | **bandwidth-limit only** — no dscp-marking / minimum-bandwidth / minimum-packet-rate; no `GET /qos/rule-types` |
| Quotas | ⚠️ | **read-only**: `GET /quotas/{id}`, `/details`, **`/default`** (SPEC-020), plus their `.json` aliases — no update / reset / list. The limits are fixed code defaults (network 100, subnet 100, port 500, router 10, floatingip 50, security_group 100, security_group_rule 500); Neutron never reads the `quotas` table, so the network keys in Nova's `os-quota-sets` change only what Nova reports |
| Agents | ⚠️ | list / show + L3-agent-on-router (list, add) — no enable/disable, delete, remove-L3-agent, or DHCP-agent scheduling |
| Auto-allocated topology | ✅ | get / create / delete |
| Network IP availability | ✅ | list / show |
| Availability zones | ✅ | `GET /availability_zones` |
| Extensions | ✅ | `GET /extensions` |
| Service providers | ✅ | `GET /service-providers` |
| Metering (labels + rules) | ✅ | labels list/create/show/delete + label-rules list/create/delete |
| FWaaS v2 · VPNaaS · BGP · SFC · segments · logging · TaaS · flavors · … | ❌ | advanced / enterprise extensions — see §8.3 |

**Contract tests** (`test/contract/neutron/`, ~16 files): README reports **49/58 (~84%)** passing; not CI-gated.

**Doc-vs-code discrepancies found:** the compat report's **"92/94 (~98%)"** is measured against a narrow core
baseline — the route-derived functional count is **94**, but Neutron's *full* api-ref surface spans 40+
resource families (FWaaS, VPNaaS, BGP, SFC, segments, …), so real coverage of all of Neutron is far below
98%. The report also lists QoS **DSCP-marking** (code has only bandwidth-limit rules) and router
**extra-route add/update endpoints** (routes are set via the router `PUT`, no `add_extraroutes`).

**Validation verdict:** the entire networking surface the validation path exercises is ✅ — networks,
subnets, ports, security groups + rules, routers + interfaces, floating IPs, port forwarding. The sc007
Terraform scenario (network / subnet / router / router-interface / port / floating-IP + association) is
fully supported. Gaps are advanced networking extensions and partial quota/agent/QoS-rule management
(see [§8.3](#83-neutron--not-implemented-endpoints)); none block validation — note only that Neutron's
quota limits are fixed code defaults, and no API sets them.

### 3.4 Cinder — Block Storage v3 (67 functional operations)

> **Surface breadth ≠ tested depth — read this before the matrix.** Two in-repo sources disagree sharply:
> the compat report claims **65/68 (~96%)** by *code analysis* (route existence), while the README's own
> *contract-test* result is **4/22 (18%)** — *"Basic volumes work, groups need implementation."* Both are
> accurate for what they measure. Lightstack **registers** a broad Cinder surface (67 functional ops), but working,
> contract-verified behaviour is concentrated in **core volume / snapshot / type** operations. The core
> volume lifecycle *is* real and host-validated (SPEC-009: create, attach, detach, delete, boot-from-volume,
> volume-from-image, on local **and** RBD). Read the 96% as "routes present," not "features working."

| Cinder v3 group | Lightstack | Notes |
|---|:--:|---|
| Volumes (CRUD) | ✅ | list / detail / create / show / update (PUT + PATCH) / delete — **core lifecycle host-validated in real mode** |
| Volume actions (`/volumes/{id}/action`) | ⚠️ | attach / detach / extend / retype / reset-status / readonly / reimage / force-detach wired (verified in the dispatcher); migrate / revert-to-snapshot / upload-to-image / set-bootable absent |
| Volume metadata | ✅ | get / set + per-key get / update / delete |
| Snapshots | ✅ | list / detail / create / show / update / delete |
| Snapshot metadata | ✅ | get / set + per-key |
| Volume types | ✅ | CRUD + `default` + action (add/remove access) |
| Volume type extra-specs | ✅ | per-key CRUD |
| Volume type access | ✅ | list (+ add/remove via type action) |
| Quotas | ✅ | quota-sets get / update / delete / **defaults**, and usage through `quota-sets/{id}?usage=true` — Block Storage v3 has no `/detail` sub-resource — under both `quota-sets` and the upstream `os-quota-sets` spelling (SPEC-020) |
| Limits | ✅ | `GET /limits` |
| Availability zones | ✅ | `GET /os-availability-zone` |
| QoS specs | ⚠️ | CRUD registered; low contract coverage |
| Volume groups | ⚠️ | `/groups` CRUD + **`/groups/detail`**, at both the flat and project-scoped paths (SPEC-020); no group_snapshots / group_types / group action. `groups/detail` is **not optional**: python-cinderclient uses it for `groups.list(detailed=True)`, and Horizon's create-volume form calls that — its absence broke the Create Volume dialog (Horizon report F8) |
| Backups | ⚠️ | CRUD + action registered; no `export_record` / `import_record` |
| Volume transfers | ⚠️ | list / create / show / accept / delete registered; low contract coverage |
| Manage / unmanage | ⚠️ | `os-volume-manage`, `manageable_volumes`, `os-snapshot-manage` |
| Services | ⚠️ | `GET` list only |
| Attachments API (`/attachments`) · group_snapshots · group_types · default-types · capabilities · messages · clusters · snapshot actions · … | ❌ | see §8.4 |

**Contract tests** (`test/contract/cinder/`, ~18 files): README reports **4/22 (18%)** — the lowest per-service
signal in lightstack. Not CI-gated. Core volume/attach paths are separately covered by SPEC-009 host tests
(`ws5_attach_real`, `ws5_boot_from_volume`, `ws5_volume_from_image`) and `test/volume_attach_test.sh`.

**Doc-vs-code reconciliation:** the compat report's **65/68 (~96%)** counts *registered routes*; the README's
**4/22 (18%)** counts *passing contract tests*. This blueprint treats core volume + snapshot + type ops as
✅ (they work and are host-validated) and the advanced surface (groups, QoS specs, transfers, backups
export/import, attachments API, migrate) as ⚠️ registered-but-thin.

**Validation verdict:** the validation-critical surface is ✅ — `openstack_blockstorage_volume_v3` create
(sc007) and `openstack_compute_volume_attach_v2` attach (via Nova), plus the **#422 RBD volume lifecycle**
(create / attach / detach / delete), all host-validated in real mode. Backends: **local (qcow2)** and **RBD**
(behind the `ceph` build tag) + hybrid `local,rbd`; **no S3 backend for Cinder** (see §4). The advanced
surface is registered-but-thin (see [§8.4](#84-cinder--not-implemented-endpoints)) and off the validation
path.

### 3.5 Glance — Image v2 (38 functional operations)

Lightstack advertises **v2.9**. Version discovery (`GET /`, `GET /v2`) is in `main.go`; the service registers
routes without the `/v2` prefix (gophercloud appends it). Core image lifecycle is solid; the coverage gap
is concentrated in the metadefs sub-resources.

| Glance v2 group | Lightstack | Notes |
|---|:--:|---|
| Images (CRUD) | ✅ | list / create / show / update (PATCH) / delete |
| Image data | ✅ | `PUT /images/{id}/file` (upload) · `GET …/file` (download) — **validation-critical for #422 (RGW/S3 backend)** |
| Image import | ✅ | stage / import / get-import-info (web-download, glance-direct) |
| Image members (sharing) | ✅ | create / list / show / update / delete |
| Image tags | ✅ | add / delete |
| Image actions | ✅ | deactivate / reactivate |
| Schemas | ✅ | image / images / member / members |
| Tasks | ✅ | create / list / show |
| Info / stores | ✅ | `GET /stores`, `/stores/info` (lightstack paths; canonical is `/v2/info/stores` + `/v2/info/import`) |
| Cache | ✅ | list / queue (prefetch) / delete / clear |
| Metadefs | ⚠️ | namespaces CRUD + global `resource_types` list only — **no properties / objects / tags / namespace-RT associations** (~15 ops) |
| Multi-store data ops · metadefs sub-resources | ❌ | see §8.5 |

**Contract tests** (`test/contract/glance/`, 6 files): README reports **27/29 (93%)** — "image service fully
functional." Not CI-gated. This is a high contract signal despite the 72% *coverage* figure — i.e. what's
implemented works; what's missing is the metadefs catalog.

**Backends (richest of the five):** local, **RBD** (behind the `ceph` build tag), and **S3 — AWS / MinIO /
Ceph RGW** via `s3_endpoint` (path-style) — plus hybrids `local,rbd` / `local,s3` / `rbd,s3`. The S3/RGW
backend is validation-critical for **#422**; details in §4.

**Doc-vs-code:** the compat report's **38/53 (~72%)** matches the route-derived functional count (**38**) and
honestly names the metadefs-advanced gap — the most accurate of the five service sections in that report.

**Validation verdict:** the validation-critical surface is ✅ — image create + upload (`PUT …/file`) +
download (`GET …/file`) + list + delete, which is exactly what **#422** exercises against Ceph RGW/S3 and
what Nova consumes for boot-from-image. Metadefs sub-resources and multi-store operations
(see [§8.5](#85-glance--not-implemented-endpoints)) are off the validation path.

## 4. Storage backends

Lightstack persists two kinds of data through pluggable backends selected per service by `storage_mode`
(both default to `local`). The epic (#413) advertises "Ceph RBD, S3, and MinIO"; the honest per-service
reality — from `pkg/storage/image_store.go` (Glance) and `pkg/storage/ceph.go` + `ceph_rbd.go` (Cinder) — is:

| Backend | Glance (images) | Cinder (volumes) | Notes |
|---|:--:|:--:|---|
| `stub` | ✅ | ✅ | in-memory, no I/O — dev / unit tests |
| `local` | ✅ | ✅ | Glance: raw at `/var/lib/lightstack/images`; Cinder: qcow2 at `/var/lib/lightstack/volumes` |
| `rbd` (Ceph) | ⚠️ placeholder | ✅ *(build-tag)* | Cinder: real `go-ceph` (`ceph_rbd.go`, `//go:build ceph`); Glance: the `rbd` case exists but replication is log-only / not wired |
| `s3` (AWS / MinIO / Ceph RGW) | ✅ | ❌ | **Glance only** — AWS SDK v2, `s3_endpoint` + path-style for MinIO/RGW; optional TLS CA bundle / skip-verify (`s3_ca_bundle`, `s3_insecure_skip_verify`) + upload checksum (SPEC-014) |
| hybrid | `local,s3` ✅ · `local,rbd` ⚠️ · `rbd,s3` ❌ *(rejected)* | `local,rbd` ✅ | first backend = primary, rest = fallback/replica |

**The Ceph build-tag caveat — critical for #422.** RBD support lives in `pkg/storage/ceph_rbd.go` behind
`//go:build ceph`. **Neither `make build` (`Makefile:19`) nor the container image
(`build/package/Dockerfile:23`) passes `-tags ceph`** — so the **stock binary and stock chart image are
Ceph-less**, and a `storage_mode: rbd`/`local,rbd` config against them will not do real RBD I/O. Cinder-on-RBD
(a hard dependency of #422) therefore requires a **custom `go build -tags ceph`** with `librados`/`librbd`
present — and note the image is Alpine/musl, where cgo + go-ceph is notoriously fiddly. **Action item for
#422: ship a Ceph-enabled image variant.**

**Glance hybrid nuance.** Only `local` and `s3` (and `local,s3`, which replicates local→S3) are fully wired.
`local,rbd` uploads locally and merely *logs* the intended RBD copy. **`rbd,s3` is rejected at startup**
(and removed from the chart's `values.schema.json` enum): Glance's RBD backend is a placeholder in *every*
build, so the mode could never write the S3 replica its own download-fallback reads — it now fails fast
rather than silently promising redundancy. For #422's "Glance on Ceph RGW", use `storage_mode: s3` (or
`local,s3`) with `s3_endpoint` = the RGW endpoint — that path is real.

**Fail-fast timeouts** (consistent with §5): Cinder Ceph/RBD **1 s** (`ceph.go:65`); Glance S3 bounds
**connect + time-to-first-byte** (configurable via `s3_connect_timeout`, `config.go:101`) while the image
**body streams uncapped** so large images complete — SPEC-014 replaced the old flat 5 s total cap that
would abort big transfers. A slow or unreachable backend still errors quickly rather than hanging the
synchronous request.

**Mapping to #422 (Ceph RGW + RBD).**
- **Glance on Ceph RGW / MinIO** → `storage_mode: s3` (+ `s3_endpoint`, path-style). **Real, works today.**
- **Cinder on Ceph RBD** → `storage_mode: rbd` / `local,rbd`. **Real go-ceph, but only in a `ceph`-tagged build.**
- **Tenant object read/write** (also named in #422's scope) → **available via S3, not Swift**: Lightstack still
  exposes no Swift/`object-store` API, but SPEC-014 added the Keystone-side S3 auth bridge — EC2 credentials
  (`…/OS-EC2`) + public `POST /v3/s3tokens` — so tenants use **Ceph RGW's S3 API** as a per-project object
  store (RGW `implicit tenants=s3`), advertised under the `s3` catalog type. S3/RGW is therefore both a
  Glance image backend *and* a tenant object store; only the Swift protocol is absent (see §7 and
  [§8](#8-known-gaps--limitations)).

**VM disks (Nova).** Not a Cinder backend, but storage-adjacent: each instance gets a per-instance qcow2
**overlay over a shared raw base** under `/var/lib/lightstack` (SPEC-009); boot-from-volume instead routes the root
disk to a Cinder volume. Node-local storage (a `hostPath`, with subdirectories pre-created by the chart
initContainer per SPEC-011) — so local-disk instances are not live-migratable across nodes.

## 5. Synchronous architecture & trade-offs

Lightstack is a **single Go binary, one process**: `cmd/lightstack/main.go` starts one `http.Server` per service
(Keystone, Nova, Neutron, Cinder, Glance, Placement, Metadata, + health) as goroutines on distinct ports,
all sharing one `pgxpool` connection pool and one JWT auth middleware, with graceful shutdown. There is
**no message queue** — grep confirms no AMQP / RabbitMQ / oslo.messaging dependency in `go.mod`. Services
call each other in-process; cross-node coordination is done through PostgreSQL, not RPC.

**Synchronous by default — with one deliberate exception.** Volume create/attach, network/port/subnet
create, image upload, and floating-IP association all complete **in the request** (SPEC-009 made
block-storage attach synchronous per the constitution), so a `2xx` means the resource is usable — no task
to poll. The exception is **real-mode VM boot**: `CreateServer` inserts the instance as `BUILD`, spawns a
`WaitGroup`-tracked goroutine to do the libvirt define/create + network wiring, and returns `BUILD`
immediately; the goroutine flips it to `ACTIVE`/`ERROR` (`handlers.go:427`). This matches OpenStack's async
server-create contract that Terraform and the CLI already poll on — but it's an **in-process goroutine, not
a conductor + queue** (the `WaitGroup` lets graceful drain wait for in-flight boots — SPEC-013).

**Coordination via PostgreSQL, not RPC.** Multi-node networking uses a DB-polling VXLAN coordinator (~1 s)
and a node registry with heartbeats (~30 s) instead of agent RPC. Auth is **stateless HMAC-SHA256 JWT**
(24 h TTL, no token store). **Fail-fast timeouts** keep the synchronous path honest: Ceph/RBD 1 s, S3
connect + first-byte (configurable; image body uncapped for large transfers — SPEC-014), libvirt ~2 s
connect / ~30 s boot, DB health-check 30 s — a slow dependency errors quickly rather than hanging a request.

### Trade-offs vs. traditional (RabbitMQ/oslo.messaging) OpenStack

| Concern | Traditional OpenStack | Lightstack | Consequence |
|---|---|---|---|
| Messaging | RabbitMQ + oslo.messaging | none — in-process calls | no broker to operate; no distributed RPC layer |
| Task orchestration | Nova conductor / scheduler, cast+call | synchronous; one goroutine for VM boot | far simpler; **no retry/backoff, no task queue** |
| Neutron agents | separate L2 / L3 / DHCP agents over RPC | in-process netlink / iptables + DB-poll VXLAN | fewer moving parts; **no agent-level HA** |
| Cinder volume manager | `c-vol` over RPC | synchronous provisioning in-request | volume usable on return; **long ops block the call** |
| Identity tokens | Fernet / UUID + revocation events | stateless HMAC-JWT, short TTL | no token DB; **no revocation list** (§8.1) |
| Scale-out | scale each service independently | monolith; scale = more nodes (VXLAN) | **can't scale one service alone**; multi-node Helm deferred (#52) |
| Control-plane HA | N API workers behind a load balancer | single process | HA delegated to Kubernetes restart + readiness probes (SPEC-013) |
| Failure handling | async retry, eventual consistency | fail-fast (1–5 s), errors returned inline | **deterministic and testable**; no automatic retry |

**Why this suits the validation use case.** For sci-in-a-box, determinism beats scale: a synchronous API
means a test runner sees success/failure immediately (no polling task state, no broker flakiness), the
whole control plane is one restartable pod, and a clean deploy/destroy cycle (#421) has no queue state to
leak. The costs — no independent per-service scaling, no async retry, VM boot as the sole background step —
are acceptable for a single-node (or small multi-node) validation cloud, which is exactly lightstack's target.

## 6. Terraform coverage

lightstack's headline claim is "100 % Terraform compatible." The evidence is one curated module,
`test/terraform/sc007/main.tf` (SPEC-009 SC-007 / FR-018), which stands up a full single-node topology with
the **unmodified** OpenStack provider and asserts `terraform apply` + `destroy` both succeed. It exercises
**10 resource types + 2 data sources** (14 blocks):

| Terraform type | sc007 | Backing Lightstack API |
|---|:--:|---|
| `openstack_images_image_v2` *(data)* | ✅ | Glance |
| `openstack_networking_network_v2` / `subnet_v2` | ✅ | Neutron (tenant + external) |
| `openstack_networking_router_v2` / `router_interface_v2` | ✅ | Neutron L3 |
| `openstack_networking_floatingip_v2` / `floatingip_associate_v2` | ✅ | Neutron FIP |
| `openstack_networking_port_v2` *(data)* | ✅ | Neutron (port lookup — see deviation) |
| `openstack_compute_keypair_v2` | ✅ | Nova |
| `openstack_compute_instance_v2` | ✅ | Nova (+ `user_data` / cloud-init) |
| `openstack_compute_volume_attach_v2` | ✅ | Nova volume-attach |
| `openstack_blockstorage_volume_v3` | ✅ | Cinder |

**Documented deviation (the "100 %" asterisk).** lightstack's server detail **doesn't populate the per-interface
`port` id**, so the idiomatic `openstack_compute_instance_v2.network[0].port` is `null`. sc007 works around
it by resolving the port through a `data "openstack_networking_port_v2"` (filtered by `device_id`) and
feeding that into the floating-IP association (`main.tf:83-93`). So FIP-to-instance is **compatible with a
one-line workaround**, not literally zero-modification — worth stating plainly against the "unchanged
scripts" promise.

**What's tested, and what isn't.**
- **Real-mode only, not CI-gated** — sc007 runs against a live Lightstack on a Linux/k3s host (`test/terraform/README.md`), never in the pipeline. (That README also still lists `ws3/ws4/ws5` module dirs that no longer exist — only `sc007/` remains; stale.)
- It is a **smoke test of one topology**, not exhaustive coverage of the `openstack_*` provider.

**Coverage vs. #423's required smoke set.** #423 asks Terraform to cover *compute instances, networks,
security groups, volumes, object storage*:

| #423 category | Covered by sc007? | Note |
|---|:--:|---|
| Compute instances | ✅ | instance + keypair + volume-attach |
| Networks | ✅ | network / subnet / router / FIP |
| Volumes | ✅ | `blockstorage_volume_v3` (core Cinder) |
| Security groups | ❌ | **not in sc007** — the Neutron secgroup API exists (§3.3) but no `openstack_networking_secgroup_v2` is exercised |
| Object storage | ❌ | Terraform's `openstack_objectstorage_*` is **Swift**-based, which Lightstack doesn't expose; tenant object storage exists via **S3** (RGW + EC2 creds, §4 / §7) but not through this provider |

**Verdict.** For the compute + network + volume + image resources it covers, the drop-in Terraform claim
holds (modulo the port-id workaround), and it's validated end-to-end in real mode. But "100 %" is
overstated: two of #423's five smoke categories are unmet — **security groups** (API present, not yet
Terraform-tested → add an `openstack_networking_secgroup_v2` case) and **object storage** (no API → will
fail; drop it or track against the Swift decision, §7). Neither is CI-gated yet.

## 7. sci-in-a-box integration surface

**sci-in-a-box** is lightstack's primary consumer: it drives an instance's lifecycle **through Helm — not a CRD**.
The interface is a two-half contract, defined by the chart's `CONTRACT.md` + `values.schema.json` and the
SPEC-013 health endpoints.

> **Status.** These artifacts are now in-tree on `main`: the chart's `values.schema.json` + `CONTRACT.md`
> (SPEC-012 / #474) and the SPEC-013 (#475) health endpoints on the identity listener. #474's contract is
> still being completed against its acceptance criteria, but the schema and health signal described below
> are live.

**Input contract — `values.schema.json` (SPEC-012 / #474).** Every value is typed, bounded, and described,
and Helm validates it on `install` / `upgrade` / `lint` / `template`, **rejecting structurally invalid input
before any workload is created**. Unknown keys are rejected (`additionalProperties: false`), with Kubernetes
passthrough maps (`annotations`, `resources`, `nodeSelector`, `tolerations`, `affinity`) left open;
`image.repository` is required; render-time guards fail fast when `database.url` / `jwtSecret` /
`adminPassword` are absent without an `existingSecret`, or `teardown.kubectlImage` is unset under
`dataRetentionPolicy: delete`; unsafe cross-field combos (multi-node + real mode with `local` backends) are
rejected while benign capacity/topology preferences are advisory.

**Output / status contract (SPEC-012 / #474).** The orchestrator derives exactly one of
**Provisioning / Ready / Failed** from Helm release status + pod readiness (a documented deterministic
recipe: `helm status`, `kubectl rollout`/DaemonSet readiness, and terminal-failure detection like
`CrashLoopBackOff` / `ImagePullBackOff`), owning the bounded wait itself. Endpoints are resolved
Keystone-first, then discovered from the service catalog.

**Health signal (SPEC-013 / #475).** Lightstack exposes three unauthenticated endpoints on the identity listener
(`:35357`): `GET /livez` (liveness — process only, never touches the DB), `GET /readyz` (readiness — `200`
only when all listeners are bound, the DB `Ping` succeeds, and the pod isn't draining, and in real mode
once the console socket directory has been prepared, which takes the host's libvirt answering once
(SPEC-031); `503` with a short curated reason otherwise, bounded to <1 s), and `GET|HEAD /healthcheck` (an OpenStack-compatible,
content-negotiated alias of `/readyz`). The **machine-readable signal sci-in-a-box consumes is the
Kubernetes Pod `Ready` condition** (the aggregate of the readiness probe and its roll-ups) — *not* a bespoke
Lightstack status protocol. The chart wires `liveness`/`readiness`/`startup` probes accordingly.

**Lifecycle automation (#421).** Maps onto this contract directly: **deploy** = `helm install` with
schema-validated values → wait on Pod `Ready`; **populate** = seed users / projects / quotas (the binary
first-boot seed + the SPEC-012 seed hook, #473); **destroy** = `helm uninstall` + the SPEC-010 pre-delete
teardown hooks (blocking purge of host + network state) — **already in `main` via `#67`**.

### Remaining gaps for sci-in-a-box integration

| Gap | Impact | Lands / tracked via |
|---|---|---|
| SPEC-012 contract not fully complete | `values.schema.json`, `CONTRACT.md`, and the honest `/readyz` (SPEC-013) are in `main`; #474's remaining acceptance criteria are still open | #474 (SPEC-012) |
| **No Swift / `object-store` API** | Swift-protocol scenarios unservable; but tenant object read/write **is** servable via S3 (RGW + EC2 creds + `/v3/s3tokens`, SPEC-014) | Swift decision: lightstack#15 (§8) |
| No CI wiring Lightstack ↔ orchestrator | integration is validated manually / out-of-tree; contract + Terraform tests aren't CI-gated | #421 / #424 |
| Multi-node Helm deferred | only single-node deploys today; #421's multi-node configuration is unmet | #52 |
| Ceph-less stock image | Cinder-on-RBD (#422) needs a custom `-tags ceph` image variant | #422 (§4) |

## 8. Known gaps & limitations

This section collects (a) the **cross-cutting limitations** that shape whether Lightstack fits the validation use
case, and (b) per-service lists of canonical OpenStack endpoints Lightstack does not implement (§8.1–§8.5).

### Cross-cutting limitations

| Limitation | Detail | Impact / tracked |
|---|---|---|
| **No Swift / `object-store` API** | the catalog advertises `identity / compute / placement / network / volumev3 / image` + a non-standard **`s3`** (RGW) type (SPEC-014); tenants get object storage via **S3** (EC2 creds + `/v3/s3tokens` + RGW `implicit tenants`), but the **Swift** protocol is absent | S3 object read/write servable; Swift-only scenarios await the lightstack#15 decision |
| **Ceph behind a build tag** | `pkg/storage/ceph_rbd.go` is `//go:build ceph`; neither `make build` nor `build/package/Dockerfile` pass `-tags ceph`, so the stock binary/image are Ceph-less | Cinder-on-RBD (#422) needs a custom Ceph-enabled image (§4) |
| **Cinder advanced surface is thin** | ~96 % of routes registered but only ~18 % pass contract tests; groups / QoS specs / transfers / backup export-import are registered-but-thin | core volume ops are ✅ and host-validated; advanced ops unverified (§3.4) |
| **Multi-node Helm deferred** | the VXLAN overlay + node registry exist, but the chart deploys single-node only | #52; #421's multi-node configuration is unmet |
| **Tests not CI-gated** | the 308 gophercloud contract tests and the sc007 Terraform module need a live instance and don't run in CI | coverage figures are point-in-time; #423 asks for a CI smoke |
| **Placement is stub-only** | returns empty lists so Horizon's panels load; no resource-provider / allocation logic | adequate for validation; not a real scheduler |
| **Terraform "100 %" is qualified** | one topology (sc007), real-mode / not-CI, plus the port-id workaround; security groups untested and object storage impossible | §6; two of #423's five smoke categories unmet |
| **SPEC-012 contract in progress** | `CONTRACT.md`, `values.schema.json`, and the honest `/readyz` (SPEC-013 / #475) are in `main`; #474's remaining acceptance criteria are still being completed | §7 |

**On the headline numbers.** Lightstack implements **~335 functional operations** across the five core services
(§2). The repo's "104 % (342/330)" counted *raw routes* — URL aliases + version-discovery — against a
self-selected 330-endpoint baseline; the epic's "~70 % complete" (#413) is a holistic assessment weighted by
the thin Cinder advanced surface, the absent Swift API, and the metadefs/extension gaps. Both are true of
what they measure — this blueprint reports honest per-resource coverage and the gaps above rather than a
single percentage.

### 8.1 Keystone — not-implemented endpoints

> **Scope note.** None of the endpoints below are on the sci-in-a-box / Terraform / Horizon validation
> path — they are enterprise SSO, delegation, policy, quota-limit, and tagging features. They are listed
> for completeness against the OpenStack Identity v3 api-ref (2025.2). The one *in-scope* Keystone defect
> is a **bug**, not a missing endpoint: catalog URL-template substitution (see §3.1).

The table lists every canonical Identity v3 operation Lightstack does not implement, with **Impl.**
(⚠️ group partially implemented · ❌ group absent) and the reason it is unimplemented. Multiple
endpoints per group are separated by a line break (`<br>`).

| Identity v3 group | Impl. | Not-implemented endpoint(s) | Justification |
|---|:--:|---|---|
| Authentication & token mgmt | ⚠️ | `HEAD /v3/auth/tokens`<br>`GET /v3/auth/catalog`<br>`GET /v3/auth/domains`<br>`GET /v3/auth/system`<br>`GET, HEAD /v3/auth/receipts` | Lightstack validates tokens via `GET /v3/auth/tokens` and embeds the catalog in the token-issue response; the separate check / catalog / domains / system discovery calls aren't required by Horizon, Terraform, or the CLI against lightstack. MFA receipts assume a multi-step auth-plugin chain the single HMAC-JWT issuer doesn't model. |
| Service catalog & endpoints | ⚠️ | `GET /v3/endpoints/{endpoint_id}`<br>`PATCH /v3/endpoints/{endpoint_id}` | The catalog is seeded at bootstrap and read through the token; per-endpoint show/update is an admin catalog-editing convenience the validation clients never call. |
| Domain configuration | ⚠️ | `PUT, PATCH, DELETE /v3/domains/{id}/config`<br>`GET /v3/domains/config/default`<br>`…/config/{group}` (GET / PATCH / DELETE)<br>`…/config/{group}/{option}` (GET / PATCH / DELETE) | Per-domain config drives multi-backend Keystone (per-domain LDAP / identity drivers). Lightstack is single-backend (PostgreSQL + HMAC-JWT), so writable domain config has no effect; the read-only `GET …/config` exists only for Horizon's identity panel. |
| Role assignments (advanced) | ⚠️ | `HEAD /v3/projects/{pid}/users/{uid}/roles/{rid}` (check)<br>group-on-project, user/group-on-domain (assign / list / check)<br>implied roles `…/roles/{id}/implies/{id}`, `GET /v3/role_inferences` | lightstack's RBAC is the project-scoped user→role model Terraform / Horizon / CLI use. Group- and domain-scoped grants and role-inference trees are advanced RBAC the validation scenarios don't exercise. Because no domain-level grant exists, SPEC-023 makes a **domain-scoped token request an explicit `400`** rather than the silent role-less `201` it used to be. |
| Project tags | ❌ | `GET, PUT, DELETE /v3/projects/{pid}/tags`<br>`GET, PUT, DELETE /v3/projects/{pid}/tags/{value}` | Project tagging is a metadata / search convenience; no validation scenario filters projects by tag. |
| Regions | ❌ | `GET, POST /v3/regions`<br>`GET, PUT, PATCH, DELETE /v3/regions/{id}` | Lightstack is a single-region deployment; multi-region catalog topology is out of scope. |
| Policies | ❌ | `GET, POST /v3/policies`<br>`GET, PATCH, DELETE /v3/policies/{id}` | The policy API stores / distributes RBAC policy blobs; Lightstack enforces authorization in-code (role checks in middleware), so there is no policy document to serve. De-emphasized upstream. |
| System role assignments | ❌ | `GET, PUT, HEAD, DELETE /v3/system/users/{uid}/roles/{rid}` (+ list)<br>`…/system/groups/{gid}/roles/{rid}` (+ list) | System-scoped roles govern deployment-wide cloud-admin operations; Lightstack has no system scope. Deployment-wide operations are gated on the `admin` role instead — since SPEC-023 that includes host aggregates, the deployment-wide migration list and the aggregate usage report, which previously had no gate at all. |
| Unified limits | ❌ | `GET, POST /v3/registered_limits`, `GET, PATCH, DELETE /v3/registered_limits/{id}`<br>`GET, POST /v3/limits`, `GET, PATCH, DELETE /v3/limits/{id}`, `GET /v3/limits/model` | Lightstack keeps quotas per service — Nova and Cinder quota sets, Neutron fixed defaults — and enforces only Nova's instances, cores and RAM, not via Keystone's centralized unified-limits model; the validation path uses the per-service quota APIs. |
| OS-INHERIT | ❌ | `PUT, HEAD, DELETE …/roles/{rid}/inherited_to_projects` (+ `GET …/roles/inherited_to_projects`; domain- & project-scoped, user & group) | Inherited (domain→project) role assignments are an advanced RBAC convenience; lightstack's flat project-scoped model has no inheritance. |
| OS-TRUST | ❌ | `GET, POST /v3/OS-TRUST/trusts`<br>`GET, DELETE /v3/OS-TRUST/trusts/{id}` (+ trust-roles) | Trusts delegate roles for unattended / Heat-style workflows; not used by the validation scenarios. |
| OS-FEDERATION | ❌ | identity providers · protocols · mappings · service providers · `GET /v3/OS-FEDERATION/projects` · `/domains` · SAML2/ECP auth (entire extension) | Federated identity (external IdPs / SAML / OIDC) is enterprise SSO; Lightstack authenticates locally against its own user store. |
| OS-OAUTH1 / OS-OAUTH2 | ❌ | consumers, request / access tokens, authorization; OAuth2 token + discovery (entire extensions) | OAuth delegation flows are enterprise integrations outside the validation scope. |
| OS-EP-FILTER | ❌ | endpoint groups CRUD + project↔endpoint / project↔endpoint-group associations | Endpoint filtering tailors the catalog per project; Lightstack serves a single flat catalog. |
| OS-ENDPOINT-POLICY | ❌ | policy↔endpoint / policy↔service associations | Associates policy blobs with endpoints; depends on the (absent) policy API. |
| OS-REVOKE | ❌ | `GET /v3/OS-REVOKE/events` | Revocation-event lists invalidate cached tokens; Lightstack tokens are stateless short-TTL JWTs validated by signature, so there is no revocation list to publish (`DELETE /v3/auth/tokens` still works locally). |
| OS-SIMPLE-CERT *(deprecated)* | ❌ | `GET /v3/OS-SIMPLE-CERT/ca`<br>`GET /v3/OS-SIMPLE-CERT/certificates` | Served signing certs for the deprecated PKI token format; obsolete — Lightstack uses HMAC-JWT. |
| OS-PKI *(deprecated)* | ❌ | revoked-token list | Deprecated PKI token revocation list; obsolete. |

### 8.2 Nova — not-implemented endpoints

> **Scope note.** None of the below are on the validation path. The sc007 Terraform scenario and Horizon
> exercise the server lifecycle + action set, flavors, keypairs, metadata, volume/interface attach,
> quotas and AZ — all ✅ in §3.2, including the interactive remote console since SPEC-021 (SPEC-018 F1
> fixed). The gaps are admin, legacy nova-network, or enterprise features.
> Multiple endpoints per group are separated by `<br>`.

| Compute v2.1 group | Impl. | Not-implemented endpoint(s) | Justification |
|---|:--:|---|---|
| Server actions | ⚠️ | `trigger_crash_dump`<br>`injectNetworkInfo`<br>`unrescue`<br>legacy `addFloatingIp` / `removeFloatingIp` / `addFixedIp` / `removeFixedIp` | The action route covers the full lifecycle; crash-dump and network-info inject are admin/debug, `unrescue` is unwired (rescue works), and the floating/fixed-IP actions are deprecated nova-network proxies superseded by Neutron. |
| Volume attachments | ⚠️ | `PUT /servers/{id}/os-volume_attachments/{id}` | Volume swap/update is an advanced (live-migration-adjacent) op; attach + detach cover the validation workflow. |
| Interface attachments | ⚠️ | `GET /servers/{id}/os-interface/{port_id}` | List + attach + detach cover the workflow; per-port show is a minor convenience. |
| Services management | ⚠️ | `PUT /os-services/{enable,disable,force-down}` | Single-node has no compute-service fleet to enable/disable; `GET /os-services` (list) suffices. |
| Server password | ❌ | `GET, DELETE /servers/{id}/os-server-password` | Returns the encrypted admin password set at boot (Windows get-password flow); Lightstack injects SSH keys via cloud-init and the validation path uses keypairs, not password retrieval. |
| Server IPs | ❌ | `GET /servers/{id}/ips`<br>`GET /servers/{id}/ips/{network}` | Addresses are already returned inline in server show/detail; the standalone IPs sub-resource is redundant. |
| Server topology | ❌ | `GET /servers/{id}/topology` | NUMA / CPU-pinning introspection for perf tuning; out of scope for single-node validation. |
| Quota class sets | ❌ | `GET, PUT /os-quota-class-sets/{id}` | Class-level default quotas across projects; Lightstack sets per-project quotas via `os-quota-sets`. |
| Extensions | ❌ | `GET /extensions`<br>`GET /extensions/{alias}` | Legacy extension-discovery list; modern clients negotiate via microversions. Horizon tolerates its absence. |
| Assisted volume snapshots | ❌ | `POST, DELETE /os-assisted-volume-snapshots` | Cinder-orchestrated snapshot assist for specific backends; not used by the validation path. |
| Server external events | ❌ | `POST /os-server-external-events` | Lets Neutron notify Nova of port/network status; lightstack's synchronous in-process model updates state directly, so no callback channel is needed. |
| os-agents | ❌ | `GET, POST, PUT, DELETE /os-agents` | Guest-agent build registry (xen-era); unused. |
| Instance usage audit log | ❌ | `GET /os-instance_usage_audit_log` | Billing / audit-log aggregation; out of scope. |
| os-hosts *(deprecated)* | ❌ | `GET, PUT /os-hosts/{host}` | Deprecated host power / maintenance management. |
| Nova-network proxies *(deprecated)* | ❌ | `os-networks` · `os-floating-ips` · `os-floating-ip-pools` · `os-fixed-ips` · `os-security-groups` | Deprecated Nova proxies to the old nova-network; superseded by Neutron, which Lightstack implements directly. |

### 8.3 Neutron — not-implemented endpoints

> **Scope note.** The core L2/L3 + security surface (networks, subnets, ports, security groups + rules,
> routers, floating IPs, port forwarding) is fully ✅ in §3.3 and covers the sc007 Terraform scenario and
> Horizon networking. Below are partial management surfaces (⚠️) and advanced / enterprise extensions (❌).
> Multiple endpoints per group are separated by `<br>`.

| Neutron v2.0 group | Impl. | Not-implemented endpoint(s) | Justification |
|---|:--:|---|---|
| QoS rules | ⚠️ | `dscp_marking_rules`<br>`minimum_bandwidth_rules`<br>`minimum_packet_rate_rules`<br>`GET /qos/rule-types` | Only bandwidth-limit shaping is wired; the other rule types target DSCP marking / SR-IOV min-bandwidth / packet-rate QoS, none used by the validation scenarios. |
| Quotas | ⚠️ | `PUT /quotas/{id}` (update)<br>`DELETE /quotas/{id}` (reset)<br>`GET /quotas` (list) | Neutron quota is read-only in lightstack and reports fixed code defaults. Nova's `os-quota-sets` carries network keys (networks, subnets, ports, routers, security groups), but Neutron never reads them, so a value written there changes only what Nova reports — no API sets Neutron's limits. `GET /quotas/{id}/default` **was** missing and is now served (SPEC-020) — Horizon's default-quota views read it; the Overview widget reads `/details`. |
| Agents | ⚠️ | `PUT /agents/{id}` (enable/disable)<br>`DELETE /agents/{id}`<br>`DELETE /routers/{id}/l3-agents/{agent_id}`<br>DHCP-agent scheduling (`/agents/{id}/dhcp-networks`, `/networks/{id}/dhcp-agents`) | Single-node has no agent fleet to schedule or drain; list + show + L3-agent-add satisfy Horizon's agent panel. |
| Routers | ⚠️ | `PUT /routers/{id}/add_extraroutes`<br>`PUT /routers/{id}/remove_extraroutes` | Extra routes are set via the router `PUT` (`routes` attribute); the atomic add/remove is a concurrency convenience. |
| FWaaS v2 | ❌ | `/fwaas/firewall_groups`<br>`/fwaas/firewall_policies`<br>`/fwaas/firewall_rules` | Firewall-as-a-Service; tenant traffic filtering is handled by security groups (✅), which is what the validation path uses. |
| VPNaaS | ❌ | `/vpn/vpnservices` · `/ikepolicies` · `/ipsecpolicies` · `/ipsec-site-connections` · `/endpoint-groups` | Site-to-site IPsec VPN; not used by the validation scenarios. |
| BGP / BGP-VPN | ❌ | `/bgp-speakers` · `/bgp-peers` · `/bgpvpn/*` | Dynamic routing / L3VPN for multi-site fabrics; out of scope for single-node. |
| Service Function Chaining | ❌ | `/sfc/port_chains` · `/port_pair_groups` · `/port_pairs` · `/flow_classifiers` | NFV traffic steering; enterprise-only. |
| Network segments | ❌ | `/segments` | Multi-segment (routed / L2) provider networks; lightstack uses flat + VXLAN tenant networks. |
| **External networks (model gap, not an endpoint)** | ❌ | *n/a — `router:external` is hard-coded `false`* | Every network object reports `router:external: false` (`internal/neutron/network.go:527,709,814`) and `GET /v2.0/networks?router:external=true` returns `[]` by design (`networkListDecision`, `network.go:563`). North-south egress is instead reached by attaching a router gateway to an ordinary network — `PUT /routers/{id}` accepts `external_gateway_info` (`router.go:68`), which is how the SPEC-014 runbook wires it. **Consequence for the dashboard**: Horizon's router *Set Gateway* dialog lists candidates from exactly that filter, so it is always empty and egress cannot be configured from the UI (Horizon report F11). This is an attribute-level gap, which is why it does not appear as a missing endpoint above. |
| Logging | ❌ | `/log/logs` · `/log/loggable-resources` | Security-group / FWaaS packet-flow logging; not used. |
| Tap-as-a-Service | ❌ | `/taas/tap_services` · `/taas/tap_flows` | Port mirroring for IDS / monitoring; out of scope. |
| Neutron flavors + service profiles | ❌ | `/flavors` · `/service_profiles` | The Neutron flavor framework selects pluggable L3 / LB service drivers; Lightstack ships a single built-in L3 implementation. |
| Local IPs · NDP proxies · conntrack helpers · port bindings · address groups · default SG rules · L2 gateway | ❌ | `/local_ips` · `/ndp_proxies` · `/ports/{id}/conntrack_helpers` · `/ports/{id}/bindings` · `/address-groups` · `/default-security-group-rules` · `/l2-gateways` | Niche / newer extensions (node-local IP, IPv6 NDP proxy, ALG helpers, multi-binding for live-migration, named CIDR groups, SG-rule templates, VXLAN↔VLAN bridging); none on the validation path. |

### 8.4 Cinder — not-implemented endpoints

> **Scope note.** Core volume + snapshot + type operations are ✅ in §3.4 and host-validated (SPEC-009);
> the validation path (`openstack_blockstorage_volume_v3` + Nova-side attach, and the #422 RBD lifecycle)
> is fully covered. Below are the ⚠️ registered-but-thin surfaces and ❌ missing groups behind the low
> contract signal (README 4/22). Multiple endpoints per group are separated by `<br>`.

| Cinder v3 group | Impl. | Not-implemented endpoint(s) | Justification |
|---|:--:|---|---|
| Volume actions | ⚠️ | `os-migrate_volume`<br>`os-migrate_volume_completion`<br>`revert` (revert-to-snapshot)<br>`os-volume_upload_image`<br>`os-set_bootable` | Core attach / detach / extend / retype / reset-status / readonly / reimage / force-detach are wired; migration, revert-to-snapshot, upload-to-image and set-bootable target multi-backend or image-pipeline flows off the validation path. |
| Volume groups | ⚠️ | `POST /groups/{id}/action` (+ group replication) | `/groups` CRUD and `/groups/detail` are registered (SPEC-020); consistency-group actions are absent. `groups/detail` must be *registered*: without it gin matches `detail` as a group id and the handler 500s — how Horizon report F8 arose. Registration order is irrelevant; gin's static segment wins over the wildcard either way. |
| Backups | ⚠️ | `GET /backups/{id}/export_record`<br>`POST /backups/import_record` | Backup-metadata export/import for DR portability; the validation path doesn't back up volumes. |
| Services | ⚠️ | `PUT /os-services/{enable,disable}` | Single-node has no storage-service fleet to enable/disable. |
| Attachments API *(v3.27+)* | ❌ | `GET, POST /attachments`<br>`GET, PUT, DELETE /attachments/{id}`<br>`POST /attachments/{id}/action` | The microversioned Cinder attachments resource; Lightstack attaches volumes through Nova's `os-volume_attachments` (✅), which is what Terraform and Horizon use. |
| Group snapshots | ❌ | `/group_snapshots` (+ action) | Consistency-group snapshots; the volume-groups feature itself is thin. |
| Group types | ❌ | `/group_types`<br>`/group_types/{id}/group_specs` | Types for volume groups; groups are thin. |
| Default volume types | ❌ | `GET, PUT, DELETE /default-types` | Per-project default-type override (3.62+); Lightstack uses a single default type. |
| Snapshot actions | ❌ | `POST /snapshots/{id}/action` | reset-status / force-delete / update-status admin actions; core snapshot CRUD suffices. |
| Capabilities | ❌ | `GET /capabilities/{host}` | Backend capability discovery for the scheduler; single-backend deployment. |
| Messages | ❌ | `GET, DELETE /messages` | Async user-facing fault messages; Lightstack is synchronous — errors are returned inline. |
| Clusters / Workers | ❌ | `/clusters` · `/workers/cleanup` | Active/active HA service-cluster management; single-node. |
| Quota class sets · resource filters · manageable snapshots | ❌ | `/os-quota-class-sets/{id}` · `/resource_filters` · `/manageable_snapshots` | Class-default quotas (per-project quota-sets used instead), filterable-attribute discovery, adopt-existing-snapshot listing — niche. **`GET /v3/extensions` is now implemented** (SPEC-020): Horizon's create-volume form probes it via `extension_supported`; without it the form still opens, but shows "Unable to determine if availability zones extension is supported." and offers no availability-zone choice. |

### 8.5 Glance — not-implemented endpoints

> **Scope note.** Core image lifecycle (create, upload, download, list, delete, members, tags, actions) is
> ✅ in §3.5 and contract-verified (README 27/29). #422's image upload/download against Ceph RGW/S3 is
> covered. The gaps are the metadefs catalog and multi-store operations. Multiple endpoints per group are
> separated by `<br>`.

| Glance v2 group | Impl. | Not-implemented endpoint(s) | Justification |
|---|:--:|---|---|
| Metadefs | ⚠️ | `/metadefs/namespaces/{ns}/properties` (CRUD)<br>`/metadefs/namespaces/{ns}/objects` (CRUD)<br>`/metadefs/namespaces/{ns}/tags` (CRUD)<br>`/metadefs/namespaces/{ns}/resource_types` (associate / disassociate) | Namespaces + the global `resource_types` list are present; the property / object / tag sub-resources are a metadata-schema catalog (<5% upstream adoption) that no validation scenario reads. |
| Multi-store data ops | ❌ | `POST /images/{id}/import` with `copy-image` + `stores`<br>`DELETE /stores/{store_id}/{image_id}` (delete-from-store) | Per-store placement / copy / delete for multi-store Glance; Lightstack uses a single active backend (or hybrid failover), so per-store operations don't apply. |
| Info discovery paths | ⚠️ | canonical `GET /v2/info/stores`<br>`GET /v2/info/import` | Lightstack serves the same data at `GET /stores` + `/stores/info`; only the path differs (clients hard-coding `/v2/info/…` won't find it). |
