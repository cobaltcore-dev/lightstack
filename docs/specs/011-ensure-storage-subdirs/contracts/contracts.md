# Phase 1 Contracts: Ensure Node-Local Storage Directories Exist in Real Mode

**Feature**: `011-ensure-storage-subdirs` · **Date**: 2026-06-25

This feature exposes **no new API, CLI, or DB interface**. The verifiable contracts are (1) the
binary's local-image-store path resolution and (2) the chart's rendered pod spec. Each maps to a
test and to spec requirements.

## Binary contracts (unit — `pkg/storage/image_store_test.go`)

- **C-BIN-1 → FR-002, FR-003, INV-1**: `LocalImageDir()` returns `/var/lib/lightstack/images`.
- **C-BIN-2 → FR-002, FR-008**: `NewImageStore("local", …)` sets `localPath` to the canonical
  `LocalImageDir()`; `localPath` is never a home-relative / `~/.lightstack` path, regardless of whether the
  canonical dir pre-exists.
- **C-BIN-3 → FR-006**: behavior matches the existing volume contract (`ceph_test.go`:
  `LocalVolumeDir()` / `CephClient.localPath` == `/var/lib/lightstack/volumes`) — Glance is no longer an
  outlier.

## Chart contracts (helm-unittest — `charts/lightstack/tests/init_host_storage_test.yaml`)

Rendered via the shared `lightstack.podSpec` (covers `deployment.yaml` and `daemonset.yaml`).

- **C-CHART-1 → FR-001, US1-AC2**: with `realMode.enabled=true`, the pod spec contains an
  `initContainers` entry that:
  - uses the Lightstack image and the configured `image.pullPolicy`;
  - runs a command that `mkdir -p`s `images`, `volumes`, `instances`, `cloud-init`, `dhcp` under
    `realMode.hostStoragePath`;
  - mounts the `host-storage` volume at `realMode.hostStoragePath`.
- **C-CHART-2 → FR-009, INV-5**: with `realMode.enabled=false` (stub), the pod spec has **no**
  `initContainers` (and continues to render an unprivileged spec — existing assertions unaffected).
- **C-CHART-3 → FR-001 (config drift)**: the initContainer's dir list equals the five `pkg/paths`
  subdirs; a template comment references `pkg/paths/paths.go` so the two stay in sync.

## End-to-end contract (host e2e — see quickstart.md)

- **C-E2E-1 → SC-001/002/005, FR-004**: on a fresh real-mode deploy (no manual `mkdir`),
  `image create` then `server create --wait` reaches **ACTIVE**, and the base image resides at
  `<base>/images/` (not `~/.lightstack`).
- **C-E2E-2 → SC-003, US3**: after removing the node-local trees, a redeploy reaches **ACTIVE**
  again with no manual remediation (idempotence).

## Out-of-contract (explicitly unchanged)

- Glance image API responses / status codes; Nova/Cinder/Neutron APIs; microversions; DB schema.
- Non-local storage backends (`rbd`, `s3`) beyond their existing behavior.
