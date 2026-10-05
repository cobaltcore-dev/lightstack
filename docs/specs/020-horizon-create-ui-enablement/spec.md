# Feature Specification: Horizon compatibility — create/write UI enablement (F2, F3, F7, F8)

**Feature Branch**: `feature/533` (feature id: `020-horizon-create-ui-enablement`)
**Created**: 2026-07-24
**Status**: Draft
**Input**: cobaltcore-dev/cloud-storage#533 — "o3k — Horizon compat: create/write UI enablement (F2, F3, F7, F8)" (sub-issue of #531, step 2 of 3; parent epic #423). Grounded in the SPEC-018 verification report [`docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`](../../compatibility/HORIZON_COMPATIBILITY_REPORT.md).

## Context

The SPEC-018 Horizon verification (feature/482) found Lightstack is a drop-in for the OpenStack **API/CLI**, but the Horizon **dashboard is not usable for resource management**: read/list panels render, yet every **create/write** workflow through the UI fails. It logged nine findings (F1–F9). The self-contained backend/response defects (F4, F5, F6, F9) were fixed in the sibling ticket #532 / SPEC-019 and are **already present in this branch's base** (merged to `main` as `0a4dde0`). This feature closes the **create/write-UI cluster** — the four findings that share a single suspected root:

- **F7** (major) — Launch Instance is **not completable via the dashboard**: no "Launch Instance" action on the Instances panel, and the wizard's Networks step renders no selectable rows.
- **F8** (major, umbrella) — Horizon's resource-**creation** UIs are broken while the APIs work; its concrete, not-otherwise-covered symptom is the **Create Volume modal opening nothing**. F8 closes when F2/F3/F7 and the volume-create modal do.
- **F2** (medium) — Project → Overview shows **"Unable to retrieve limits information"**.
- **F3** (medium) — **No "Create Network" action** in Project → Network → Networks (only the Admin panel has it).

**The suspected common root** (from the report's F8 analysis, and now grounded in a code map of the branch): Horizon's create flows load supporting **quota + limits + capability + type** metadata up front, and **disable create/launch actions, blank the Overview widget, and refuse to open modals** when that metadata can't be fetched as expected. Most of the relevant endpoints already exist in Lightstack; the defects are **gaps and inconsistencies** in that metadata surface (missing quota `detail`/`default` variants, limit values that disagree between endpoints, and network attributes the launch wizard needs) rather than wholly-absent services. The ticket's guidance is explicit: **trace the calls each create UI makes first — one fix likely re-enables several.**

The APIs themselves work (proven in the SPEC-018 real pass): via the CLI a network, a volume, and a server all create successfully, and a real libvirt VM boots from a Ceph-RGW image with working networking and SSH. This feature is about making the **dashboard** reach that same working backend.

## Clarifications

### Session 2026-07-24

- Q: Which environments must the four create/write workflows be tested/verified in? → A: **Both stub mode and real mode**, reusing the existing setup scripts and charts (no new harness). Each of F2/F3/F7/F8 must be observed fixed in **both**:
  - **Stub** — Horizon + Lightstack (stub) via `deployments/docker-compose-horizon.yml`, per the SPEC-018 stub runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md`).
  - **Real** — **Lightstack on k3s** via the `charts/lightstack` Helm chart (real mode); **Horizon installed with Helm** via `deployments/horizon-chart/`; and **Ceph + RGW (S3) provided by Rook on minikube** via `deployments/rook/setup-rook-rgw.sh`, per the SPEC-018 real runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md`).
- Q: What is the fix boundary — may the shipped Horizon config/policy be changed, or must fixes be Lightstack-side against stock Horizon? → A: **Lightstack-side only, against stock/unpatched Horizon.** Fixes live in Lightstack's API responses; the SPEC-018 Horizon deployment keeps its default policies and config. Any workflow that cannot be enabled without patching Horizon's own config/policy is recorded as a documented finding, not worked around — stock Horizon is the drop-in compatibility bar.
- Q: What authz posture should the new/modified quota endpoints (the `detail`/`default` variants) enforce on the URL project id? → A: **Scope reads to the caller's project; a token with the `admin` role may query any project.** A non-admin caller receives only their own project's quota (a URL project id that doesn't match the token is never answered with another project's data), matching upstream OpenStack and the established JWT project-scope + `roles` idiom. Horizon's self-project responses are unchanged; this only closes cross-project reads on the endpoints this feature touches. *(Refined 2026-10-02: an explicit foreign target is a `403`, see FR-002a.)*
- Q: How should the disagreeing Nova compute limits be reconciled (which values, which source)? → A: **Reconcile to the upstream defaults (instances/cores/RAM = 10/20/51200), single source = the `quotas` table** (matching code defaults as fallback for projects without a row). `GetLimits` reads the same source as the quota-set, so the limits view and quota-set agree and a per-project quota override reflects in both. The inflated `GetLimits` values (100/200/512000) are dropped.
- Q: For F3/SC-003 ("a project member can create a network"), verify as the seeded admin on the Project dashboard, or provision a non-admin member? → A: **Verify as the seeded `admin` user acting in the Project dashboard** (a member of the `default` project) — no new fixture. Upstream Neutron's default `create_network` policy is `rule:admin_or_project_member` (project scope; confirmed via Context7 `/openstack/neutron`), so stock Horizon already permits a member to create a network — the F3 defect is the **network-quota fetch failing**, not policy. The variable is the *panel* (Project vs Admin), not the role; re-enabling the Project-panel Create Network action proves the fix.

### Session 2026-07-27

