# Phase 0 Research: Ensure Node-Local Storage Directories Exist in Real Mode

**Feature**: `011-ensure-storage-subdirs` · **Date**: 2026-06-25

The spec carried no `[NEEDS CLARIFICATION]` markers. "Research" here is code archaeology to pin the
exact fault lines and choose the minimal correct fix. All findings are from the current tree on
`bugfix/510-…`.

## Finding 0 — Reproduction & fault localization (confirmed)

- Glance store init: `pkg/storage/image_store.go:81-85` — `localPath := LocalImageDir()`
  (`/var/lib/o3k/images`); **if it does not exist**, `localPath` is reassigned to
  `~/.o3k/images`. The subsequent `os.MkdirAll(store.localPath, …)` (`:101`) then creates the
  *fallback*, not the canonical dir.
- Nova read path: per-instance qcow2 overlay is created over the base at
  `paths.ImagesDir()/image-<id>.raw` (`/var/lib/o3k/images/…`). Write (Glance → `~/.o3k`) and read
  (Nova → `/var/lib/o3k`) diverge → "Could not open backing image" → VM ERROR.
- Chart: `charts/o3k/templates/_helpers.tpl:215-218` — `host-storage` hostPath uses
  `type: DirectoryOrCreate`, which materializes only `/var/lib/o3k`, and the mount shadows the
  image's pre-created subdirs. No `initContainers` block exists in `o3k.podSpec`.

## Decision 1 — Two layers; Layer B is the root-cause fix, Layer A is defense-in-depth

- **Decision**: Fix the binary (Glance) **and** add a chart initContainer, but treat the binary fix
  as the essential one.
- **Rationale**: With Glance creating its canonical dir, **every** real-mode service `MkdirAll`s its
  own canonical dir on the hostPath mount, so the trees materialize on node-persistent storage and
  the bug is resolved even without the chart change. The initContainer adds value the binary cannot:
  the layout exists *before the first API call* (satisfies US1-AC2 "operator inspects and sees the
  dirs"), and it guards against any future service that forgets to self-create.
- **Alternatives considered**:
  - *Chart-only* (initContainer, no binary change): rejected — leaves the silent `~/.o3k` fallback
    latent for non-chart deploys (bare binary / service manager) and any path where the dir is
    absent at init. Doesn't fix the root cause.
  - *Binary-only* (no initContainer): viable and minimal, but the issue explicitly requests both,
    and it loses the "inspectable immediately after deploy" property. Kept the initContainer.

## Decision 2 — Glance: remove the `~/.o3k` fallback entirely (don't make it conditional)

- **Decision**: `NewImageStore` sets `localPath := LocalImageDir()` unconditionally and `MkdirAll`s
  it; delete the `os.Stat`→`~/.o3k/images` block.
- **Rationale**: No other service has a `~/.o3k` fallback — `pkg/storage/ceph.go:40,72`
  (`LocalVolumeDir`→`MkdirAll`) is the model, and Nova always reads `/var/lib/o3k/images`. The
  fallback was therefore **always** a divergence (even for "non-containerized" dev, Glance would
  disagree with Nova). Removing it makes Glance consistent. This mirrors the exact fix already
  applied to volumes for SPEC-009 FR-006a (see `ceph.go` comment + `ceph_test.go`).
- **Alternatives considered**:
  - *Keep fallback only when canonical `MkdirAll` fails*: rejected — reintroduces silent divergence
    (data lands where Nova won't read). FR-002/FR-003/FR-008 forbid silent relocation.
  - *Centralized `paths.EnsureAll()` called from `main` in real mode*: attractive (single place for
    the dir list) but unnecessary — every service already self-creates, so this would be dead
    belt-and-suspenders inside the binary. The initContainer already provides the up-front guarantee.
    Rejected on YAGNI/Simplicity.

## Decision 3 — Layer A: initContainer runs `mkdir -p`, reusing the o3k image (no new CLI subcommand)

- **Decision**: Add an `initContainers` entry to `o3k.podSpec`, gated on `.Values.realMode.enabled`,
  image `{{ include "o3k.image" . }}` / `imagePullPolicy: {{ .Values.image.pullPolicy }}`, command
  `["/bin/sh","-c","mkdir -p <5 dirs under hostStoragePath>"]`, mounting `host-storage` at
  `{{ .Values.realMode.hostStoragePath }}`.
- **Rationale**: Editing the shared `o3k.podSpec` helper covers **both** `deployment.yaml`
  (single-node) and `daemonset.yaml` (multi-node) in one place — both already
  `include "o3k.podSpec"`. Reusing the o3k image avoids a second image to pull/trust. In real mode
  the pod is privileged and runs as root, so `mkdir` and subsequent o3k writes share ownership; no
  `fsGroup`/`runAsUser` juggling. The project's actual constitution (`.specify/memory/constitution.md`,
  6 principles) has **no** "CLI Interface" article, so a dedicated `o3k init-storage` subcommand is
  not required.
- **Alternatives considered**:
  - *New `o3k init-storage` CLI subcommand invoked by the initContainer*: would keep the dir list in
    `pkg/paths` (DRY across binary and chart) — but adds Go code and a command surface the
    constitution doesn't demand; the chart already hardcodes the base path in `values.yaml`, so
    listing its children there is consistent. Rejected on Simplicity; mitigated by a template
    comment pointing at `pkg/paths`.
  - *Postpone subdir creation to an emptyDir/PVC redesign*: out of scope (node-local hostPath is the
    SPEC-009/008 design).

## Decision 4 — Test strategy (RED → GREEN, Constitution "Test-First")

- **Decision**:
  - **Unit** (`pkg/storage/image_store_test.go`): assert `LocalImageDir()` == `/var/lib/o3k/images`
    and that `NewImageStore("local", …)` resolves `localPath` to the canonical dir, never a
    `~/.o3k`/home-relative path — modeled on `ceph_test.go`. (No real FS dependency needed for the
    path-resolution assertion; this stays platform-independent.)
  - **Chart** (`charts/o3k/tests/init_host_storage_test.yaml`, helm-unittest): real-mode render
    contains an initContainer that creates all five subdirs under `hostStoragePath` and mounts
    `host-storage`; stub render contains no such initContainer.
  - **Host e2e** (manual, documented in quickstart): fresh real-mode deploy → image create → server
    create `--wait` ⇒ ACTIVE with zero manual `mkdir`; then wipe trees + redeploy ⇒ ACTIVE again.
- **Rationale**: Matches existing conventions (`pkg/storage/*_test.go`, `charts/o3k/tests/*.yaml`)
  and the repo's CI gates (`go test -race`, `helm unittest`). Host e2e covers SC-001/003/005 which
  can't run in unit/CI (needs nested KVM + privileged root).
- **Alternatives considered**: gophercloud contract test — unnecessary; no API contract changes.

## Resolved unknowns

| Question | Resolution |
|----------|-----------|
| Which services self-create their dir? | volumes/instances/cloud-init/dhcp already do; **only Glance/images is broken**. |
| Remove or keep the `~/.o3k` fallback? | Remove entirely (Decision 2). |
| One chart edit for both deploy modes? | Yes — `o3k.podSpec` is shared by deployment + daemonset (Decision 3). |
| New binary/CLI/DB/migration needed? | No — behavioral one-liner + additive chart block; no schema, no API. |
| Does the o3k image have a shell for `mkdir`? | Yes (Debian-based, ships qemu-img/genisoimage per SPEC-009); `/bin/sh -c` is fine. |
