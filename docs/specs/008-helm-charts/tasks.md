# Tasks: Helm Charts for Kubernetes Deployment (Single-Node and Multi-Node)

**Input**: Design documents from `docs/specs/008-helm-charts/` (plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md)

**Tests**: REQUIRED — Constitution Article III (Test-First, NON-NEGOTIABLE), adapted to a Helm chart: `helm lint`, `helm template` render/snapshots, **helm-unittest** (value→manifest + fail-fast gate assertions), the `helm test` identity hook (FR-015), and a kind/k3s smoke install. Write the assertions first and confirm they FAIL (RED) before adding the template they cover.

**Delivery scope (spec round-8, 2026-06-11)**: the validated done-line is **single-node stub mode (+ optional TLS)** — that's the MVP (US1). Real-mode and multi-node templates are **authored and render/lint/unittest-clean now**, but their end-to-end success criteria — **SC-003, SC-004, and real-VM login/floating-IP/restart** — are **DEFERRED until the product post-conditions (WS-1/4/5/6, issue [#52](https://github.com/cobaltcore-dev/o3k/issues/52)) ship**. Tasks below tagged **[DEFERRED #52]** author/validate-by-render now; their live e2e validation waits on #52.

## Format: `[ID] [P?] [Story?] Description`

- **[P]**: parallelizable (different files, no dependency on an incomplete task)
- **[Story]**: US1 = single-node one-command; US2 = multi-node; US3 = configure/upgrade/remove
- Paths are concrete; chart root is `charts/o3k/`

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Scaffold the chart and its test tooling.

- [X] T001 [P] Scaffold `charts/o3k/Chart.yaml` (name `o3k`, chart + appVersion, `kubeVersion: ">=1.25.0-0"` per research R3) and `charts/o3k/.helmignore`.
- [X] T002 [P] Create `charts/o3k/values.yaml` with documented defaults (single-node, stub) for every value in data-model.md — `image.repository` empty (no default), `deploymentMode: single-node`, all service modes `stub`, cache off (`cache.enabled: false`, empty `cache.redisUrl`), TLS off.
- [X] T003 [P] Create `charts/o3k/values.schema.json` enforcing types/enums + required-value presence (image, secrets, database) per contracts C1.
- [X] T004 [P] Set up chart test tooling: helm-unittest layout `charts/o3k/tests/`, a kind/k3s CI config, and `Makefile`/CI targets for `helm lint`, `helm template`, `helm unittest` (research R14).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core helpers + the config/secret/service plumbing every story needs.

**⚠️ CRITICAL**: No user-story work begins until this phase is complete.

- [X] T005 [P] helm-unittest gate specs in `charts/o3k/tests/gates_test.yaml` (write FIRST, confirm RED): install `fail`s when `image.repository`, token secret, admin password, or database URL is missing (value or `existingSecret`); **`cache.enabled=true` with an empty `redisUrl` also `fail`s** (FR-006a); minimal valid values render (FR-007/007a/018, contracts C1).
- [X] T006 Implement `charts/o3k/templates/_helpers.tpl`: release-prefixed names/labels (FR-016), `deploymentMode` guards, the install-time fail-gates from T005 (incl. `cache.enabled ⇒ redisUrl` required — FR-006a), per-service probe partial (FR-013), and the config-render + secret-resolution helpers.
- [X] T007 Implement `charts/o3k/templates/secret.yaml`: render the **full** `o3k.yaml` from values into a **Secret** (incl. `keystone.jwt_secret`, `keystone.admin_password`, `database.url`), or reference `existingSecret`; mount at the binary's `--config` path. Wire env `O3K_DB_URL`/`O3K_JWT_SECRET` (from Secret) + `O3K_ENDPOINT_HOST` (research R2 — no generic env layer; Secret-backed file). The rendered file includes `cache.*` (off by default); enabling cache makes Redis a **fail-fast boot dependency** — `pkg/cache.NewCache` Pings Redis at startup and the instance won't start if it is unreachable, so health-gate Redis before O3K (FR-006a).
- [X] T008 Implement `charts/o3k/templates/service.yaml`: a front-door Service for the six API services — identity 35357, compute 8774, placement 8778, network 9696, block storage 8776, image 9292 — `ClusterIP` default (FR-008). **Metadata (8775) is excluded from the load-balanced front-door** (FR-009d); VMs reach it node-locally via the link-local address (FR-009a), so expose it per-node, not via this Service.

**Checkpoint**: chart renders a minimal valid release; gates enforce fail-fast.

