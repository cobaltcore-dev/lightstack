# Feature Specification: Verify Horizon dashboard compatibility

**Feature Branch**: `018-horizon-dashboard-verification`  
**Created**: 2026-07-21  
**Updated**: 2026-07-22  
**Status**: Draft  
**Input**: [cloud-storage#482](https://github.com/cobaltcore-dev/cloud-storage/issues/482) — "lightstack — Verify Horizon dashboard compatibility". Verify the real OpenStack Horizon dashboard against Lightstack for developer use. The deployment stack and an API-level test suite already exist; the gap is verifying the actual UI workflows and publishing a report.

## User Scenarios & Testing *(mandatory)*

This is a **verification** feature: the deliverable is documented, reproducible evidence that an unmodified OpenStack Horizon dashboard works against Lightstack for the common developer workflows — plus a published report stating, per workflow, what works and what does not. It does **not** add product features to Lightstack; any defect it surfaces becomes a separate follow-up (as SPEC-016's Terraform verification spun off #524).

Verification runs in **two manual, runbook-driven passes** (no automated browser suite):

- **Portable pass — stub mode.** The existing all-in-one stack (`deployments/docker-compose-horizon.yml`, SPEC-002's deliverable) run as-is on any host. Exercises the Horizon↔Lightstack UI/API contract with no Kubernetes, KVM, or Ceph. Fast, laptop-grade, catches contract regressions.
- **Graded pass — real mode (authoritative).** The report's verdicts are graded here: real-mode lightstack on a **root/privileged k3s** cluster (real libvirt/KVM VMs), with **Rook/Ceph on minikube** providing image storage over **RGW (S3)**, and Horizon deployed onto the same k3s cluster. This is where backend-dependent workflows (real volumes, live console) get a genuine verdict.

The priority order below reflects **build dependency and MVP value**, not relative importance: the report (the last story) is the ticket's headline deliverable, but it can only report on what the earlier stories have verified.

### User Story 1 - Sign in and switch projects in the real dashboard (Priority: P1)

A developer evaluating Lightstack opens the deployed Horizon dashboard in a browser, signs in with their Lightstack credentials, lands on the project overview, and switches between the projects and domains their account can access — exactly as they would against upstream OpenStack.

**Why this priority**: Authentication and project scoping are the gate to everything else in the dashboard. If a developer cannot log in and select a project, no other UI workflow is reachable, so this is the minimum viable slice — on its own it proves that a real, unmodified Horizon can authenticate and scope against Lightstack.

**Independent Test**: Bring up either verification stack against a running Lightstack, open the dashboard in a browser, sign in with the default credentials, confirm the overview page renders, then switch project/domain and confirm the scoped view updates. Delivers value even if no resource workflow is verified.

**Acceptance Scenarios**:

1. **Given** a verification stack is running against a healthy Lightstack, **When** a developer opens the dashboard and signs in with valid Lightstack credentials, **Then** authentication succeeds and the project overview page loads without errors.
2. **Given** a signed-in developer whose account has access to more than one project, **When** they switch the active project (and domain) in the dashboard, **Then** the dashboard re-scopes to the selected project and its resources.
3. **Given** the dashboard is exposed to the verifier's browser (a NodePort address in the real pass), **When** the developer opens and signs in to it, **Then** the dashboard is reachable and its authenticated pages load without connectivity errors. (Lightstack stays in-cluster; Horizon calls it server-side — see the reachability edge case.)

---

### User Story 2 - Manage core resources through the UI (Priority: P2)

A developer uses the Horizon dashboard to perform the three everyday resource operations: launch an instance, create a network, and create a volume. Each operation is driven through the dashboard's own forms, and the resulting resource appears in the corresponding list view. In the graded (real) pass, a launched instance is additionally confirmed to be alive by connecting to it over SSH.

**Why this priority**: These three workflows are the core of day-to-day cloud use and are the exact operations named in the ticket's acceptance criteria. Verifying them — and, in real mode, proving the launched VM actually boots and is reachable — proves Lightstack's "UI compatibility / drop-in replacement" claim for the operations developers actually perform.

**Independent Test**: After signing in (US1), drive each of the three workflows to completion through the dashboard forms and confirm the created resource is listed and reaches the expected state (or record the precise point of failure). In real mode, SSH into the launched instance using the key pair selected in the launch form. Each workflow can be verified in isolation.

**Acceptance Scenarios**:

1. **Given** a signed-in, project-scoped developer, **When** they complete the launch-instance form in the dashboard (choosing a source image, flavor, key pair, and network), **Then** the instance is created and appears in the instances list with a reported status.
2. **Given** a launched instance in the real pass, **When** the verifier connects over SSH using the selected key pair and the instance's address, **Then** the connection succeeds — confirming the UI-driven image, network, key-pair, and (where used) floating-IP inputs all took effect and the VM booted.
3. **Given** a signed-in developer, **When** they create a network (and subnet) through the dashboard, **Then** the network appears in the networks list and is available for selection when launching an instance.
4. **Given** a signed-in developer, **When** they create a volume through the dashboard, **Then** the volume appears in the volumes list with a reported status.
5. **Given** any workflow fails or only partially completes, **When** the verifier observes the failure, **Then** the exact failing step and observed behavior are captured for the report (rather than the workflow being silently skipped).

---

### User Story 3 - Open an instance's console in the dashboard (Priority: P3)

A developer opens the **Console** tab of a running instance in the Horizon dashboard and sees the VM's live console (the noVNC in-browser viewer) rendered in the browser.

**Why this priority**: This is a scope **addition** beyond the ticket's five named workflows, made testable by the graded pass running real VMs. It exercises a distinct Horizon feature (the console panel) that depends on Lightstack implementing the Nova remote-console URL API and wiring the noVNC proxy — a genuinely uncertain area, and therefore a valuable verdict for the report. It is lower priority precisely because it is an addition and is expected to be a candidate "partial / not working" result.

**Independent Test**: In the real pass, launch an instance (US2), open its Console tab in the dashboard, and confirm the noVNC viewer connects and displays the VM console (e.g. the cirros login prompt) — or record exactly where it fails.

**Acceptance Scenarios**:

1. **Given** a running instance in the real pass, **When** the developer opens its Console tab in the dashboard, **Then** the noVNC viewer loads and connects to the VM's console — or the failure point (console-URL API, proxy connectivity, token) is recorded for the report.

---

### User Story 4 - Publish a reproducible compatibility report (Priority: P4)

A maintainer records the outcome of each verified workflow — working, not working, or working with known gaps — in a compatibility report published under `docs/` in the lightstack repository. The report names the environment each verdict was produced against (the graded verdicts come from the real pass), points at a reproducible procedure, and reconciles the pre-existing Horizon compatibility document so there is a single authoritative statement.

**Why this priority**: The report is the ticket's headline deliverable and the artifact the team relies on afterward. It is ordered last only because it aggregates the results of the earlier stories; a partial report (e.g. login verified only) still delivers value.

**Independent Test**: Confirm a report exists under `docs/` that lists each verified workflow with a verdict and evidence, identifies the environment (including the real-mode topology), and references a documented, repeatable procedure; confirm a reader can follow that procedure and reproduce the stated verdicts.

**Acceptance Scenarios**:

1. **Given** the workflows have been verified, **When** a reader opens the published report, **Then** they see, per workflow, a clear working / not-working / partial verdict plus any known gaps or limitations, and which pass (stub / real) produced it.
2. **Given** the report, **When** a reader wants to reproduce the result, **Then** the report points to a documented procedure (deployment + steps) sufficient to re-run both passes.
3. **Given** the pre-existing `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md` (SPEC-002's "100% compatible" claim, never grounded in a reproducible browser-level check), **When** the new report is published, **Then** the two are reconciled so there is one authoritative statement (the older document superseded or explicitly linked), with no contradictory claims left standing.
4. **Given** any workflow marked "not working" or "partial", **When** a reader inspects that entry, **Then** the report states the specific gap, its user impact, and (where known) a workaround or follow-up reference.

---

### Edge Cases

- **Partial success**: an instance launches and is SSH-reachable but its console panel does not open, or a volume is created but cannot attach. The report must distinguish "working", "partial / known gap", and "not working" rather than collapsing to pass/fail.
- **Stub cannot prove backend behavior**: in the portable (stub) pass there is no real VM, volume, or console, so backend-dependent workflows are recorded as "not exercised in stub" — their real verdict comes only from the graded pass.
- **Reachability topology**: in the real pass the browser reaches only Horizon (a NodePort); Lightstack stays in-cluster (ClusterIP) and is called by Horizon **server-side**, so Lightstack's service catalog must advertise a host the **Horizon pod** can resolve (the in-cluster Service name via `endpointHost`), not a browser-reachable one. A catalog host left at the loopback default breaks Horizon→Lightstack even though the dashboard loads — that is an environment misconfiguration, not a Horizon incompatibility. Browser→Lightstack CORS is **not** exercised by the proxied integration (confirmed in `research.md` U3).
- **Cross-cluster storage reachability**: real-mode lightstack on k3s must be able to reach the Ceph **RGW (S3)** endpoint served by Rook/Ceph on minikube; if that path is down, image workflows fail for an infrastructure reason that must not be misreported as a Horizon incompatibility.
- **Console panel**: the noVNC console depends on Lightstack implementing the Nova remote-console URL API and wiring the proxy; a missing or stubbed console API surfaces as a "partial / not working" console verdict.
- **SSH preconditions**: SSH liveness depends on the default security group admitting TCP/22 and the image (cirros) injecting the selected key via cloud-init; if either is absent the failure is a fixture/config issue, not a dashboard incompatibility, and must be recorded as such.
- **Historical regressions**: previously fixed Horizon-specific incompatibilities (e.g. an instance list failing to render, or a service endpoint recorded with the wrong path) must not have regressed; a regression here breaks a whole dashboard panel and must be reported as "not working".

## Requirements *(mandatory)*

### Functional Requirements

**Scope — what is verified**

- **FR-001**: The verification MUST exercise the workflows through an **unmodified upstream OpenStack Horizon dashboard** (the pinned kolla image) running against a live Lightstack — the real UI, not solely API-level simulation. "Unmodified" means no Horizon source changes; only standard dashboard configuration is permitted.
- **FR-002**: The verification MUST confirm a developer can **sign in** to the dashboard with Lightstack credentials and that the project overview loads.
- **FR-003**: The verification MUST confirm **project (and domain) switching** works in the dashboard and re-scopes the view.
- **FR-004**: The verification MUST exercise the **launch-instance** workflow through the dashboard and record whether the instance is created and listed. In the graded (real) pass it MUST additionally confirm the instance is **reachable over SSH** using the key pair selected in the launch form.
- **FR-005**: The verification MUST exercise the **create-network** workflow through the dashboard and record whether the network is created and listed.
- **FR-006**: The verification MUST exercise the **create-volume** workflow through the dashboard and record whether the volume is created and listed.
- **FR-007**: The verification MUST exercise the **instance console panel** (noVNC) in the graded pass and record whether the in-browser console connects to the running VM. (Scope addition beyond the ticket's five named workflows.)

**Method and environment**

- **FR-008**: The verification MUST be performed as **two manual, runbook-driven passes** — a portable stub pass and a graded real pass — with **no automated browser test suite** required. The report's authoritative verdicts MUST come from the graded pass; the report MUST state which pass produced each verdict.
- **FR-009**: The graded pass MUST run against a **real-mode lightstack** — real compute (libvirt/KVM VMs) and real networking — deployed via the lightstack Helm chart on a **privileged/root Kubernetes (k3s)** node, with **image storage backed by Ceph RGW (S3)** provided by Rook/Ceph. **Ceph RBD block storage is out of scope and MUST NOT be relied upon** (v1 does not support it); volumes use a node-local backend.
- **FR-010**: Horizon MUST be deployed onto the **same Kubernetes cluster** as the real-mode lightstack, using the **unmodified upstream image** and **reusing the existing dashboard configuration** (`deployments/horizon-config/*`). This MUST be done **without modifying the product lightstack Helm chart** (Horizon is deployed as a separate, verification-scoped Helm artifact), and MUST NOT require adopting a third-party OpenStack Horizon chart.

**Reporting**

- **FR-011**: For every verified workflow the verification MUST record a verdict of **working / not working / partial**, with observed behavior as evidence. (Within a single pass a backend-dependent workflow may instead be marked **`not-exercised`** — e.g. in the stub pass, which has no real VM/volume/console; that is a per-pass annotation, not the workflow's final verdict, which comes from the graded pass. Matches the `not-exercised` value in `data-model.md`.)
- **FR-012**: The verification MUST produce a **compatibility report published under `docs/`** in the lightstack repository.
- **FR-014**: The verification MUST be **reproducible**: the report MUST reference a documented procedure sufficient for a reader to re-run both passes and reach the same verdicts.
- **FR-015**: For every "not working" or "partial" verdict the report MUST state the **specific gap, its user impact**, and (where known) a workaround or follow-up reference.
- **FR-016**: The report MUST be **reconciled with the pre-existing Horizon compatibility document** (`docs/HORIZON_FULL_COMPATIBILITY_REPORT.md`) so that a single authoritative statement remains (the prior document superseded or explicitly linked), with no contradictory claims left standing.
- **FR-017**: The verification MUST confirm the historical Horizon-specific compatibility fixes have **not regressed** (dashboard panels that previously failed to render must render), reporting any regression as "not working".

### Key Entities *(documentation artifacts, not persisted data)*

- **Workflow verification result**: one verified workflow (sign-in, project switching, launch instance, create network, create volume, instance console) paired with a verdict (working / not working / partial), the pass it came from (stub / real), observed behavior, and any known gap.
- **Compatibility report**: the published document aggregating the verification environment, the per-workflow results, and any findings/follow-ups; the ticket's graded deliverable.
- **Verification environment**: the concrete context a verdict is valid for — Horizon version, pass (stub / real), and, for the real pass, the two-cluster topology (k3s real-mode lightstack + Horizon; Rook/Ceph on minikube via RGW/S3).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of the six verified workflows (sign-in, project switching, launch instance, create network, create volume, instance console) have a recorded working / not-working / partial verdict with evidence in the published report — none left "untested".
- **SC-002**: The portable (stub) pass is fast to reproduce — a reader following the runbook reaches a signed-in dashboard on a clean host within 15 minutes; the graded (real) pass is reproducible via the documented procedure within its stated setup time (no fixed bound, given the k3s + Rook/Ceph provisioning).
- **SC-003**: Exactly one authoritative Horizon compatibility report exists under `docs/`; the prior `HORIZON_FULL_COMPATIBILITY_REPORT.md` is superseded or explicitly linked, with no contradictory "compatible / not compatible" claims left standing.
- **SC-004**: Every workflow verdict of "not working" or "partial" has a stated cause, user impact, and (where known) a workaround or follow-up reference — zero unexplained failures.
- **SC-005**: An independent reader re-running the documented procedure reaches the same per-workflow verdicts the report states (reproducibility confirmed) for the pass in question.

## Assumptions

- **Two manual passes**: verification is manual and runbook-driven in both modes — no automated browser (e.g. Selenium/Playwright) suite. The stub pass reuses `deployments/docker-compose-horizon.yml` as-is (SPEC-002's stack, already wired into `scripts/start_validation.sh`); the real pass is the authoritative, graded environment.
- **Real topology (two clusters)**: k3s runs **as root/privileged** and hosts both the real-mode lightstack (via the Helm chart: `realMode.enabled`, `nova.libvirtMode: real`, `glance.storageMode` including `s3`) and Horizon; **minikube** runs **Rook/Ceph** exposing an **RGW (S3)** endpoint that lightstack's Glance points at. Real backend split: **VMs → libvirt/KVM, images → RGW/S3, volumes → node-local `/var/lib/lightstack/volumes`**. This is the SPEC-009/010-validated k3s real-mode path.
- **RBD is permanently out**: Ceph block storage (RBD) is not supported in v1; only RGW (S3) is used for real Ceph. The spec assumes no RBD anywhere.
- **Real compute works**: root/privileged k3s + Helm-deployed real-mode lightstack creates real libvirt VMs, so `launch instance` (and the console) get genuine verdicts.
- **Horizon on k3s = a dedicated verification Helm chart** (not the product lightstack chart, not openstack-helm's ecosystem-coupled `horizon` chart): a small chart wrapping the kolla `horizon:2025.2` image + a ConfigMap carrying the existing `horizon-config/*` (repointing `OPENSTACK_HOST`/`OPENSTACK_KEYSTONE_URL` at the in-cluster lightstack Service DNS), plus a trivial **memcached** Deployment and a maintained, pinned **`bonigarcia/novnc`** proxy. Lightstack stays **ClusterIP** with its catalog `endpointHost` set to that in-cluster name — Horizon calls Lightstack server-side, so no browser→Lightstack CORS is involved (`research.md` U3). Browser access via **NodePort** for the dashboard (and the novnc proxy, though the console URL is hardcoded to `localhost` — a known gap, `research.md` U1), avoiding ingress websocket friction. No new docker-compose file is created (Helm does not consume compose files; a real+Horizon compose would only be a dev convenience).
- **VM liveness two ways**: the launched VM is confirmed via **SSH** (proven by SPEC-009's `sc007`; the default security group admits TCP/22 and cirros injects the key via cloud-init) **and** via the Horizon **console panel** (noVNC). The console verdict may legitimately be "partial / not working" if the Nova remote-console URL API is absent.
- **Horizon version**: the pinned upstream kolla image (Flamingo 2025.2). Other releases are out of scope.
- **Credentials & seed data**: default Lightstack admin credentials and default project; the minimal fixtures the workflows need (an image, a flavor, an SSH key pair) are seeded as part of the procedure.
- **Report location**: `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, mirroring the Terraform report, reconciling/superseding `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md`.
- **Multi-node friendliness (design note, not this scope)**: the lightstack chart already supports multi-node (DaemonSet + VXLAN), and Horizon is stateless (sessions in memcached) so it scales independently — the separate-chart split keeps the two lifecycles decoupled. The one multi-node wart (routing the console proxy to the VM's host node) is sidestepped by verifying single-node.

## Out of Scope

- Implementing missing Horizon features or fixing incompatibilities discovered during verification — those become separate follow-up tickets (as SPEC-016 spun off #524).
- An automated browser test suite and any new CI job driving the Horizon UI.
- **Ceph RBD / block-storage backend** — not supported in v1; real Ceph is RGW (S3) only.
- **Folding Horizon into the product lightstack Helm chart** — the verification uses a separate, verification-scoped chart.
- **Multi-node deployment** — the graded eval is single-node; the design is noted as multi-node-friendly but multi-node verification (incl. cross-node console routing) is deferred.
- A new `docker-compose` file for real-mode + Horizon (the real eval is Helm-based).
- Performance/load characterization of the dashboard (covered separately by the existing load-test script).
- Object storage / Swift dashboard panels — Lightstack exposes no Swift API (consistent with SPEC-016; see lightstack#15).
- Exhaustive coverage of every Admin/Identity dashboard panel beyond what the named workflows (plus console) require.

## Dependencies

- **Stub pass**: `deployments/docker-compose-horizon.yml` + `deployments/horizon-config/*`; optionally `scripts/start_validation.sh`.
- **Real pass — compute/network**: a root/privileged **k3s** cluster and the lightstack **Helm chart** in real mode (libvirt/KVM, real networking).
- **Real pass — image storage**: **minikube** running **Rook/Ceph** exposing an **RGW (S3)** endpoint, reachable from k3s; lightstack Glance configured for the `s3` backend.
- **Real pass — dashboard**: a dedicated verification **Horizon Helm chart** (kolla image + reused `horizon-config` ConfigMap) + **memcached** + a pinned **`bonigarcia/novnc`** proxy, exposed via NodePort.
- The existing `test/horizon_*.sh` API-level suite, reused as supporting evidence.
- Seed data: admin user, default project, an image + flavor, and an SSH key pair for the launch/SSH check.
- A Horizon-compatible Lightstack service catalog (identity, compute, network, block-storage, image) whose endpoints are reachable **by the Horizon pod** (in-cluster, via `endpointHost`). Horizon calls Lightstack **server-side**, so the browser reaches only Horizon, not Lightstack directly — see the reachability-topology edge case and `research.md` U3.
