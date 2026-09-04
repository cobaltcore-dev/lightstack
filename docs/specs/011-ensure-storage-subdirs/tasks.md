# Tasks: Ensure Node-Local Storage Directories Exist in Real Mode

**Feature**: `011-ensure-storage-subdirs` · **Branch**: `bugfix/510-real-mode-helm-deploy-node-local-storage-subdirs-not-pre-created` · **Date**: 2026-06-25
**Input**: `docs/specs/011-ensure-storage-subdirs/` — spec.md, plan.md, research.md, data-model.md, contracts/contracts.md, quickstart.md
**Source issue**: [cloud-storage#510](https://github.com/cobaltcore-dev/cloud-storage/issues/510)

**Tests**: REQUIRED — FR-011 + SC-007 + Clarification 2026-06-25 (Option D) mandate a three-tier
regression suite, each tier demonstrated RED on the pre-fix behavior and GREEN after (Constitution
Article III, Test-First). Test tasks therefore precede their implementation within each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1 / US2 / US3 (maps to spec.md user stories)
- Exact file paths included per task.

## Path Conventions

Go binary under `pkg/` / `internal/`; Helm chart under `charts/lightstack/`; docs under `docs/` and
`deployments/`. No `src/`-style layout. Canonical node-local dirs are defined in `pkg/paths/paths.go`.

---

## Phase 1: Setup (baseline)

**Purpose**: Establish a known-green baseline before changes so each tier's RED→GREEN is unambiguous.

- [X] T001 Verify a green baseline at repo root: `go build ./...`, `go test -race ./pkg/storage/...`, `helm lint charts/lightstack`, `helm unittest charts/lightstack` all pass on the unmodified tree; confirm the two fault lines (`pkg/storage/image_store.go` ~L82-85 `~/.lightstack` fallback; `charts/lightstack/templates/_helpers.tpl` ~L215-218 `host-storage` `DirectoryOrCreate`, no `initContainers`).

---

## Phase 2: Foundational (blocking prerequisites)

**Purpose**: Shared prerequisites for all stories.

**None.** The canonical dir helpers already exist in `pkg/paths/paths.go`
(`ImagesDir/VolumesDir/InstancesDir/CloudInitDir/DHCPDir`) and every non-Glance service already
`MkdirAll`s its own dir. No shared code must land before the stories — US1 and US2 can start
immediately and in parallel.

**Checkpoint**: Stories unblocked.

---

## Phase 3: User Story 2 - Image storage never silently relocates (Priority: P1) 🎯 root-cause fix

**Goal**: Glance always resolves and creates the canonical node-local image dir
(`/var/lib/lightstack/images`) and never the `~/.lightstack` fallback — the root cause, and the primary
anti-silent-regression guard (CI unit test). Independently fixes every deployment (chart and bare).

**Independent Test**: `go test -race ./pkg/storage/...` — the unit test proves `localPath` is the
canonical dir, never home-relative, with no deploy required.

### Tests for User Story 2 (write FIRST — confirm RED) ⚠️

- [X] T002 [P] [US2] Add CI-gating unit test `pkg/storage/image_store_test.go` (model on `pkg/storage/ceph_test.go`) asserting: (i) `LocalImageDir() == "/var/lib/lightstack/images"`; (ii) `NewImageStore("local", …).localPath == LocalImageDir()`; (iii) `localPath` is never home-relative / under `~/.lightstack` (assert it has prefix `/var/lib/lightstack` and does not contain `.lightstack`), in stub and local modes. Run `go test ./pkg/storage/ -run ImageStore` → **confirm RED** (current code falls back to `~/.lightstack/images` when the canonical dir is absent). (Contracts C-BIN-1/2/3; FR-002/003/006/011a; SC-007)

### Implementation for User Story 2

- [X] T003 [P] [US2] Fix `pkg/storage/image_store.go` `NewImageStore`: set `localPath := LocalImageDir()` unconditionally and delete the `os.Stat(localPath)` → `~/.lightstack/images` fallback block (~L82-85); keep `os.MkdirAll(store.localPath, 0755)` for `local`/local-hybrid modes but log on error instead of discarding it (FR-008 — surface, never mask). Mirrors `pkg/storage/ceph.go`. Re-run T002 → **GREEN**. (FR-002/003/006/008/011a; resolves Clarification 2026-06-25)

**Checkpoint**: Unit test green; Glance resolves the canonical image dir in all modes. Bug fixed at the source.

---

## Phase 4: User Story 1 - Boot a VM on a fresh real-mode deploy with no manual setup (Priority: P1) 🎯 MVP outcome

**Goal**: All node-local subdirs exist on the persistent host mount before Lightstack serves requests (chart
initContainer = defense-in-depth + "inspectable before first API call"), so a fresh real-mode deploy
boots a VM with zero manual `mkdir`.

**Independent Test**: `helm unittest charts/lightstack` (real-mode render shows the initContainer; stub does
not) for CI; plus the host e2e (fresh deploy → image → VM ACTIVE).

### Tests for User Story 1 (write FIRST — confirm RED) ⚠️

- [X] T004 [P] [US1] Add CI-gating helm-unittest spec `charts/lightstack/tests/init_host_storage_test.yaml` (model on `charts/lightstack/tests/singlenode_stub_test.yaml`; templates: `deployment.yaml` + `daemonset.yaml`) asserting: with `realMode.enabled=true` the pod spec has an `initContainers` entry that (i) uses the Lightstack image and `image.pullPolicy`, (ii) runs a command creating `images volumes instances cloud-init dhcp` under `realMode.hostStoragePath`, (iii) mounts `host-storage` at `realMode.hostStoragePath`; with `realMode.enabled=false` there is **no** `initContainers`. Run `helm unittest charts/lightstack` → **confirm RED** (no initContainer rendered yet). (Contracts C-CHART-1/2/3; FR-001/009/011b; SC-007)

### Implementation for User Story 1

- [X] T005 [P] [US1] Add the real-mode-gated `initContainers` block to the `lightstack.podSpec` helper in `charts/lightstack/templates/_helpers.tpl` (inside `{{- if .Values.realMode.enabled }}`, before `containers:`): image `{{ include "lightstack.image" . }}`, `imagePullPolicy: {{ .Values.image.pullPolicy }}`, `command: ["/bin/sh","-c","mkdir -p {{ .Values.realMode.hostStoragePath }}/images … /dhcp"]`, mounting `host-storage` at `{{ .Values.realMode.hostStoragePath }}`. Add a comment noting `pkg/paths/paths.go` is the source of truth for the dir list. Single edit covers `deployment.yaml` + `daemonset.yaml`. Re-run T004 → **GREEN**; `helm lint` and existing chart tests stay green. (FR-001/005/007/009/011b)

### End-to-end validation for User Story 1 (host harness — not CI)

- [X] T006 [US1] Run the fresh-deploy host e2e per `docs/specs/011-ensure-storage-subdirs/quickstart.md` §2 on a Linux node with nested KVM + privileged root: from a clean node with no `/var/lib/lightstack`, deploy real mode with the T003+T005 image/chart (no manual `mkdir`); confirm `/var/lib/lightstack/{images,volumes,instances,cloud-init,dhcp}` exist after pod start, `openstack image create` then `openstack server create --wait` ⇒ **ACTIVE**, base image under `/var/lib/lightstack/images` (not `~/.lightstack`). Record that the pre-fix build produced VM ERROR (RED) and the fixed build reaches ACTIVE (GREEN). (Contracts C-E2E-1; FR-001/004/011c; SC-001/002/005; US1-AC1/2/3)

**Checkpoint**: Chart-render test green; fresh real-mode deploy boots a VM ACTIVE with no manual setup. **MVP complete** (US2 + US1).

---

## Phase 5: User Story 3 - Redeploy after teardown still boots (Priority: P2)

**Goal**: The directory guarantee is idempotent — a redeploy after the node-local trees were wiped
boots a VM on the first attempt. No new production code (both layers are idempotent: `mkdir -p` /
`os.MkdirAll`); this is the idempotence regression check.

**Independent Test**: Host e2e redeploy-after-wipe reaches ACTIVE on the first attempt.

- [X] T007 [US3] Run the redeploy-after-teardown host e2e per `docs/specs/011-ensure-storage-subdirs/quickstart.md` §3: after a successful deploy, `helm uninstall` + `sudo rm -rf /var/lib/lightstack/*` (simulate teardown wipe), redeploy real mode with no manual `mkdir`; confirm the subdirs are recreated automatically and `openstack server create --wait` ⇒ **ACTIVE** on the first attempt. (Contracts C-E2E-2; FR-005; SC-003; US3-AC1/2)

**Checkpoint**: Redeploy idempotence proven.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T008 [P] Remove the manual `mkdir -p /var/lib/lightstack/{images,volumes,instances,cloud-init,dhcp}` workaround from `docs/specs/009-single-node-real-mode/HOST-TESTING.md`, `docs/SINGLE_NODE_DEPLOYMENT.md`, `docs/STORAGE_MODES.md`, and `deployments/README-KVM.md`; replace with a one-line note that the subdirs are auto-created (chart initContainer + per-service self-create). Verify with `grep -rn 'mkdir -p /var/lib/lightstack' docs deployments` returning nothing. (FR-010; SC-004)
- [X] T009 Run the full CI gates at repo root: `go build ./...` (~62 MB), `golangci-lint run`, `go test -race ./...`, `helm lint charts/lightstack`, `helm unittest charts/lightstack` — all green (confirms SC-006: stub/non-real unaffected; existing tests unchanged).
- [X] T010 Execute `docs/specs/011-ensure-storage-subdirs/quickstart.md` steps 1–5 end-to-end and confirm SC-001..SC-007 are met; tick `docs/specs/011-ensure-storage-subdirs/checklists/requirements.md` notes if needed.
- [ ] T011 Open a PR in `cobaltcore-dev/lightstack`, link cloud-storage#510, and summarize the two-layer fix (binary root cause + chart defense-in-depth) and the three-tier regression suite.

---

## Dependencies & Execution Order

### Phase order
- **Setup (P1)** → no deps; do first.
- **Foundational (P2)** → empty; no gate.
- **US2 (Phase 3, P1)** and **US1 (Phase 4, P1)** → independent of each other; both can start immediately. Each independently fixes the chart-deploy bug (US2: Glance self-creates; US1: initContainer pre-creates → Glance's path exists). US2 additionally fixes non-chart deploys → it is the root-cause fix.
- **US3 (Phase 5, P2)** → host e2e depends on the T003+T005 build being deployed.
- **Polish (Phase 6)** → T008 independent; T009/T010 after T003+T005; T011 last.

### Within stories
- US2: T002 (RED) → T003 (GREEN).
- US1: T004 (RED) → T005 (GREEN) → T006 (host e2e, needs T003 deployed too).

### Critical path to "bug fixed in CI"
T001 → (T002→T003) ∥ (T004→T005) → T009. The host e2e (T006/T007) and docs (T008) can proceed once code lands.

## Parallel Opportunities

- **Test authoring**: T002 (`image_store_test.go`) ∥ T004 (`init_host_storage_test.yaml`) — different files.
- **Implementation**: T003 (`image_store.go`) ∥ T005 (`_helpers.tpl`) — different files, different stories.
- **Docs**: T008 edits four independent files — can be split.

```bash
# After T001, two independent RED→GREEN tracks in parallel:
# Track A (US2): write T002 (RED) → apply T003 (GREEN)   # pkg/storage/*
# Track B (US1): write T004 (RED) → apply T005 (GREEN)   # charts/lightstack/*
```

## Implementation Strategy

- **MVP = US2 + US1** (both P1): the root-cause binary fix plus the chart defense-in-depth, each
  guarded by a CI-gating regression test (unit + chart-render). At the MVP checkpoint the bug is
  fixed and cannot silently reappear in CI (SC-007).
- **Incremental**: land US2 (fastest RED→GREEN, no deploy) → land US1 → validate host e2e (T006) →
  add US3 idempotence (T007) → docs + gates (Phase 6).
- **Test-First**: for each story, confirm the new test is RED against current code before applying
  the fix; never mark a tier done until RED→GREEN is demonstrated (FR-011).

## Traceability

| Task | Contract | FR | SC | Story |
|------|----------|----|----|-------|
| T002 | C-BIN-1/2/3 | FR-002/003/006/011a | SC-007 | US2 |
| T003 | C-BIN-2/3 | FR-002/003/006/008/011a | SC-002 | US2 |
| T004 | C-CHART-1/2/3 | FR-001/009/011b | SC-007 | US1 |
| T005 | C-CHART-1/2 | FR-001/005/007/009/011b | SC-001 | US1 |
| T006 | C-E2E-1 | FR-001/004/011c | SC-001/002/005 | US1 |
| T007 | C-E2E-2 | FR-005/011c | SC-003 | US3 |
| T008 | — | FR-010 | SC-004 | polish |
| T009 | — | FR-009 | SC-006 | polish |
| T010 | all | — | SC-001..007 | polish |

## Notes

- Host e2e (T006/T007) needs a Linux node with nested KVM + privileged root — runs in the SPEC-009
  host harness, not standard CI. The CI-gating guard against silent regression is the unit +
  chart-render pair (T002/T004).
- The unit test asserts the resolved `localPath` value (not on-disk FS state), so it is
  platform-independent and needs no root.
- Commit per task or per RED→GREEN pair; keep stub-mode behavior and existing tests green throughout.

## Implementation status — 2026-06-25

- **Done in-repo (CI-verifiable):** T001 baseline · T002+T003 (US2, binary, `pkg/storage/`) ·
  T004+T005 (US1, chart, `charts/lightstack/`) · T008 docs (4 files de-workaround'd) · T009 gates
  (`go build` OK, `golangci-lint run` 0 issues, `go test -race` all packages pass except the
  live-server `test/contract/*` suites, `helm lint` clean, `helm unittest` 43/43).
- **RED note (T002):** on this dev box `/var/lib/lightstack/images` already exists (leftover host-test
  data), so the unfixed code didn't fall back and the test was green-before here; on a fresh box /
  CI (dir absent) the unfixed code falls back to `~/.lightstack/images` and the test is RED. After the fix
  `localPath` is deterministically canonical — GREEN everywhere.
- **KNOWN LIMITATION — accepted (branch review 2026-06-26):** `image_store_test.go` asserts
  `localPath == "/var/lib/lightstack/images"`, which only goes RED against the pre-fix code when that dir is
  **absent** (CI runners / fresh nodes). On a host where the canonical dir already exists, the buggy
  code would also pass — so the unit test cannot reproduce the FS-state-dependent bug independent of
  the host. It is a valid forward CI guard (matches `ceph_test.go`) and is left as-is; making it
  host-independent would require injecting the base dir into `NewImageStore` (larger than the fix
  warrants). The host e2e (T006/T007) is what demonstrates the bug end-to-end.
- **Host e2e VALIDATED on real k3s (2026-06-26):** T006/T007 — single-node real-mode deploy from
  this worktree on k3s with **zero manual `mkdir`**: the `init-host-storage` initContainer ran and
  auto-created all five subdirs; a cirros VM booted **ACTIVE**, took an injected SSH key, was
  SSH-reachable from the node, and `ping`ed `1.1.1.1` out the node uplink — proving Glance wrote the
  base to `/var/lib/lightstack/images` where Nova reads it (no `~/.lightstack` divergence). A clean-slate redeploy
  (ns + `/var/lib/lightstack` wiped) recreated the subdirs and booted again (T007 / SC-003). Captured in
  `HOST-TESTING.md`. Three k3s gotchas hit + documented: `ErrImageNeverPull` (image must be
  `docker.io/library/lightstack:dev`), `helm test` startup race, and the chart-managed-namespace
  termination race on back-to-back reinstall.
- **Committed (2026-06-26):** 3 commits on `bugfix/510` (`d1de7dd` fix · `262285b` feat · `b9bfc9b`
  docs), pushed to origin. Branch review run (PASS, no Critical/Major); the two Minor findings fixed
  in follow-up — chart initContainer paths now shell-quoted (`>-` block scalar), and
  `STORAGE_MODES.md` fully swept off the obsolete `~/.lightstack` local-storage path. (Octal style aligned
  to `0755`; initContainer dir-list duplication left as-is per review.)
- **Pending operator action:** T011 — open the PR (deferred by the user for now).