---

## Phase 3: User Story 1 — Single-Node Deployment with One Command (Priority: P1) 🎯 MVP

**Goal**: One `helm install` brings up a working single-node O3K (stub) with reachable OpenStack endpoints; optional TLS; single-node real mode is authored.

**Independent Test**: `helm install` (image + DB + secrets) on kind/k3s → all endpoints healthy <5 min → `openstack token issue` succeeds (configured pw; `secret` rejected) → stub server create/list/delete → `helm test` passes.

### Tests for User Story 1 (write first — confirm RED) ⚠️

- [X] T009 [P] [US1] helm-unittest `charts/o3k/tests/singlenode_stub_test.yaml`: single-node stub renders a **Deployment** (unprivileged, no hostNetwork/hostPath → `restricted`-PSA-clean), the config Secret, the Service (7 ports), and the helm-test hook; no privileged/hostPath objects (contracts C2).
- [X] T010 [P] [US1] helm-unittest `charts/o3k/tests/tls_test.yaml`: with `tls.enabled=true`, an Ingress renders and `O3K_ENDPOINT_SCHEME=https` is set (FR-008b); fails when no cert/issuer is supplied.
- [X] T011 [P] [US1] kind/k3s smoke `test/helm_singlenode_stub_test.sh`: install single-node stub → `openstack token issue` works with the configured password and rejects `secret` → `server create/list/delete` (stub) → `helm test` green (SC-001/002 subset, FR-015).

### Implementation for User Story 1