- Q: How far should the role-check unification onto the existing shared helper reach? → A: **Repo-wide, and drop the redundant wrapper.** Every hand-rolled admin-role loop is replaced by the existing shared helper `common.HasRole` (`internal/common/roles.go`), which already documents itself as "the single shared role-check helper for all services": the four hand-rolled loops (`internal/nova/quotas.go:166`, `internal/nova/advanced_actions.go:478` and `:1224`, `internal/cinder/volumes.go:980`) **and** Keystone's pass-through wrapper `hasRole` (`internal/keystone/handlers.go:147`, 5 call sites) are collapsed into direct `common.HasRole` calls. New Neutron code uses it from the start. Behavior-preserving; the only edge-case change is Cinder's `os-reset_status`, where a malformed (non-`[]string`) roles claim now yields 403 instead of 500 — unreachable in practice, since the auth middleware always sets `[]string`.
- Q: Cinder's quota `PUT`/`DELETE` have no authorization at all — close that here, and fix Nova's check ordering? → A: **Yes to both.** `PUT`/`DELETE /v3/quota-sets/{id}` become **admin-only**, matching Nova's existing behavior and upstream Cinder — today any authenticated caller can rewrite or reset **any** project's quota, a cross-project write hole in code this feature already edits. Additionally, Nova's `UpdateQuotaSet` currently binds the request body *before* checking the admin role, so a non-admin sending a malformed body gets 400 instead of 403; the admin check moves first. Both are deliberate, documented, tested behavior changes (an exception to FR-016's "unchanged behavior", recorded in FR-020).

### Session 2026-10-05

- Q: The launch wizard's default sends the image as a `source_type: image`, `destination_type: volume` root mapping. Keep adopting it as an image boot, which accepts `volume_size` and `delete_on_termination` and discards them, or create the volume? → A: **Create the volume, as upstream Nova does (FR-025).** Accepting both fields and ignoring them is the silent downgrade upstream never does. A request that cannot get its volume is refused (`400`, or `503` when the volume cannot be created) and never falls back to an image boot. The volume is unnamed, as upstream's `_create_volume` leaves it (`nova/virt/block_device.py`).
- Q: Nova and Cinder used any image id they were given, so a project that knew the id of another project's private image could boot it or copy it into a volume. Refuse only an existing image the caller cannot see, or also an id that names no image, which stub mode accepted? → A: **Both, as upstream, on every path that takes an image (FR-026)**: server create (`imageRef` and the root mapping), rebuild, volume create from an image, and `os-reimage`. Upstream asks Glance with the caller's token, so the two cases are one answer. The contract suite and five shell scripts booted from `00000000-0000-0000-0000-000000000001`, which names no image; they now create the image they boot from.
- Q: Glance's own show, list and download ignored image sharing, and an image that was not active could still be booted or copied into a volume. Fix both here? → A: **Yes (FR-026).** Glance reads follow upstream's visibility rules, the list with its `visibility`, `member_status` and `status` filters, and every boot and volume-from-image path refuses an image that is not active, as upstream does.
- Q: CI never ran the regression probe, and the probe only read what Horizon checks before it enables a create control, so SC-001 – SC-004 were never gated. Gate them how? → A: **The probe replays the create requests Horizon sends, and CI runs it (FR-028).** The browser pass stays the manual check in the runbooks; an automated browser suite remains out of scope.
- Q: Volume attachments reported a device hardcoded to `/dev/vdb`, a boot volume included. Fix only the device, or the attachment reporting as a whole? → A: **As a whole (FR-027)**: upstream's attachment object, the attached volumes on server show and detail, and `os-attach` recording the mountpoint it is given.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Launch an instance end-to-end from the dashboard (Priority: P1)

A project user opens Horizon, goes to Project → Compute → Instances, clicks **Launch Instance**, and completes the wizard — choosing an image, a flavor, and a **network** — and the instance boots. Today this cannot be done through the UI: the Instances panel has **no Launch Instance action**, and even when the wizard is reached (via Images → Launch) its **Networks step renders no selectable rows**, so the required step never validates and Launch stays disabled. Every one of these operations already succeeds via `openstack server create`.

**Why this priority**: Launching an instance is the single most important cloud workflow and the ticket's flagship acceptance item. It also exercises the deepest part of the shared root (compute quota/limits for the button, plus network selectability for the wizard), so getting it working validates the core fix.

**Independent Test**: From the Instances panel, use the Launch Instance action; in the wizard select an image, a flavor, and one of the project's networks (a selectable row appears); launch. The instance reaches ACTIVE and appears in the Instances panel — matching what `openstack server create` already produces.

**Acceptance Scenarios**:

1. **Given** a project with at least one network, image, and flavor, **When** the Instances panel is opened, **Then** a working **Launch Instance** action is present (not hidden).
2. **Given** the launch wizard's Networks step, **When** it is opened, **Then** the project's network(s) render as **selectable rows** and a network can be chosen, allowing the step to validate.
3. **Given** all required wizard steps are completed, **When** Launch is submitted, **Then** the instance is created, reaches ACTIVE, and appears in the Instances panel.
4. **Given** the same inputs, **When** an instance is created via `openstack server create`, **Then** it succeeds identically (the API path is unaffected — no regression).

---

### User Story 2 - Create a volume from the dashboard (Priority: P2)

