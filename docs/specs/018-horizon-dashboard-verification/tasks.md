---
description: "Task list for SPEC-018 — Verify Horizon dashboard compatibility"
---

# Tasks: Verify Horizon dashboard compatibility

**Input**: Design documents from `docs/specs/018-horizon-dashboard-verification/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: This is a **verification** feature with **no product code** and no automated browser suite (spec Out-of-Scope; plan Technical Context). Verification is manual/runbook-driven. There are therefore **no TDD/test-code tasks** — the existing `test/horizon_*.sh` scripts are *reused as supporting evidence*, not rewritten. (Constitution Article III Test-First applies to product code; none is added here.)

**Organization**: Tasks are grouped by the four user stories from spec.md. The shared verification environments (stub + real) and the Horizon verification chart are **Foundational** — they block all workflow verification.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files / independent infra, no dependency on an incomplete task)
- **[Story]**: US1–US4 for user-story phases only
- Paths are relative to the repo root

## Path conventions (this feature)

- Verification Horizon chart: `deployments/horizon-chart/`
- Reused as-is: `deployments/docker-compose-horizon.yml`, `deployments/horizon-config/*`, `test/horizon_*.sh`, product `charts/o3k/` (unchanged)
- Feature docs/runbooks: `docs/specs/018-horizon-dashboard-verification/`
- Report: `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`

---

## Phase 1: Setup (Shared scaffolding)

**Purpose**: Create the artifact skeletons the rest of the work fills in.

- [X] T001 [P] Scaffold the verification Horizon chart: `deployments/horizon-chart/Chart.yaml` (name `o3k-horizon`, appVersion `2025.2`) and an empty `deployments/horizon-chart/values.yaml`
- [X] T002 [P] Create the feature index `docs/specs/018-horizon-dashboard-verification/README.md` — goal, scope, and a "where things live" table (mirror SPEC-016's `README.md`), cross-linking spec/plan/research/contracts/runbooks/report and ticket #482
- [X] T003 [P] Create the report skeleton `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` from `contracts/report-template.md` (empty environment + verdict tables, Findings placeholder)

---

## Phase 2: Foundational (Blocking prerequisites)

**Purpose**: Build the verification Horizon chart, author both runbooks, and stand up both verification environments with seed fixtures. **No workflow verification (US1–US4) can begin until this phase is complete.**

**⚠️ CRITICAL**: This is the bulk of the effort — the real environment (k3s + minikube/Rook-Ceph + Horizon chart) is what makes the graded verdicts possible.

### Horizon verification chart (`deployments/horizon-chart/`)

- [X] T004 [P] ConfigMap template `deployments/horizon-chart/templates/configmap.yaml` mounting `deployments/horizon-config/*` (`config.json`, `local_settings`, apache vhosts) at the kolla `/var/lib/kolla/config_files/` paths; template `OPENSTACK_HOST`/`OPENSTACK_KEYSTONE_URL`/`AVAILABLE_REGIONS` to the in-cluster o3k Service DNS (`<release>-o3k:35357/v3`) — see research.md U3
- [X] T005 [P] Horizon Deployment `deployments/horizon-chart/templates/horizon-deployment.yaml` — unmodified kolla `quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble`, `KOLLA_INSTALL_TYPE=source`, `KOLLA_CONFIG_STRATEGY=COPY_ALWAYS`, mounting the ConfigMap
- [X] T006 [P] Horizon NodePort Service `deployments/horizon-chart/templates/horizon-service.yaml` (port 80 → NodePort)
- [X] T007 [P] memcached Deployment + ClusterIP Service `deployments/horizon-chart/templates/memcached.yaml` (sessions)
- [X] T008 [P] novnc Deployment + NodePort Service `deployments/horizon-chart/templates/novnc.yaml` (`bonigarcia/novnc`) — note in a comment that the console URL is hardcoded `localhost:6080` (research.md U1), so this is for behavior-reproduction only
- [X] T009 Fill `deployments/horizon-chart/values.yaml` — image tags, o3k release/namespace for the Service DNS, NodePort numbers, memcached/novnc toggles (depends on T004–T008)
- [X] T010 `helm lint` + `helm template` the chart and fix render errors; paste the clean render into `docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md` (depends on T009). *(Optional: a minimal `helm-unittest` render smoke matching the repo's existing chart-test harness — not required; automated browser tests remain out of scope.)*

### Runbooks (the reproducible procedure — FR-014)

- [X] T011 [P] Author the stub-pass runbook `docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md` — prereqs → `docker compose -f deployments/docker-compose-horizon.yml up` (or `scripts/start_validation.sh`) → per-workflow steps → teardown; zsh-copy-paste-safe blocks
- [X] T012 [P] Author the real-pass runbook `docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md` — root k3s prereqs (libvirtd/KVM/qemu-img/genisoimage); minikube + Rook/Ceph `CephObjectStore` + `CephObjectStoreUser` + pre-created bucket + RGW NodePort; o3k chart real-mode values (realMode, `nova.libvirtMode: real`, `glance.storageMode: s3` + `glance.s3.{endpoint,bucket,region}`, `endpointHost`, `service.type: ClusterIP`, `rgw-s3-creds` Secret + `extraEnv` AWS keys); Horizon chart install; fixture seeding — all values from research.md U2/U3; **state an expected end-to-end setup duration** so SC-002 (real pass) is checkable; zsh-copy-paste-safe

### Stand up environments (execute the runbooks)

- [ ] T013 Bring up the **stub** environment per `HOST_TESTING.md` (`deployments/docker-compose-horizon.yml`); confirm o3k `/v3` healthcheck and Horizon `/dashboard/auth/login/` reachable; **record the bring-up wall-clock against the 15-min SC-002 target**
- [ ] T014 [P] Stand up **minikube + Rook/Ceph** per `HOST_TESTING_REAL_MODE.md`: `CephObjectStore` (RGW) + `CephObjectStoreUser`, pre-create bucket `o3k-images`, expose RGW via NodePort; record the endpoint URL + `AccessKey`/`SecretKey`
- [ ] T015 [P] Stand up **root/privileged k3s** + node prereqs (libvirtd, KVM, `qemu-img`, `genisoimage`) per `HOST_TESTING_REAL_MODE.md`
- [ ] T016 Install the **o3k chart in real mode** on k3s: create the `rgw-s3-creds` Secret from T014; set `realMode.enabled`, `services.nova.libvirtMode: real`, `glance.storageMode: s3` + `glance.s3.endpoint`=RGW NodePort URL + `bucket: o3k-images`, `endpointHost: <release>-o3k`, `service.type: ClusterIP`, `extraEnv` AWS keys; confirm pods healthy + a test VM boots (depends on T014, T015)
- [ ] T017 Install the **Horizon verification chart** on k3s and confirm the dashboard is reachable at its NodePort and the login page renders (depends on T009/T010, T016)
- [ ] T018 Seed fixtures on **both** environments: an image (uploaded into the RGW-backed Glance on real), a flavor, and an SSH key pair (depends on T013, T016)

**Checkpoint**: both environments live, Horizon chart installed, fixtures seeded → workflow verification can begin.

---

## Phase 3: User Story 1 — Sign in and switch projects (Priority: P1) 🎯 MVP

**Goal**: Prove an unmodified Horizon can authenticate and project-scope against O3K.

**Independent Test**: Open each dashboard, sign in `admin`/`secret`, confirm the overview renders, switch project/domain and confirm the view re-scopes.

- [ ] T019 [US1] Verify **sign-in through the browser UI** on the stub pass and the real pass — the UI walk-through is the verification; the scripts are evidence only, not a substitute (FR-001): overview renders; service catalog contains identity/compute/network/volumev3/image with a public endpoint each (supporting evidence: `test/horizon_dashboard_access_test.sh` + `test/horizon_compat_test.sh`); record both verdicts + evidence in the Results table of `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [ ] T020 [US1] Verify **project/domain switching** re-scopes the view (stub + real); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`

**Checkpoint**: authentication slice proven and recorded — a partial report already delivers value (MVP).

---

## Phase 4: User Story 2 — Manage core resources through the UI (Priority: P2)

**Goal**: Prove launch-instance (+SSH), create-network, and create-volume work through the dashboard.

**Independent Test**: Drive each workflow to completion in the UI; in the real pass, SSH into the launched VM with the selected key.

- [ ] T021 [P] [US2] Verify **create-network** (+subnet) via the UI; **regression-guard** the Neutron catalog URL (`…:9696`, no `/v2.0`; `test/horizon_critical_test.sh`); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [ ] T022 [P] [US2] Verify **create-volume** via the UI (node-local backend, no RBD); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [ ] T023 [US2] Verify **launch-instance** via the NG wizard (image from RGW + flavor + key pair + network); **regression-guard** that server-detail includes `addresses` (Instances panel renders); record the verdict in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [ ] T024 [US2] **SSH** into the launched instance with the selected key (default SG admits tcp/22; cirros injects the key via cloud-init) to prove end-to-end boot; record the evidence against the launch-instance row in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` (depends on T023)

**Checkpoint**: the three named resource workflows verified, real pass authoritative.

---

## Phase 5: User Story 3 — Instance console panel (noVNC) (Priority: P3)

**Goal**: Record a grounded verdict for the dashboard Console tab (expected `not-working` per research.md U1).

**Independent Test**: Open a running instance's Console tab; observe whether the noVNC viewer shows the guest.

- [ ] T025 [US3] Open the **Console** tab of a running instance (real pass); record the result — **expected `not-working`** — and write finding **F1** (hardcoded `localhost:6080` URL + unpersisted token + no libvirt VNC-port readback + an unwired noVNC proxy; `internal/nova/console.go`) into the Findings section of `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [ ] T026 [US3] Confirm the serial **Console Log** (`openstack console log show`) returns real boot output in real mode; record it as the working console-adjacent feature in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`

**Checkpoint**: console verdict + finding recorded.

---

## Phase 6: User Story 4 — Publish a reproducible compatibility report (Priority: P4)

**Goal**: A single authoritative, reproducible report under `docs/`.

**Independent Test**: The report lists every workflow with a verdict + evidence, names the environment, references the runbooks, and there is no contradictory prior claim.

- [ ] T027 [US4] Finalize the **environment** + **per-workflow verdict** tables in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` (both passes; o3k build/commit, Horizon version, real topology) from the recorded results
- [ ] T028 [US4] Write the **Findings** section (F1 console + any others discovered) with cause / user impact / workaround / follow-up in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- [ ] T029 [US4] **Reconcile** `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md` (SPEC-002's ungrounded "100% compatible") — supersede or explicitly link so a single authoritative statement remains (depends on T027)
- [ ] T030 [US4] Cross-link report ↔ runbooks ↔ ticket #482 with relative paths; add the report to `docs/INDEX.md` if it indexes compatibility docs
- [ ] T031 [US4] Open a **follow-up ticket** for finding F1 (wire real noVNC: token store + libvirt VNC-port readback + a real proxy + configurable URL) and reference it from the report (mirrors SPEC-016 → #524)

**Checkpoint**: report published; one authoritative Horizon compatibility statement.

---

## Phase 7: Polish & cross-cutting

- [X] T032 [P] (Optional) De-hardcode the host `10.2.199.101` in `test/horizon_critical_test.sh` and `test/horizon_full_test.sh` so the API-level harness is host-agnostic — both now read `O3K_HOST` (default `localhost`)
- [ ] T033 [P] Run `docs/specs/018-horizon-dashboard-verification/quickstart.md` end-to-end as a reproducibility check (SC-005) and note any drift
- [ ] T034 Final consistency pass — spec / plan / report / runbooks agree; tick the matching boxes on the #482 `## Checklist` comment to reflect completed work

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
3. **STOP and VALIDATE**: a partial report with the auth slice already proves an unmodified Horizon authenticates against O3K — a viable increment.

### Incremental delivery

- Foundational → US1 (auth) → US2 (resources +SSH) → US3 (console verdict/F1) → US4 (publish + reconcile). Each adds report rows without breaking earlier ones.

### Notes

- The console verdict (US3) is **known in advance** (not-working, F1) — the task is to *reproduce and document* it, then spin off the fix as a follow-up (out of scope here, as SPEC-016 → #524).
- Product `charts/o3k` is **not** modified; Horizon ships as the separate `deployments/horizon-chart/`.
- Keep runbook shell blocks zsh-copy-paste-safe (no `$!`/bare `!` in double quotes, no `\` line-continuations, no inline `#`); verify with `zsh -n`.