- [X] T012 [US1] Implement `charts/o3k/templates/deployment.yaml`: single-node Deployment; stub = configurable `replicaCount`, unprivileged; real = `replicas=1` + `strategy: Recreate` gated by `realMode.enabled` (FR-003/003a/003b). Mount the config Secret (T007); attach probes (T006). Rely on the binary's startup migration (golang-migrate lock) — **add no migration Job** (FR-012b); readiness gates traffic until the schema is current.
- [X] T013 [US1] Implement `charts/o3k/templates/ingress.yaml` + TLS wiring: optional, per-service-port termination referencing an operator/cert-manager cert; sets `O3K_ENDPOINT_SCHEME=https` (FR-008a/008b, research R5). (depends on T012)
- [X] T014 [P] [US1] Implement `charts/o3k/templates/tests/identity-check.yaml`: `helm test` Job that hits the identity endpoint `/v3` and asserts success (FR-015, contracts C3).
- [X] T015 [P] [US1] Implement `charts/o3k/templates/NOTES.txt`: post-install endpoints, auth hint, `helm test` pointer.
- [X] T016 [US1] Author the real-mode partials in `_helpers.tpl` + `deployment.yaml`: privileged securityContext, `hostNetwork`, hostPath mounts for the host libvirt socket and `hostStoragePath`, and the `privileged` PodSecurity namespace label toggle (`charts/o3k/templates/psa-namespace.yaml`) — FR-012a/012c/012d. **[DEFERRED #52]** real-VM e2e (login/floating-IP/restart) validated only once WS-4/5/6 land.
- [X] T017 [US1] FR-012f packaging: add `dnsmasq` to `build/package/Dockerfile` (keep `iptables`/`iproute2`/`ipset`) so real-mode DHCP works; the chart references the image by tag only.

**Checkpoint**: single-node stub (+ optional TLS) is the **validated MVP**; single-node real is authored (e2e deferred).

---

## Phase 4: User Story 2 — Multi-Node Deployment Across Cluster Nodes (Priority: P2)

**Goal**: Switch to `deploymentMode: multi-node`; a DaemonSet runs one instance per designation-labeled node; instances self-register and coordinate via the shared DB. **Live cross-node behavior is gated on issue #52.**

**Independent Test (now)**: `helm template`/lint/unittest render the multi-node DaemonSet + front-door Service cleanly for stub and real permutations, and the multi-node-real storage gate fires on local backends. **[DEFERRED #52]** live N-active-in-60s (SC-003) and cross-node VM traffic (SC-004).

### Tests for User Story 2 (write first — confirm RED) ⚠️

- [X] T018 [P] [US2] helm-unittest `charts/o3k/tests/multinode_test.yaml`: `deploymentMode=multi-node` renders a **DaemonSet** (nodeSelector = designation label), no Deployment; single front-door Service; `neutron.vxlan_enabled: true` in the rendered config (FR-004/005a/009/009d).
- [X] T019 [P] [US2] helm-unittest `charts/o3k/tests/multinode_storage_cache_gate_test.yaml`: multi-node + real + local Glance/Cinder backend → install `fail`/hard-warn, `rbd`/`s3` passes (FR-012e); and multi-node + `cache.enabled` (with `redisUrl` set) → render warns the Redis must be a single shared instance (FR-006a).
- [X] T020 [P] [US2] **[DEFERRED #52]** e2e suite hookup `test/vxlan_multinode_test.sh` against a chart deployment (SC-003 N-active <60s, SC-004 cross-node traffic) — kept skipped/gated until WS-1 ships; documented as deferred.

### Implementation for User Story 2

- [X] T021 [US2] Implement `charts/o3k/templates/daemonset.yaml`: multi-node DaemonSet with `nodeSelector` on `multiNode.nodeSelectorLabel`; reuse the real-mode partials (T016); label add/remove starts/evicts without chart upgrade (FR-004/005/005a).
- [X] T022 [US2] Multi-node config: set `neutron.vxlan_enabled: true` + `compute.vxlan_port` in the rendered config; document node-to-node UDP 4789 reachability (FR-009, research R11).
- [X] T023 [US2] Front-door Service for multi-node (load-balances the API services across all instances, relies on WS-1 any-instance-serves; **metadata excluded — node-local per FR-009a/009d**) and `multiNode.minProductVersion` value + docs (FR-009d, FR-010). **[DEFERRED #52]** correct cross-node lifecycle needs WS-1; node-local metadata reachability (169.254 redirect) needs WS-4.
- [X] T024 [US2] Storage + cache validation gate in `_helpers.tpl` (the rules T019 covers): require Glance `rbd|s3` + Cinder `rbd` in multi-node real, reject/warn local (FR-012e); and when `cache.enabled` in multi-node, **warn that `redisUrl` must point at a single shared Redis** — invalidation is delete-on-write against the shared keystore (`pkg/cache` `Delete`/`DeletePattern`, no pub/sub), so a per-pod Redis serves stale cross-node reads (FR-006a).
- [X] T025 [US2] `realMode.floatingIpPool` value + MetalLB/routed-subnet documentation for north-south ingress (FR-009c). **[DEFERRED #52]** live floating-IP reachability needs WS-5.

**Checkpoint**: multi-node renders/lints/unittests clean; live multi-node/real e2e gated on #52.

---

## Phase 5: User Story 3 — Configure, Upgrade, and Remove the Deployment (Priority: P3)

**Goal**: Day-2 ops — change a value and upgrade (no data loss), bump the image, uninstall cleanly with an explicit data-retention choice; multiple releases coexist.

**Independent Test**: change a value + `helm upgrade` (change takes effect, resources preserved); `helm uninstall` leaves no workloads; two releases install without collision.

### Tests for User Story 3 (write first — confirm RED) ⚠️

- [X] T026 [P] [US3] helm-unittest `charts/o3k/tests/release_isolation_test.yaml`: all resources are release-prefixed; two releases render without name collisions (FR-016).
- [X] T027 [P] [US3] Scenario test `test/helm_upgrade_uninstall_test.sh`: value change + `helm upgrade` (single-node stub) takes effect; `helm uninstall` removes all release workloads; data-retention flag honored (SC-005/006, FR-014).

### Implementation for User Story 3

- [X] T028 [US3] Upgrade behavior: single-node real `Recreate`, multi-node DaemonSet rolling update; ensure external DB + networked/host storage untouched on upgrade (FR-014).
- [X] T029 [US3] Uninstall + `dataRetentionPolicy` value (retain|delete for hostPath/PVC node-local data); document the external-DB/backends are operator-owned (FR-014).
- [X] T030 [US3] Multi-release isolation pass — verify every resource/name/registration is release-scoped (FR-016).
- [X] T031 [P] [US3] Write `charts/o3k/README.md`: complete values reference — purpose, default, and applicable mode(s) for every value (FR-017, SC-007).

**Checkpoint**: all three user stories independently functional (US2/real e2e per #52).

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T032 [P] Docs: OpenShift SCC equivalent for privileged real mode, and the MetalLB / routed-subnet floating-IP recipe (FR-009c, FR-012d).
- [X] T033 [P] Flag/correct the product config divergence: note in `docs/CONFIGURATION.md` that only `O3K_DB_URL`/`O3K_JWT_SECRET`/`O3K_ENDPOINT_HOST`/`O3K_ENDPOINT_SCHEME` are honored as env (chart uses a Secret-backed `o3k.yaml`); file a product issue to wire a real env layer or fix the docs (research R2). *(Resolved by fixing the docs directly — all env-var sections rewritten to the four honored vars.)*
- [X] T034 Run `quickstart.md` end-to-end for the single-node-stub MVP (+ optional TLS). *(Done 2026-06-12 on minikube/kvm2: `test/helm_singlenode_stub_test.sh` → SMOKE PASS — install, rollout, helm test, configured-password auth + `secret` rejected, stub lifecycle, uninstall. Surfaced and fixed the command/args exec bug (now part of the US2 chart commit). Optional TLS leg remains available via `minikube addons enable ingress`.)*
- [X] T035 Final gate: `helm lint` + `helm template` over all mode permutations render clean; `helm unittest` green; record which suites are **[DEFERRED #52]** so coverage isn't overstated. *(Done 2026-06-12: lint + 17/17 permutation matrix PASSED — 11 renders, 6 gate rejections — and `helm unittest` GREEN: 6 suites / 24 tests. The first unittest run caught a real wart — an empty `env:` key rendered when neither endpointHost nor TLS was set — fixed in `_helpers.tpl`. Deferred suites: `test/helm_multinode_test.sh` (SC-003/004, gated off) and real-VM e2e — issue #52.)*

---

## Dependencies & Execution Order

- **Setup (T001–T004)**: no dependencies.
- **Foundational (T005–T008)**: after Setup — blocks all user stories. T005 (RED) → T006/T007/T008.
- **US1 (T009–T017)**: after Foundational. Tests T009–T011 (RED) → T012 → T013; T014/T015 [P]; T016/T017 (real-mode authoring).
- **US2 (T018–T025)**: after Foundational; reuses US1's real-mode partials (T016). Tests T018–T020 (RED) → T021–T025.
- **US3 (T026–T031)**: after Foundational; exercises US1 (and US2) deployments. Tests T026–T027 (RED) → T028–T031.
- **Polish (T032–T035)**: after the desired stories.

### User-story independence

- **US1 (P1)** is the standalone MVP — fully validated without US2/US3.
- **US2 (P2)** renders/lints independently now; its *live* behavior depends on issue #52 (WS-1/4/5/6), not on US1/US3.
- **US3 (P3)** operates on any installed release; independently testable.

### Parallel opportunities

- All Setup tasks (T001–T004) in parallel.
- T009 ∥ T010 ∥ T011 (US1 tests); T014 ∥ T015 (US1 docs/hooks).
- T018 ∥ T019 ∥ T020 (US2 tests).
- T026 ∥ T027 (US3 tests); T031 [P] alongside US3 impl.
- Once Foundational is done, US1/US2/US3 can proceed in parallel by different people.

## Parallel Example: User Story 1

```bash
# RED tests first, in parallel:
Task: "helm-unittest single-node stub render in charts/o3k/tests/singlenode_stub_test.yaml"   # T009
Task: "helm-unittest TLS render in charts/o3k/tests/tls_test.yaml"                              # T010
Task: "kind smoke install in test/helm_singlenode_stub_test.sh"                                # T011
```

## Implementation Strategy

### MVP first (User Story 1 only)
1. Phase 1 Setup → Phase 2 Foundational.
2. Phase 3 US1: RED tests → Deployment + config/secret + Service + TLS + helm test.
3. **STOP & VALIDATE**: single-node stub (+TLS) — `helm install`, `openstack token issue`, stub lifecycle, `helm test`. Ship the MVP.

### Incremental delivery
1. Setup + Foundational → foundation ready.
2. US1 → validated MVP (single-node stub +TLS).
3. US2 → author multi-node + real-mode templates (render/lint/unittest clean); **e2e validation gated on issue #52**.
4. US3 → day-2 ops.
5. Polish → docs + final gates.

### Deferred (gated on issue #52 / WS-1/4/5/6)
- SC-003 (multi-node N active <60s) and SC-004 (cross-node VM traffic) — T020.
- Real-mode VM usability (cloud-init/login, floating-IP reachability, networking surviving restarts) — T016/T023/T025.
These tasks author and render-validate now; live e2e runs once the post-conditions land.

## Notes

- [P] = different files, no incomplete-task dependency.
- Confirm each helm-unittest assertion FAILS before adding the template it covers (Constitution Article III).
- Config is delivered via a **Secret-backed `o3k.yaml`** (research R2) — do not rely on per-key `O3K_*` env beyond the four honored vars.
- Commit after each task or logical group; stop at any checkpoint to validate a story independently.