A project user opens Project → Volumes and clicks **Create Volume**. Today the button **opens no dialog** — the create modal cannot load the supporting metadata it needs (volume types, availability, quota/limits), so nothing happens. The user expects the dialog to open, to pick a name/size (and optionally a volume type), and to create a volume that then appears in the list (the list itself already renders after the #532 F9 fix).

**Why this priority**: Volume creation is a core resource-management workflow and the concrete, not-otherwise-covered symptom of the F8 umbrella. It is self-contained (Cinder metadata only) and independently testable.

**Independent Test**: From Project → Volumes, click Create Volume; the dialog opens populated with volume-type and availability options; create a small volume; it appears in the Volumes panel.

**Acceptance Scenarios**:

1. **Given** the Volumes panel, **When** Create Volume is clicked, **Then** the create dialog **opens** (today it opens nothing), populated with the metadata it loads (volume type(s), availability).
2. **Given** the open dialog, **When** a name and size are submitted, **Then** the volume is created and appears in the Volumes panel.
3. **Given** a volume created via the dialog, **When** the Volumes panel is reloaded, **Then** it lists the volume without error (consistent with the #532 F9 fix).

---

### User Story 3 - Create a network from the Project panel (Priority: P2)

A project user opens Project → Network → Networks and expects a **Create Network** action, then creates a network (and its subnet). Today the action appears **only in the Admin panel**, not the standard Project panel that is the ticket's named workflow location — Horizon hides it because the network capability/quota metadata it checks isn't served as expected.

**Why this priority**: Self-service network creation is a named acceptance workflow. It is medium severity (an Admin can still create networks), but it is required for the dashboard to be a resource-management drop-in.

**Independent Test**: As the seeded `admin` user (a member of the `default` project) on the **Project** dashboard, open Project → Network → Networks; the **Create Network** action is present; create a network with a subnet; it appears in the list exactly once (consistent with the #532 F4 dedup).

**Acceptance Scenarios**:

1. **Given** a project member on Project → Network → Networks, **When** the panel loads, **Then** a **Create Network** action is present (not only in the Admin panel).
2. **Given** the create flow, **When** a network and subnet are submitted, **Then** they are created and the network appears in the list **exactly once**.
3. **Given** the network was created through the Project panel, **When** it is later used in the launch wizard (US1), **Then** it is selectable there.

---

### User Story 4 - Project Overview limits widget loads (Priority: P3)

A user opens Project → Overview. Today the usage/limits widget shows **"Unable to retrieve limits information"**. The user expects the widget to render the project's compute, block-storage, and network usage against its limits — including after switching projects.

**Why this priority**: The Overview widget is the **visible tip** of the shared root and the simplest signal that the quota/limits aggregation is healthy, but it has the lowest standalone user value (an informational panel), so it is sequenced last.

**Independent Test**: Open Project → Overview; the usage/limits panel renders (no "Unable to retrieve limits" error) and shows compute, block-storage, and network usage for the current project.

**Acceptance Scenarios**:

1. **Given** the Project → Overview page, **When** it loads, **Then** the usage/limits widget renders without the "Unable to retrieve limits information" error.
2. **Given** the widget renders, **Then** it shows compute (instances/cores/RAM), block-storage (volumes/storage), and network usage for the current project.
3. **Given** more than one project is scoped to the user, **When** the project is switched, **Then** the widget reloads for the newly-scoped project without error.

---

### Edge Cases

- **Project with zero resources**: usage reads as 0 against the limits, and create/launch actions are **enabled** (headroom present) rather than disabled.
- **Project at or over a resource limit**: the corresponding create/launch action is disabled with Horizon's standard "quota exceeded" affordance — **not** a crash or a blank panel (matching OpenStack).
- **Project with no explicitly-seeded quota rows**: falls back to Lightstack's default limits (only one seed project is pre-populated today), and the metadata still resolves successfully rather than erroring.
- **Quota endpoint variant with a suffix or extra path segment** (e.g. a `detail`/`details`/`default`/`defaults` form, including any client-appended `.json`): served successfully wherever upstream defines it, not a 404 — Nova's `detail` and `defaults`, Neutron's `details` and `default` (with or without `.json`), Cinder's `defaults`. Cinder has no `detail`; its usage view is `?usage=true` (FR-002 amendment).
- **Absolute limits reported by two different endpoints** (the limits view vs. the quota-set view) for the same resource: **agree** (today they disagree for compute).
- **Network with no subnet**: the launch wizard behaves consistently with OpenStack; the standard create-network flow creates a subnet so launchable networks have one.
- **Project switch on Overview**: the widget re-queries for the newly-scoped project and does not carry stale or errored state.
- **Admin vs. member**: the Create Network action is available to a project **member** on the Project panel, not only to an admin.

## Requirements *(mandatory)*

### Functional Requirements

**Shared root — quota / limits metadata completeness & consistency (F2/F3/F7/F8)**

- **FR-001**: The per-project quota and limits metadata that Horizon's create/launch flows read before enabling actions MUST all resolve successfully (HTTP 200, well-formed) — the compute (Nova) limits and quota, the block-storage (Cinder) limits and quota, and the network (Neutron) quota — so that a failure in any one does not blank the Overview widget or disable a create/launch action.
- **FR-002**: The quota endpoint **variants** that Horizon (and standard OpenStack clients) request MUST be served rather than returning 404 — the `detail`/`details` and `default`/`defaults` forms of the compute, block-storage, and network quota, **each in the spelling its own service actually defines**. Additionally, the **network** quota MUST also be served at its `.json`-suffixed form, which is a Neutron client convention; the compute and block-storage clients do not append `.json`, so those variants are only added if the trace (FR-014) shows they are requested. *(Grounded gaps in this branch's base: Neutron `GET /v2.0/quotas/{project}/default` is unimplemented and its details route is registered without a `.json` form; Nova `GET /os-quota-sets/{project}/detail` is unimplemented; Cinder's usage view and `/defaults` are unimplemented.)*
  - **Amended 2026-07-29 (block storage):** this requirement originally named Cinder `GET /v3/quota-sets/{project}/detail`, carried over from Nova's spelling. That endpoint **does not exist** in the Block Storage v3 API: it exposes show / update / delete / `/defaults`, and surfaces usage through the **`usage=true` query parameter** on the show route ("Show project's quota usage information. Default is `false`"). Nova's `/detail` and Neutron's `.json` are real endpoints of their services, so the trace's live `404`s (research.md G2, G5) are genuine gaps. Cinder's `/detail` is not: research.md G9 records a `404` for it too, but that 404 answers **the probe's own synthetic request** — no client can ask for a path the Block Storage API does not define. *(Wording corrected 2026-08-26: an earlier draft of this amendment said the gap was "never observed", which misread G9's trace-verdict row.)* **Cinder's usage view is therefore `GET /v3/{,os-}quota-sets/{project}?usage=true`, and no `/detail` route is registered for block storage.** `/defaults` is upstream and stays.
- **FR-002a**: The quota/limits endpoints this feature adds or modifies MUST **scope reads to the caller's project**.
  - A non-admin caller may read only their own project's quota. An explicit URL project id naming another project is a `403`, as upstream's quota policies give it: Nova `PROJECT_READER_OR_ADMIN`, Cinder `SYSTEM_READER_OR_PROJECT_READER`, Neutron `get_quota`. The refusal comes before any lookup, so it reveals nothing about which projects exist.
  - A caller holding the `admin` role may query any project.
  - The empty and `default` forms mean the caller's own project.
  - Nova's `defaults` stays readable by anyone, as upstream's `RULE_ANY` allows.
  - Horizon's self-project responses are unchanged (FR-016). This closes the current cross-project read on the endpoints being touched.
  - *(Amended 2026-10-02 after review r4148510409. Until then, a foreign target was answered with the caller's own quotas.)*
- **FR-003**: The absolute limits a project sees MUST be internally consistent across the endpoints that report them — the limits view and the quota-set view MUST agree for the same resource. The disagreeing compute limits MUST be reconciled to the **upstream default values (instances/cores/RAM = 10/20/51200)**, with the **`quotas` table as the single source** (matching code defaults used when a project has no row) so that the limits view reads the same source as the quota-set and an admin's per-project override reflects in both. *(Today compute limits report 100/200/512000 via the limits endpoint but 10/20/51200 via the quota-set; the inflated limits-endpoint values are dropped.)*
- **FR-004**: Reported quota **usage** MUST reflect the project's actual consumption (counts/sums of instances, cores, RAM, volumes, storage, networks, subnets, ports, etc.), so Horizon computes real headroom and enables create actions when the project is under limit.

**F2 — Overview limits widget**

- **FR-005**: The Project → Overview usage/limits widget MUST load without the "Unable to retrieve limits information" error and MUST render the current project's compute, block-storage, and network usage — on first load and after a project switch.

**F7 — Launch instance via the dashboard**

- **FR-006**: The Instances panel MUST present a working **Launch Instance** action (not hidden), gated by the compute quota/limits metadata Horizon checks; from it the launch wizard MUST be reachable and completable.
- **FR-007**: The launch wizard's Networks step MUST render the project's networks as **selectable rows**, so a network can be chosen and the step validates. The network responses MUST carry every attribute the wizard needs to render and select a row — ownership (project scope), status, administrative state, and associated subnets. *(The deduplicated, filter-honoring network list from #532 / F4 is the input to this step; this requirement ensures the surviving rows are actually selectable.)*
- **FR-008**: An instance launched through the wizard MUST be created, reach ACTIVE, and appear in the Instances panel — matching what `openstack server create` already achieves via the API.

**F3 — Create network from the Project panel**

- **FR-009**: The Project → Network → Networks panel MUST present a **Create Network** action to a project member (not only in the Admin panel), gated by the network quota/capability metadata Horizon checks (network quota available, and the required Neutron capabilities advertised). *(Upstream Neutron's default `create_network` policy is `rule:admin_or_project_member`, so stock Horizon already permits a member; the action is hidden today because the network-quota fetch fails. This is a quota-metadata fix, not a policy change — consistent with FR-018.)*
- **FR-010**: Creating a network and its subnet through the Project panel MUST succeed, and the new network MUST appear in the list **exactly once** (consistent with the #532 F4 dedup) and be selectable in the launch wizard (FR-007).

**F8 — Create volume modal**

- **FR-011**: The Project → Volumes **Create Volume** action MUST open its dialog (today it opens nothing), populated with the supporting metadata the modal loads — volume type(s), availability, and quota/limits. *(The metadata was necessary but not the reason the dialog stayed shut — that was the cinder group route, **FR-021**. This requirement states the outcome; FR-021 states the root.)*
- **FR-012**: Creating a volume through the dialog MUST succeed and the volume MUST appear in the Volumes panel (which already renders after the #532 F9 fix).

**Neutron capability advertisement**

- **FR-013**: Neutron's advertised extension/capability list MUST include every capability Horizon requires to render the Project network create/management controls; any alias Horizon gates a control on MUST be present, and only aliases Lightstack genuinely implements may be advertised.
  - **Amended 2026-07-29 — the trace has closed, so the set is now named rather than open-ended** (research.md, *Scope decisions closed by this trace*). The confirmed set is:
    - **Neutron `GET /v2.0/extensions`** MUST advertise exactly **seven** aliases: the six already present (`security-group`, `router`, `port-security`, `binding`, `provider`, `quotas`) **plus `quota_details`**, which is the only addition the trace implicated.
    - **Cinder** MUST serve a **`GET /v3/extensions`** (it had none at all) advertising **`AvailabilityZones`** (alias `os-availability-zone`), the capability Horizon's Create Volume dialog gates on.
  - No other alias is implicated, in either service. Anything further would be speculative surface under FR-014's trace-first mandate.

**Investigation-first (per the ticket)**

- **FR-014**: The fix MUST be driven by **tracing the actual Nova/Cinder/Neutron calls Horizon issues** when opening each create UI (Overview load, Launch wizard, Create Network, Create Volume) — using the Horizon container/apache logs per the SPEC-018 runbooks — so the change targets the real gaps rather than assumed ones. The grounded gaps above are the starting hypothesis, not an assumed-complete list.

**Cross-cutting**

- **FR-015**: Each re-enabled workflow MUST be covered by an automated test that fails before the fix and passes after, runnable in the existing CI test job, so the defect cannot silently regress.
- **FR-016**: All other existing API behavior and response fields for the affected services (Nova, Neutron, Cinder) MUST remain unchanged; the existing unit and integration suites MUST continue to pass (no regression, including the API/CLI paths that already work). *Documented exceptions, each deliberate and tested:* the reconciled compute limit values (FR-003) — with Nova's `maxSecurityGroups` following the same defaults (50 → 10) and Cinder's `/limits` now read from `cinder_quotas` (`maxTotalVolumes` and `maxTotalSnapshots` 1000 → 100, `maxTotalBackupGigabytes` 5000 → 10000) — a failed usage count answering `500` rather than reporting zero usage, every instance row counted in Nova's usage (`DELETED` included), Neutron listings without `limit` returning every row instead of stopping at 1000, `remote_group_id` validated (`400` when malformed, `404` when not the caller's group) instead of stored unchecked, security-group rules scoped to their group's project (`404` for another project's group or rule, and for an unknown rule id on delete), network and image cache keys scoped to the caller, the eBPF rule translation failing closed, Nova's `addSecurityGroup` answering `400` instead of `409` for a group the server already has (as upstream), the quota-write authorization and Nova check-ordering fix (FR-020), the one unreachable role-claim edge case in FR-019, an unscoped token on Nova's and Cinder's `GET /limits` — it names no project, so it is now answered `404` as the quota routes answer it, rather than `200` with code defaults (raised in PR #79 review) — a root image-to-volume block-device mapping creating a volume instead of booting the image directly, with `400`/`503` where it was accepted before (FR-025), an image the caller cannot read, that does not exist, or that is not active, refused on server create, rebuild, volume create and `os-reimage`, and Glance's show, list and download following upstream's sharing rules (FR-026), the attachment reporting and the `os-attach`/`os-detach` checks of FR-027, and a network's `tenant_id`/`project_id` and a subnet's `tenant_id` in list and show, which now name the owning project rather than the caller, who was reported as the owner of every foreign network or subnet they could see (moved from cloud-storage#546 B11 and cloud-storage#545 on PR #79 review). The client-facing list, with before → after values, is in `docs/UPGRADING.md`.
- **FR-017**: Each of the four workflows MUST be tested/verified in **both** operating modes, reusing the existing setup scripts and charts (no new harness); each corresponding finding's panel/behavior MUST be observed fixed in both:
  - **Stub mode** — Horizon + Lightstack (stub) via `deployments/docker-compose-horizon.yml`, following the SPEC-018 stub runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md`).
  - **Real mode** — **Lightstack on k3s** via the `charts/lightstack` Helm chart (real mode), **Horizon installed with Helm** via `deployments/horizon-chart/`, and **Ceph + RGW (S3) provided by Rook on minikube** via `deployments/rook/setup-rook-rgw.sh`, following the SPEC-018 real runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md`).

**Authorization consistency & hardening**

- **FR-019**: The admin/role check MUST be performed through the **single existing shared helper**
  (`common.HasRole`) everywhere in the codebase. All hand-rolled role loops MUST be replaced by it — four
  sites (`internal/nova/quotas.go:166`, `internal/nova/advanced_actions.go:478` and `:1224`,
  `internal/cinder/volumes.go:980`) — **and** Keystone's redundant pass-through wrapper `hasRole`
  (`internal/keystone/handlers.go:147`) MUST be removed in favor of direct `common.HasRole` calls at its 5 call
  sites. **Additionally** — found during the branch review — `middleware.RequireRole`
  (`internal/middleware/auth.go`) was a **sixth** hand-rolled loop, with an unchecked `roles.([]string)`
  assertion and **zero call sites** repo-wide; it MUST be **deleted** rather than converted, since converting
  it would leave unused exported API. That deletion is what makes SC-007's "no hand-rolled copies left in the
  codebase" literally true. New code (including Neutron, which has no role check today) MUST use the helper
  rather than reintroduce a copy. The replacement MUST be behavior-preserving apart from one unreachable edge
  case:
  Cinder's `os-reset_status` returns 403 rather than 500 for a malformed non-`[]string` roles claim (the auth
  middleware always sets `[]string`).
- **FR-020**: Quota **write** endpoints MUST be authorized. `PUT` and `DELETE /v3/quota-sets/{project_id}`
  MUST require the `admin` role — today they have **no** authorization check at all, so any authenticated
  caller can rewrite or reset any project's quota (a cross-project write hole). This matches Nova's existing
  admin-only quota update and upstream Cinder. Additionally, Nova's `UpdateQuotaSet` MUST perform the admin
  check **before** binding the request body, so a non-admin receives 403 rather than 400 for a malformed body.
  These are deliberate behavior changes — an explicit exception to FR-016 — and MUST be covered by tests.

**Roots found during implementation (added after the host passes; see the corrected Assumption below)**

These four were not predicted by the plan. Each is the actual root of a finding the metadata work did
not fix, each was reproduced live before being fixed, and each carries a regression test per FR-015.

- **FR-021** *(F8's real root)*: Cinder MUST serve `GET /v3/groups/detail`, and the group routes MUST also be
  registered under `/v3/{project_id}`. Until it was served, `detail` fell through to `groups/:id`, which
  matched it as a group id and answered 500 — which is what made **Create Volume open nothing**:
  python-cinderclient's `groups.list(detailed=True)` requests that path, and Horizon's
  `CreateForm._populate_group_choices` converts any `group_list` error into a redirect, so the modal returned
  an empty 200 with `X-Horizon-Location` and the button looked inert, with no traceback anywhere.
  *Tests:* `internal/cinder/groups_test.go`; route-table guard in `quotas_test.go`.
- **FR-022** *(F7)*: Every security group in `GET /v2.0/security-groups` MUST carry a
  `security_group_rules` array and both `project_id`/`tenant_id`. Horizon reads this through openstacksdk,
  whose `Resource.to_dict()` emits the key unconditionally, so an omitted array arrives as **null** and
  `SecurityGroup.__init__`'s missing-key guard does not catch it — the request died with
  `'NoneType' object is not iterable`, emptying the launch wizard's security-group step.
  *Test:* `internal/neutron/security_group_shape_test.go`.
- **FR-023** *(F7)*: Every security-group **rule** object MUST carry the full key set — `protocol`,
  `port_range_min`, `port_range_max`, `remote_ip_prefix`, `remote_group_id` — **null-valued when the column
  is NULL**, as upstream Neutron does, on all three emitters (nested in the group list, the flat
  `security-group-rules` list, and by id). Horizon dereferences these as `sgr['...']`, so an absent key is a
  `KeyError` → HTTP 500 `"'protocol'"`; the seeded default group's egress rules have a NULL protocol, so it
  fires on a stock project. *Tests:* `requiredRuleKeys` in `internal/neutron/security_group_shape_test.go`,
  asserted against all three emitters.
- **FR-024** *(F7)*: `POST /v2.1/servers` MUST resolve `security_groups[].name` by **id or name**. The field
  is spelled "name", but with Neutron as the network backend upstream Nova matches each entry against both,
  and Horizon's launch wizard relies on it — `setFinalSpecSecurityGroups` pushes `securityGroup.id` whenever
  Neutron is enabled. Matching on name alone rejected every dashboard launch with
  `400 Security group <uuid> not found.` once the security-group list stopped failing.
  *Test:* `internal/nova/security_group_ref_test.go`.

**Added in review**

- **FR-025** *(F7, F16)*: A root `block_device_mapping_v2` entry with `source_type: image` and
  `destination_type: volume`, which is what the launch wizard sends with *Create New Volume* on (its
  default), MUST create a Cinder volume from the image and boot the server from it, as upstream Nova does.
  - **Size.** `volume_size` sets the size: an integer from 1 to 2^31-1, as a number or a string of digits,
    as upstream's request schema accepts; anything else is `400`. When it is absent the flavor's disk
    applies. Either way the size MUST be at least the image's floor, max(`min_disk`, the image size rounded
    up to whole GiB), else `400` before any volume exists.
  - **Lifetime.** `delete_on_termination` decides whether the volume is deleted with the server or released.
  - **Refusals.** An unknown image is `400 Can not find requested image`, upstream's answer. A deployment
    without a block-storage service answers `400`, and a volume that cannot be created answers `503`.
    Neither may fall back to booting the image directly. A create that fails after its volume exists
    removes the volume.
  - The same entry without `destination_type: volume` asks for no volume and is still adopted as the boot
    image.
  - **Image base.** A volume created from an image on a node-local backend (`local`, `local,rbd`) MUST first
    fetch the image's raw base onto the node, verified against the image checksum, as an image boot does. An
    RGW/S3-backed image's base is otherwise absent there, which also failed `volume create --image`.

  *Tests:* `internal/nova/boot_volume_test.go`, `TestBootSourcePrecedence` in
  `internal/nova/boot_from_volume_test.go`, `internal/cinder/boot_volume_test.go`.
- **FR-026**: Every API that makes a server or a volume from an image MUST refuse an image the caller cannot
  read, by Glance's rule for reading one image (`glance/db/utils.py` `is_image_visible`): an administrator reads
  every image; anyone reads a public or community image, or one with no owner; the owner reads its own; a project
  a shared image was shared with reads it, whatever the membership's status. An id that names no image, or is
  not a UUID, is refused the same way, so a refusal does not tell which. An image the caller can read but
  that is not active (its data not uploaded yet, or deactivated) MUST be refused too. The checks run before
  anything is reserved or created. Upstream's answers, which these use:
  - server create (`imageRef`, an adopted root image mapping, a new boot volume's image): `400 Can not find
    requested image`, or `400 Image <id> is not active.`;
  - rebuild: `400 Cannot find image for rebuild`, or `400 Image <id> is not active.`;
  - volume create from an image: `400 Invalid image identifier or unable to access requested image.`, or
    `400 Invalid input received: Image <id> is not active.`;
  - `os-reimage`: `404 Image <id> could not be found.`, or `400 Invalid input received: Image <id> is not active.`

  Glance applies the same rule to image show and download. Its list follows upstream's narrower rule
  (`glance/db/sqlalchemy/api.py` `_select_images_query`, `_image_get_all`): an administrator's list starts from
  every image; anyone else's from public and community images, their own, and shared images whose membership
  has the requested `member_status` (`accepted` unless asked). `?visibility=` keeps one visibility, or `all`
  keeps everything; without it, community images are left out unless they are the caller's own. `?status=`
  keeps one status, which is how the launch wizard asks for active images only. An invalid `visibility` or
  `member_status` is a `400`, with upstream's message. A change to an image's members evicts its cached reads.

  *Tests:* `internal/common/image_test.go`, `internal/nova/image_visibility_test.go`,
  `internal/cinder/image_visibility_test.go`, `internal/glance/visibility_test.go`,
  `internal/glance/images_cache_test.go`.
- **FR-027**: Volume attachments MUST read back as upstream reports them.
  - Cinder volume show and detail: each attachment is `{id, attachment_id, volume_id, server_id, host_name,
    device, attached_at}` (`id` is the volume's id, as upstream; `host_name` is null), with the device the volume
    was attached at: `/dev/vda` for a boot volume. It was `/dev/vdb` for every attachment.
  - Nova server show and detail: `os-extended-volumes:volumes_attached` lists `{id, delete_on_termination}`
    for every attached volume, the boot volume included, and is an empty list when there is none.
  - `os-attach` requires the `mountpoint` and records it; the server must be one of the caller's (any server
    for an administrator) and the volume available. `os-detach` and `os-force_detach` remove the attachment.
    An unknown volume is a `404` on all three.

  *Tests:* `internal/cinder/attachments_test.go`, `internal/nova/volumes_attached_test.go`.
- **FR-028**: SC-001 – SC-004 MUST be gated in CI at the API level. `make test-horizon-ui`
  (`test/horizon_create_ui_test.sh`) checks what Horizon reads before it enables each create control, then
  replays the requests Horizon stable/2025.2 sends, in the shape it builds them:
  - SC-002: a volume create with python-cinderclient's body; the volume becomes available and is listed once.
  - SC-003: a network, then its subnet; listed once, owned by the project, the subnet resolving.
  - SC-001: a launch on the wizard's defaults (no `imageRef`, a new boot volume that outlives the server, the
    network by id, the security group by id); `ACTIVE`, listed, an address on the network, the boot volume
    attached at `/dev/vda`, then deleted and the volume released.
  - SC-004: the Overview's limit reads with a token scoped to a second project.

  The probe removes everything it creates, on failure too. CI's integration job runs it on the Compose stack
  after the quick tests. It does not drive Horizon itself: the browser pass in the runbooks remains the manual
  check (an automated browser suite is out of scope).

**Fix boundary**

- **FR-018**: All fixes MUST be **Lightstack-side** (changes to Lightstack's API responses/handlers); the SPEC-018 Horizon deployment MUST remain **stock/unpatched** — default policies and config, no overrides added to force a control to appear. A workflow that cannot be enabled without modifying Horizon's own config/policy MUST be recorded as a documented finding rather than worked around; passing against **stock Horizon** is the drop-in compatibility bar.
  - **FR-018a** *(added after the first stub pass; corrected after the real pass)*: "Stock" bars **overrides that force a control to appear** — policy relaxations, feature toggles, and workarounds that hide an Lightstack gap. It does **not** bar correcting an outright **misconfiguration in Lightstack's own deployment artifacts** (`deployments/horizon-config/*` and its generated copy under `deployments/horizon-chart/files/`) that breaks Horizon against *any* backend. Such a correction MUST be justified against upstream Horizon behaviour and MUST NOT compensate for a missing Lightstack API. The one instance in this feature is `CSRF_COOKIE_HTTPONLY`, which Lightstack's `local_settings` set to `True`: Horizon's AngularJS layer reads the CSRF token from `document.cookie` (`framework.module.js`: `xsrfCookieName = 'csrftoken'`) with no DOM fallback, so a write on that path is sent with no `X-CSRFToken` and Django rejects it. Upstream Horizon does not set it; Django documents it as offering no practical protection.
    - **The effect is selective, not total.** Measured in real mode 2026-07-29 with `HttpOnly` still `True`: `POST /api/nova/keypairs/` returned **201** (import *and* generate), and the Angular launch wizard's `POST /api/nova/servers/` **succeeded** (instance reached ACTIVE) — but `DELETE /api/nova/keypairs/{name}` produced Django's `Forbidden (CSRF token missing.)` and the panel silently did nothing. So some Angular paths obtain the token another way. An earlier draft of this requirement claimed *every* Angular write was rejected; that was inferred from a `curl` probe rather than observed, and is wrong.
    - **Both copies must be changed together.** `deployments/horizon-chart/files/local_settings` is generated by `make horizon-chart-sync`; real mode reads **that** file. The first fix touched only the compose copy, so real mode still ran with `True` — which is exactly how the Key Pairs delete failure was found. Any change under `deployments/horizon-config/` MUST be followed by that target.

### Key Entities *(include if feature involves data)*

- **Quota / limits metadata**: the per-project resource limits and current usage that Horizon's create flows read before enabling actions — spanning compute (instances/cores/RAM/keypairs), block-storage (volumes/snapshots/storage), and network (networks/subnets/ports/routers/floating IPs/security groups). Must be complete (all `detail`/`default` variants served), internally consistent (limits agree across endpoints), and reflect real usage.
- **Volume type**: the block-storage type(s) the Create Volume dialog loads to populate its options.
- **Selectable network**: a Neutron network as the launch wizard consumes it — carrying the attributes the wizard needs to render and select a row (project scope, status, administrative state, associated subnets).
- **Neutron capability list**: the advertised extension aliases Horizon checks to decide which network controls (e.g. Create Network) to render.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A user can launch an instance **end-to-end from the dashboard** — the Launch Instance action is present, the wizard's Networks step offers a selectable network, and the launched instance reaches ACTIVE and appears in the Instances panel — where today the action is absent and the wizard cannot select a network.
- **SC-002**: A user can create a volume from the dashboard — the Create Volume dialog **opens** (populated with type/availability options) and the created volume appears in the Volumes list — where today the button opens nothing.
- **SC-003**: A project **member** (verified as the seeded `admin` user acting in the Project dashboard) can create a network and subnet from Project → Network → Networks — the Create Network action is present and the network is created and listed exactly once — where today the action appears only in the Admin panel.
- **SC-004**: The Project → Overview usage/limits widget loads and shows compute, block-storage, and network usage without the "Unable to retrieve limits information" error, including after a project switch.
- **SC-005**: Every quota/limits endpoint that Horizon's project-usage aggregation calls returns success (no 404/500), and the absolute limits reported for a resource agree across the limits and quota-set views.
- **SC-006**: All four workflows are observed working in **both** the stub pass (`deployments/docker-compose-horizon.yml` Horizon) and the real pass (**Lightstack on k3s** via `charts/lightstack` real mode + **Horizon via Helm** `deployments/horizon-chart/` + **Ceph/RGW via Rook on minikube** `deployments/rook/setup-rook-rgw.sh`), re-run from the SPEC-018 runbooks; the existing unit/integration test suites still pass (no regression).
- **SC-007**: No cross-project quota access remains on any quota endpoint: a non-admin cannot read another project's quota (FR-002a) and cannot write or reset it (FR-020, where today any authenticated caller can); an admin can do both. The admin/role check resolves to a **single** shared implementation with no hand-rolled copies left in the codebase (FR-019).
- **SC-007a**: A quota target that does not exist is a **404 on every service** — Nova, Neutron and Cinder alike — for an unknown-but-well-formed UUID, the nil UUID, and a non-UUID string; and project resolution resolves to a **single** shared implementation (`common.ResolveProjectRef`), with no per-package copies. Rationale: none of the three validated its target. Nova and Cinder used an admin's URL id verbatim and Neutron ignored it — so a phantom UUID was answered with a full **200** quota set of code defaults for a project that had never existed, and in Nova a non-UUID reached the `uuid`-typed `quotas.project_id` column and returned **500**. Writes must never store a quota row against a nonexistent project (Nova's `PUT` previously hit the `projects` foreign key and 500'd). A **non-admin** naming a foreign project is still silently scoped to their own project rather than 404'd, so the endpoint cannot be used to probe which projects exist (FR-002a).

## Assumptions

- **~~One shared root.~~ FALSIFIED during implementation — three roots, not one.** The plan assumed all four findings shared a quota/limits/capability metadata root (the ticket's "one fix likely re-enables several"), and that completing that read surface would re-enable all four. Host tracing disproved it. What held and what did not:
  - **F2 and F3 — quota-metadata root, as predicted.** The missing quota `detail`/`default` variants, the compute-limit inconsistency, Neutron's `/quotas/{project}/default` and `.json`-suffixed details, and the launch-wizard network attributes were all real and are FR-001…FR-014.
  - **F8 — route-registration root.** Nothing to do with metadata: `GET /v3/groups/detail` was never registered, so `groups/:id` matched `detail` and cinderclient's `groups.list(detailed=True)` got a 500 and Horizon's `CreateForm._populate_group_choices` aborted the modal with a redirect (FR-021).
  - **F7 — response-shape and request-parsing roots, three of them.** A null `security_group_rules` array, then a `protocol` KeyError once that array existed, then security groups arriving as ids rather than names (FR-022, FR-023, FR-024). Plus the boot-from-new-volume BDM: first adopted as an image boot, then implemented as a real volume in review (FR-025).

  The lesson is recorded because it shaped the work: each fix only became visible once its predecessor was fixed, since Horizon abandons a request at the first failure. A single-root assumption made the effort look smaller than it was.
- **UI-enablement, not quota enforcement.** Lightstack already exposes quota/limits endpoints and a compute-side quota check, plus `quotas`/`cinder_quotas` tables. This feature **completes and reconciles the read surface** Horizon needs; it does **not** add new hard-enforcement of quotas on create paths. Existing default limit values are retained **except** where two views disagreed: the Nova compute limits are reconciled to the upstream defaults (10/20/51200) sourced from the `quotas` table (FR-003), Nova's `maxSecurityGroups` follows the same defaults (50 → 10), and Cinder's `/limits` now reads `cinder_quotas` (`maxTotalVolumes` and `maxTotalSnapshots` 1000 → 100, `maxTotalBackupGigabytes` 5000 → 10000); values are otherwise immaterial provided usage < limit so actions enable.
- **Network selectability is a data-shape outcome resolved during implementation.** The network list already carries subnets, status, administrative state and `tenant_id`; the fix supplies whatever additional attribute (e.g. `project_id`) or subnet rendering the launch wizard actually requires, determined by tracing the wizard's real API calls (FR-014). The #532 F4 dedup/filter fix (already in this branch's base) is the input.
- **Verification is manual/runbook-driven in both modes** (no automated browser suite), reusing SPEC-018's stub (`docker-compose-horizon.yml`) and real (k3s + `charts/lightstack` real mode + `horizon-chart` + Rook/Ceph RGW on minikube) setups. F2 was confirmed in the stub pass and left "to be confirmed" in real; the fix targets the root regardless of which mode surfaced it, and both modes are re-verified.
- **Server-side integration.** Horizon calls Lightstack server-side (the Django process reads the catalog and calls each service; the AngularJS launch wizard uses Horizon's same-origin `/api/…` proxy which forwards server-side), per SPEC-018 research U3 — so this feature concerns Lightstack's API responses, not browser/CORS wiring.
- **Reference material**: `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` (findings F2/F3/F7/F8) and the SPEC-018 runbooks (`HOST_TESTING.md`, `HOST_TESTING_REAL_MODE.md`).
- **Verify-then-fix lineage.** This branch is based on `main`, where #532 (SPEC-019) landed as `0a4dde0`; its F4/F5/F6/F9 fixes are already in the tree; the create/write-UI findings were spun off as #533, exactly as SPEC-016 spun off #524 and SPEC-018 spun off #531.

## Out of Scope

- **F1** (noVNC interactive console) — ticket #534 (sequenced last).
- **F4, F5, F6, F9** — already fixed in #532 / SPEC-019 and present in this branch's base; not re-done here (only relied upon: the F4 deduplicated network list feeds US1/US3).
- **Full per-project quota enforcement** — rejecting over-quota creates beyond what Lightstack already does; this feature serves/reconciles the read surface only.
- **A full external/provider-network model** in Neutron — only the network attributes the launch wizard and Project panel need are in scope.
- **Any automated browser test suite** — verification remains manual/runbook-driven, per SPEC-018.
- **Object storage / Swift panels** — no Swift API (tracked separately in lightstack#15).
- **Patching or overriding Horizon's own config/policy** to force a control to appear — fixes are Lightstack-side and the SPEC-018 Horizon deployment stays stock; a workflow that would require a Horizon patch is a documented finding, not a fix (see FR-018).

## Dependencies

- Based on `main`, which carries **#532 / SPEC-019** (`0a4dde0`; F4/F5/F6/F9 fixes already in tree). **F4 unblocks F7**: the launch wizard consumes the deduplicated, filter-honoring network list.
- Based on **feature/482 / SPEC-018**, which produced `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` and the two `HOST_TESTING*.md` runbooks reused for verification.
- Part of the **#531** umbrella (step **2 of 3**); sibling **#534** (noVNC console) is sequenced **last**.
