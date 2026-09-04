# Implementation Plan: Ensure Node-Local Storage Directories Exist in Real Mode

**Branch**: `bugfix/510-real-mode-helm-deploy-node-local-storage-subdirs-not-pre-created` (feature id `011-ensure-storage-subdirs`) | **Date**: 2026-06-25 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/011-ensure-storage-subdirs/spec.md` · Source issue [cloud-storage#510](https://github.com/cobaltcore-dev/cloud-storage/issues/510)

## Summary

On a fresh real-mode deploy the node-local base dir (`/var/lib/lightstack`) is created but its
per-service subdirs are not; Glance, finding `/var/lib/lightstack/images` absent, **silently falls back
to `~/.lightstack/images`** (pod-ephemeral), while Nova reads the base image from `/var/lib/lightstack/images`
→ VM boots to ERROR even though `image create` reported success.

Two-layer fix (do both, per the issue):

- **Layer B — binary (root cause):** make Glance create and use its canonical node-local image dir
  instead of the `~/.lightstack` fallback. This is the essential fix: with it, every real-mode service
  `MkdirAll`s its own canonical dir on the hostPath, so the bug is resolved end-to-end.
- **Layer A — chart (defense-in-depth):** a real-mode-gated `initContainer` that `mkdir -p`s all
  five subdirs on the `host-storage` mount before Lightstack starts, so the layout is present and
  inspectable immediately after deploy and is robust to any future service that forgets.

**Key finding from code tracing**: Glance is the *only* outlier. Volumes (`pkg/storage/ceph.go`),
instances (`pkg/hypervisor/disk.go`), cloud-init (`pkg/hypervisor/xml_template.go`), and DHCP
(`pkg/networking/dhcp.go`) already `MkdirAll` their canonical dirs. So FR-006 ("no service may be
an outlier") is satisfied by fixing Glance alone — no other service code changes.

## Technical Context

**Language/Version**: Go 1.26 (binary); Helm 3 / Go-template (chart)
**Primary Dependencies**: stdlib `os`/`path/filepath` + `pkg/paths` (binary); existing chart helpers (`lightstack.podSpec`, `lightstack.image`) — **no new Go or chart dependencies**
**Storage**: node-local hostPath `/var/lib/lightstack` (configurable via `realMode.hostStoragePath`); local image/volume/instance/cloud-init/dhcp trees
**Testing**: `go test -race` (unit, `pkg/storage`); `helm unittest charts/lightstack` (chart render); bash host e2e (`docs/specs/009-single-node-real-mode/HOST-TESTING.md` flow)
**Target Platform**: Linux real mode (single-node k3s validated); stub mode unaffected on any platform
**Project Type**: Single Go binary + Helm chart (no new project)
**Performance Goals**: N/A — one-time directory creation at deploy/startup
**Constraints**: Fail-fast + observability (surface a dir-creation failure, never mask it by relocating data); idempotent; no DB/API/microversion change
**Scale/Scope**: 2 production files (`pkg/storage/image_store.go`, `charts/lightstack/templates/_helpers.tpl`); +1 unit test, +1 chart test; 4 docs de-workaround’d

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Notes |
|-----------|--------|-------|
| I. OpenStack API Compatibility | ✅ Pass | No API surface, response shape, or microversion change. Glance upload already returns success; this makes the stored data actually usable. |
| II. Synchronous Operations | ✅ Pass | Dirs created synchronously (initContainer before main container; `MkdirAll` at store init). No queues/async. |
| III. Fail-Fast Design | ✅ Pass | Glance `MkdirAll` failure is logged (surfaced) rather than masked by a silent `~/.lightstack` relocation; subsequent writes fail loudly at the canonical path. |
| IV. Multi-Mode Architecture | ✅ Pass | initContainer is `realMode.enabled`-gated; stub renders/deploys unchanged. Binary change only affects `local`/local-hybrid store mode. |
| V. Project Isolation | ✅ Pass | Per-project subdirs (`instances/<id>`, `cloud-init/<id>.iso`) unchanged; only their parents are pre-created. |
| VI. Single Binary Deployment | ✅ Pass | No new binary/process; initContainer reuses the same Lightstack image. |

**Result: PASS — no violations, Complexity Tracking not required.**

## Project Structure

### Documentation (this feature)

```text
docs/specs/011-ensure-storage-subdirs/
├── spec.md              # Feature spec (/cl.specify output)
├── plan.md              # This file (/cl.plan output)
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output (on-disk layout contract)
├── quickstart.md        # Phase 1 output (validation steps)
├── contracts/
│   └── contracts.md     # Phase 1 output (testable contracts)
├── checklists/
│   └── requirements.md  # /cl.specify quality checklist
└── tasks.md             # /cl.tasks output (NOT created here)
```

### Source Code (repository root)

```text
pkg/storage/
├── image_store.go            # CHANGE (Layer B): drop the ~/.lightstack fallback in NewImageStore;
│                             #   localPath := LocalImageDir() always; MkdirAll it; log on error.
├── image_store_test.go       # NEW: unit test — local store resolves/creates the canonical dir,
│                             #   never ~/.lightstack (mirrors ceph_test.go).
├── ceph.go                   # reference only — already canonical (LocalVolumeDir → MkdirAll)
└── paths_test.go             # reference only — pins canonical paths

pkg/paths/paths.go            # reference only — canonical dir helpers (ImagesDir, …, DHCPDir)

charts/lightstack/templates/
└── _helpers.tpl              # CHANGE (Layer A): add initContainers (realMode-gated) to
                              #   lightstack.podSpec — mkdir -p the 5 subdirs on the host-storage mount.
                              #   Covers BOTH deployment.yaml and daemonset.yaml (both include it).

charts/lightstack/tests/
└── init_host_storage_test.yaml  # NEW: helm-unittest — initContainer present iff realMode.enabled,
                                 #   creates all 5 subdirs under hostStoragePath, mounts host-storage.

# Docs — remove the manual `mkdir -p /var/lib/lightstack/{...}` workaround (SC-004):
docs/specs/009-single-node-real-mode/HOST-TESTING.md
docs/SINGLE_NODE_DEPLOYMENT.md
docs/STORAGE_MODES.md
deployments/README-KVM.md
```

**Structure Decision**: No new packages or projects — a one-line behavioral fix in the existing
storage package plus an additive block in the existing shared chart helper. Tests live beside the
code they cover (`pkg/storage`, `charts/lightstack/tests`), matching repo convention.

## Complexity Tracking

> No Constitution violations — section intentionally empty.
