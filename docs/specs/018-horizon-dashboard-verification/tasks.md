---
description: "Task list for SPEC-018 — Verify Horizon dashboard compatibility"
---

# Tasks: Verify Horizon dashboard compatibility

**Input**: Design documents from `docs/specs/018-horizon-dashboard-verification/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: This is a **verification** feature with **no product code** and no automated browser suite (spec Out-of-Scope; plan Technical Context). Verification is manual/runbook-driven. There are therefore **no TDD/test-code tasks**: Constitution Article III Test-First applies to product
code, and none is added here. The existing `test/horizon_*.sh` scripts are kept as supporting API-level
evidence, but they are **not untouched** — this feature updates four of them (+185/−69) to make that
evidence trustworthy: host parameterisation via `LIGHTSTACK_HOST`, `jq -e` assertions instead of
substring greps, server-status polling, resource cleanup, and an auth-rejection group. That work is
tracked as **T032**.

**Organization**: Tasks are grouped by the four user stories from spec.md. The shared verification environments (stub + real) and the Horizon verification chart are **Foundational** — they block all workflow verification.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files / independent infra, no dependency on an incomplete task)
- **[Story]**: US1–US4 for user-story phases only
- Paths are relative to the repo root

## Path conventions (this feature)

- Verification Horizon chart: `deployments/horizon-chart/`
- Reused as-is: `deployments/docker-compose-horizon.yml`, `deployments/horizon-config/*`, `test/horizon_*.sh`, product `charts/lightstack/` (unchanged)
- Feature docs/runbooks: `docs/specs/018-horizon-dashboard-verification/`
- Report: `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`

---

## Phase 1: Setup (Shared scaffolding)

**Purpose**: Create the artifact skeletons the rest of the work fills in.

- [X] T001 [P] Scaffold the verification Horizon chart: `deployments/horizon-chart/Chart.yaml` (name `lightstack-horizon`, appVersion `2025.2`) and an empty `deployments/horizon-chart/values.yaml`
- [X] T002 [P] Create the feature index `docs/specs/018-horizon-dashboard-verification/README.md` — goal, scope, and a "where things live" table (mirror SPEC-016's `README.md`), cross-linking spec/plan/research/contracts/runbooks/report and ticket #482
- [X] T003 [P] Create the report skeleton `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` from `contracts/report-template.md` (empty environment + verdict tables, Findings placeholder)

---

## Phase 2: Foundational (Blocking prerequisites)

**Purpose**: Build the verification Horizon chart, author both runbooks, and stand up both verification environments with seed fixtures. **No workflow verification (US1–US4) can begin until this phase is complete.**

**⚠️ CRITICAL**: This is the bulk of the effort — the real environment (k3s + minikube/Rook-Ceph + Horizon chart) is what makes the graded verdicts possible.

### Horizon verification chart (`deployments/horizon-chart/`)

- [X] T004 [P] ConfigMap template `deployments/horizon-chart/templates/configmap.yaml` mounting `deployments/horizon-config/*` (`config.json`, `local_settings`, apache vhosts) at the kolla `/var/lib/kolla/config_files/` paths; template `OPENSTACK_HOST`/`OPENSTACK_KEYSTONE_URL`/`AVAILABLE_REGIONS` to the in-cluster lightstack Service DNS (`<release>-lightstack:35357/v3`) — see research.md U3
- [X] T005 [P] Horizon Deployment `deployments/horizon-chart/templates/horizon-deployment.yaml` — unmodified kolla `quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble`, `KOLLA_INSTALL_TYPE=source`, `KOLLA_CONFIG_STRATEGY=COPY_ALWAYS`, mounting the ConfigMap
- [X] T006 [P] Horizon NodePort Service `deployments/horizon-chart/templates/horizon-service.yaml` (port 80 → NodePort)
- [X] T007 [P] memcached Deployment + ClusterIP Service `deployments/horizon-chart/templates/memcached.yaml` (sessions)
- [X] T008 [P] novnc Deployment + NodePort Service `deployments/horizon-chart/templates/novnc.yaml` (`bonigarcia/novnc`) — note in a comment that the console URL is hardcoded `localhost:6080` (research.md U1), so this is for behavior-reproduction only
- [X] T009 Fill `deployments/horizon-chart/values.yaml` — image tags, lightstack release/namespace for the Service DNS, NodePort numbers, memcached/novnc toggles (depends on T004–T008)
- [X] T010 `helm lint` + `helm template` the chart and fix render errors; paste the clean render into `docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md` (depends on T009). *(Optional: a minimal `helm-unittest` render smoke matching the repo's existing chart-test harness — not required; automated browser tests remain out of scope.)*

### Runbooks (the reproducible procedure — FR-014)

- [X] T011 [P] Author the stub-pass runbook `docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md` — prereqs → `docker compose -f deployments/docker-compose-horizon.yml up` (or `scripts/start_validation.sh`) → per-workflow steps → teardown; zsh-copy-paste-safe blocks
- [X] T012 [P] Author the real-pass runbook `docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md` — root k3s prereqs (libvirtd/KVM/qemu-img/genisoimage); minikube + Rook/Ceph `CephObjectStore` + `CephObjectStoreUser` + pre-created bucket + RGW NodePort; lightstack chart real-mode values (realMode, `nova.libvirtMode: real`, `glance.storageMode: s3` + `glance.s3.{endpoint,bucket,region}`, `endpointHost`, `service.type: ClusterIP`, `rgw-s3-creds` Secret + `extraEnv` AWS keys); Horizon chart install; fixture seeding — all values from research.md U2/U3; **state an expected end-to-end setup duration** so SC-002 (real pass) is checkable; zsh-copy-paste-safe

### Stand up environments (execute the runbooks)

- [X] T013 Bring up the **stub** environment per `HOST_TESTING.md` (`deployments/docker-compose-horizon.yml`); confirm lightstack `/v3` healthcheck and Horizon `/dashboard/auth/login/` reachable; **record the bring-up wall-clock against the 15-min SC-002 target**
- [X] T014 [P] Stand up **minikube + Rook/Ceph** per `HOST_TESTING_REAL_MODE.md`: `CephObjectStore` (RGW) + `CephObjectStoreUser`, pre-create bucket `lightstack-images`, expose RGW via NodePort; record the endpoint URL + `AccessKey`/`SecretKey`
- [X] T015 [P] Stand up **root/privileged k3s** + node prereqs (libvirtd, KVM, `qemu-img`, `genisoimage`) per `HOST_TESTING_REAL_MODE.md`
- [X] T016 Install the **lightstack chart in real mode** on k3s: create the `rgw-s3-creds` Secret from T014; set `realMode.enabled`, `nova.libvirtMode: real`, `glance.storageMode: s3` + `glance.s3.endpoint`=RGW NodePort URL + `bucket: lightstack-images`, `endpointHost: <release>-lightstack`, `service.type: ClusterIP`, `extraEnv` AWS keys; confirm pods healthy + a test VM boots (depends on T014, T015)
- [X] T017 Install the **Horizon verification chart** on k3s and confirm the dashboard is reachable at its NodePort and the login page renders (depends on T009/T010, T016)
- [X] T018 Seed fixtures on **both** environments: an image (uploaded into the RGW-backed Glance on real), a flavor, and an SSH key pair (depends on T013, T016)

**Checkpoint**: both environments live, Horizon chart installed, fixtures seeded → workflow verification can begin.

---

## Phase 3: User Story 1 — Sign in and switch projects (Priority: P1) 🎯 MVP

**Goal**: Prove an unmodified Horizon can authenticate and project-scope against Lightstack.

**Independent Test**: Open each dashboard, sign in `admin`/`secret`, confirm the overview renders, switch project/domain and confirm the view re-scopes.

- [X] T019 [US1] Verify **sign-in through the browser UI** on the stub pass and the real pass — the UI walk-through is the verification; the scripts are evidence only, not a substitute (FR-001): overview renders; service catalog contains identity/compute/network/volumev3/image with a public endpoint each (supporting evidence: `test/horizon_dashboard_access_test.sh` + `test/horizon_compat_test.sh`); record both verdicts + evidence in the Results table of `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [X] T020 [US1] Verify **project/domain switching** re-scopes the view (stub + real); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` — **done 2026-09-22; the two halves carry separate verdicts** — *project* switching `working` in both passes (below), *domain* switching `not working` (F11; report row **2b**). Blocked since the first pass only because a single seeded project left nothing to switch to; both runbooks now seed `verify-two` plus the admin role assignment (without the role the switcher will not offer it). Browser-confirmed re-scoping, corroborated at the session level: `/dashboard/auth/switch/<id>/` → `302`, page reports `verify-two`, `/dashboard/api/neutron/networks/` drops 2 items → **0** (the fixtures belong to `default`, so an empty view is the re-scope).

**Checkpoint**: authentication slice proven and recorded — a partial report already delivers value (MVP).

---

## Phase 4: User Story 2 — Manage core resources through the UI (Priority: P2)

**Goal**: Prove launch-instance (+SSH), create-network, and create-volume work through the dashboard.

**Independent Test**: Drive each workflow to completion in the UI; in the real pass, SSH into the launched VM with the selected key.

- [X] T021 [P] [US2] Verify **create-network** (+subnet) via the UI; **regression-guard** the Neutron catalog URL (`…:9696`, no `/v2.0`; `test/horizon_critical_test.sh`); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [X] T022 [P] [US2] Verify **create-volume** via the UI (node-local backend, no RBD); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [X] T023 [US2] Verify **launch-instance** via the NG wizard (image from RGW + flavor + key pair + network); **regression-guard** that server-detail includes `addresses` (Instances panel renders); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [X] T024 [US2] **SSH** into the launched instance with the selected key (default SG admits tcp/22; cirros injects the key via cloud-init) to prove end-to-end boot; record the evidence against the launch-instance row in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` (depends on T023)

**Checkpoint**: the three named resource workflows verified, real pass authoritative.

---

## Phase 5: User Story 3 — Instance console panel (noVNC) (Priority: P3)

**Goal**: Record a grounded verdict for the dashboard Console tab (expected `not-working` per research.md U1).

**Independent Test**: Open a running instance's Console tab; observe whether the noVNC viewer shows the guest.

- [X] T025 [US3] Open the **Console** tab of a running instance (real pass); record the result — **expected `not-working`** — and write finding **F1** (hardcoded `localhost:6080` URL + unpersisted token + no libvirt VNC-port readback + an unwired noVNC proxy; `internal/nova/console.go`) into the Findings section of `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [X] T026 [US3] Confirm the serial **Console Log** (`openstack console log show`) returns real boot output in real mode; record it as the working console-adjacent feature in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`

**Checkpoint**: console verdict + finding recorded.

---

## Phase 6: User Story 4 — Publish a reproducible compatibility report (Priority: P4)

**Goal**: A single authoritative, reproducible report under `docs/`.

**Independent Test**: The report lists every workflow with a verdict + evidence, names the environment, references the runbooks, and there is no contradictory prior claim.

- [X] T027 [US4] Finalize the **environment** + **per-workflow verdict** tables in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` (both passes; Horizon version, real topology) from the recorded results
- [X] T028 [US4] Write the **Findings** section (F1 console + any others discovered) with cause / user impact / workaround / follow-up in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [X] T029 [US4] **Reconcile** `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md` (SPEC-002's ungrounded "100% compatible") — supersede or explicitly link so a single authoritative statement remains (depends on T027)
- [X] T030 [US4] Cross-link report ↔ runbooks ↔ ticket #482 with relative paths; add the report to `docs/INDEX.md` if it indexes compatibility docs
- [X] T031 [US4] Open a **follow-up ticket** for finding F1 (wire real noVNC: token store + libvirt VNC-port readback + a real proxy + configurable URL) and reference it from the report (mirrors SPEC-016 → #524) — [cloud-storage#534](https://github.com/cobaltcore-dev/cloud-storage/issues/534), opened 2026-07-24 under parent [#531](https://github.com/cobaltcore-dev/cloud-storage/issues/531); the report now names an issue for every finding F1–F11 (F10 added 2026-09-22 under #533; F11 under #549)

**Checkpoint**: report published; one authoritative Horizon compatibility statement.

---

## Phase 7: Polish & cross-cutting

- [X] T032 [P] Harden the supporting API-level tests so their evidence is trustworthy (4 files, +185/−69): de-hardcode the host `10.2.199.101` in `test/horizon_critical_test.sh` and `test/horizon_full_test.sh` — both now read `LIGHTSTACK_HOST` (default `localhost`); replace substring greps with `jq -e` assertions, add server-status polling and resource cleanup in `test/horizon_compat_test.sh`, and add an auth-rejection group asserting a service refuses a bogus token
- [X] T033 [P] **Re-run** the host runbooks end-to-end as a reproducibility check (SC-005) — `HOST_TESTING.md` (stub) and `HOST_TESTING_REAL_MODE.md` (real), which are the reproducible procedure per FR-014 — and note any drift. (`quickstart.md` is a condensed summary that defers to them, not the check.) **Done 2026-09-22, both runbooks re-run; SC-005 PASS — every recorded verdict reproduced.** Drift found (all now folded into the report):
  1. **F7's first half did not reproduce** — the Instances panel's Launch-Instance dialog **opened**. The 2026-07-23 "no Launch Instance button" observation is stale; the launch still fails, but at the Networks step only.
  2. **F7's Networks step root-caused to F4** — the duplicate `id` makes AngularJS's `track by` repeater throw, so zero rows render while the count still reads 2. Closed the report's last open question.
  3. **New finding F10** — `ListSecurityGroups` omits `security_group_rules`, 500-ing Horizon's SG proxy. Reproduces in both modes; was hidden behind F7 until the wizard was driven with a network present.
  4. **Stub F5 understated** — the Volumes panel renders and shows a (dead) Create button rather than being wholly dead, i.e. the stub failure looks like F8, not a blank panel.
  5. **Runbook defects — found and since FIXED (2026-09-22)**: the image-seed block sat *after* T5 though T5 cannot run without it; **no step seeded a network** at all, and the UI cannot create one (F3); the `[ -f /tmp/cirros.img ] || curl ...` guard treated "file exists" as "file complete", so a partial download produced `upload HTTP 503` + `qemu-img convert qcow2->raw: I/O error` that reads exactly like a Glance defect; neither runbook seeded a **second project**, which is what had blocked T020 since the first pass; and the real runbook's `openstack server show <vm>` was not paste-safe (`<vm>` is shell redirection). Both runbooks now carry a required fixture-seeding section (image with a **size** check, network+subnet, second project + role assignment) placed **before** the test steps, and all 30 shell blocks pass `zsh -n`.
- [X] T034 Final consistency pass — spec / plan / report / runbooks agree; tick the matching boxes on the #482 `## Checklist` comment to reflect completed work — **done 2026-09-22.** Report + nine cross-referencing docs brought to `F1–F10`; both host runbooks repaired so they can be followed top-to-bottom (see T033 item 5); #482 checklist comment updated — the stale optional harness box ticked (T032 had landed) and a 2026-09-22 section added covering the re-run, F10, the F4/F7 root cause, the runbook repairs and the project-switching unblock. **Also 2026-09-22**: **F11** added — domain-scoped token requests are silently downgraded to unscoped (`internal/keystone/auth.go:55-92`), so FR-003's *domain* half is not-working rather than unexercised; eight cross-referencing docs moved to `F1–F11`.

### Completion

**All 34 tasks are ticked as of 2026-09-22.** T001–T019 and T021–T032 were ticked against the results
recorded in
[`../../compatibility/HORIZON_COMPATIBILITY_REPORT.md`](../../compatibility/HORIZON_COMPATIBILITY_REPORT.md)
(stub pass 2026-07-22, real pass 2026-07-23, root-causing 2026-08-25). The last three closed on
**2026-09-22**: **T033** (both runbooks re-run end-to-end, SC-005 PASS, drift recorded), **T020** (a second
seeded project finally made re-scope testable — real verdict `working`), and **T034** (report, docs,
runbooks and the #482 checklist reconciled).

Note what the completed checklist does **and does not** mean. The verification is complete; the product is
not fixed. The report's standing verdict is unchanged and unfavourable — Lightstack is a drop-in for
OpenStack API/CLI **operations**, but the Horizon dashboard is **not** usable for resource management, and
`F1–F11` remain open defects: F1–F10 under #531 → #532 / #533 / #534, and F11 (domain-scoped tokens) under #549. Fixes are out of scope here by
design (verify-not-fix, as SPEC-016 spun off #524).

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (Phase 1)**: no dependencies — start immediately.
- **Foundational (Phase 2)**: after Setup. Internal order: chart build (T004–T008 [P] → T009 → T010) and runbooks (T011, T012 [P]) can proceed in parallel; then env standup — T013 (stub) independent; T014 (minikube/RGW) ∥ T015 (k3s); T016 needs T014+T015; T017 needs T009/T010+T016; T018 needs T013+T016. **Blocks all user stories.**
- **US1 (Phase 3)**: after Foundational. MVP.
- **US2 (Phase 4)**: after Foundational. Independent of US1 (but naturally follows).
- **US3 (Phase 5)**: after Foundational; needs a running instance (reuse US2's or launch one).
- **US4 (Phase 6)**: after the workflows you intend to report on (US1–US3) are verified. T029 after T027.
- **Polish (Phase 7)**: after US4.

### Within each user story

- Verification tasks record into the single shared report `HORIZON_COMPATIBILITY_REPORT.md` — treat it as a **single-writer** artifact (a manual verifier). Hence most in-story tasks are sequential, not `[P]`, even when the workflows themselves are independent.

### Parallel opportunities

- Setup: T001, T002, T003 all `[P]`.
- Foundational: chart templates T004–T008 `[P]`; runbooks T011, T012 `[P]`; infra T014 ∥ T015 `[P]`.
- US2: T021 (network) and T022 (volume) are independent workflows — `[P]` if two verifiers, else sequential (shared report file).
- Polish: T032, T033 `[P]`.

---

## Parallel Example: Foundational chart build

```bash
# Author the four Horizon chart templates together (different files):
Task: "T004 ConfigMap template deployments/horizon-chart/templates/configmap.yaml"
Task: "T005 Horizon Deployment deployments/horizon-chart/templates/horizon-deployment.yaml"
Task: "T006 Horizon NodePort Service deployments/horizon-chart/templates/horizon-service.yaml"
Task: "T007 memcached deployments/horizon-chart/templates/memcached.yaml"

# Stand up the two clusters concurrently:
Task: "T014 minikube + Rook/Ceph RGW"
Task: "T015 root k3s + libvirt prereqs"
```

---

## Implementation Strategy

### MVP first (US1)

1. Phase 1 Setup → Phase 2 Foundational (both environments + chart + fixtures).
2. Phase 3 US1 (sign-in + project switching).
3. **STOP and VALIDATE**: a partial report with the auth slice already proves an unmodified Horizon authenticates against Lightstack — a viable increment.

### Incremental delivery

- Foundational → US1 (auth) → US2 (resources +SSH) → US3 (console verdict/F1) → US4 (publish + reconcile). Each adds report rows without breaking earlier ones.

### Notes

- The console verdict (US3) is **known in advance** (not-working, F1) — the task is to *reproduce and document* it, then spin off the fix as a follow-up (out of scope here, as SPEC-016 → #524).
- Product `charts/lightstack` is **not** modified; Horizon ships as the separate `deployments/horizon-chart/`.
- Keep runbook shell blocks zsh-copy-paste-safe (no `$!`/bare `!` in double quotes, no `\` line-continuations, no inline `#`); verify with `zsh -n`.
