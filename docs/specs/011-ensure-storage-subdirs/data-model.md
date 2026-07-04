# Phase 1 Data Model: Ensure Node-Local Storage Directories Exist in Real Mode

**Feature**: `011-ensure-storage-subdirs` · **Date**: 2026-06-25

No database entities, tables, or migrations are involved. The "data model" for this feature is the
**on-disk node-local directory layout** — the contract every real-mode service relies on. Canonical
paths are defined once in `pkg/paths/paths.go`.

## Node-local storage layout

Base: `realMode.hostStoragePath` (default `/var/lib/o3k`), mounted into the pod as the
`host-storage` hostPath volume.

| Subdir (`pkg/paths`) | Path | Owner service | Holds | Created by (after fix) |
|----------------------|------|---------------|-------|------------------------|
| `ImagesDir()` | `<base>/images` | Glance | VM base images (`image-<id>.raw`) | **Glance store init (FIXED)** + initContainer |
| `VolumesDir()` | `<base>/volumes` | Cinder (local) | local-mode block volumes | `ceph.go` (already) + initContainer |
| `InstancesDir()` | `<base>/instances` | Nova | per-instance overlay disks (`<id>/`) | `disk.go EnsureInstanceDir` (already) + initContainer |
| `CloudInitDir()` | `<base>/cloud-init` | Nova/metadata | per-instance config-drive ISOs (`<id>.iso`) | `xml_template.go` (already) + initContainer |
| `DHCPDir()` | `<base>/dhcp` | Neutron | dnsmasq leases/configs/pids | `dhcp.go` (already) + initContainer |

## Invariants (the contract)

- **INV-1 (single source of truth)**: A given image's bytes are written by Glance and read by Nova at
  **the same** path (`<base>/images/image-<id>.raw`). No component may resolve a different root.
  *(Was violated: Glance → `~/.o3k/images`, Nova → `<base>/images`.)*
- **INV-2 (existence before serve)**: In real mode, all five subdirs exist before the platform serves
  requests (initContainer guarantees up-front; each service's `MkdirAll` guarantees on first write).
- **INV-3 (persistence)**: All subdirs live under the `host-storage` hostPath, so contents survive
  pod/process restart (not pod-ephemeral `~/.o3k`).
- **INV-4 (idempotence)**: Creating the layout when some/all dirs already exist is a no-op that
  preserves existing contents (`mkdir -p` / `os.MkdirAll` semantics).
- **INV-5 (mode isolation)**: In stub / non-real mode, none of these dirs are required and none are
  created by the chart (the initContainer is `realMode.enabled`-gated).

## State transitions

Per directory, the only transition is **absent → present** (idempotent; never present → absent —
removal is the teardown feature's concern, not this one). No other lifecycle.
