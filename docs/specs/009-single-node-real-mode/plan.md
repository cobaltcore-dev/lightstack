# Implementation Plan: Single-Node Real Mode (v1)

**Branch**: `bugfix/503-single-node-real-mode-v1` | **Date**: 2026-06-18 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/009-single-node-real-mode/spec.md` (cloud-storage#503, subtask of #476 / o3k#52)

## Summary

Make a real-mode O3K deployment usable end-to-end on a single Linux node. The feature is delivered in dependency order **WS-0 → WS-2 → WS-4 → WS-3**, plus real-mode **block storage (WS-5)**:

- **WS-0** — real VMs boot from a Glance image via a **per-instance qcow2 overlay over a raw base**; images normalized to raw on ingest; image deletion blocked while a dependent disk exists; core power lifecycle + serial console.
- **WS-2** — a startup + periodic **network-state reconciler** (modeled on the existing VXLAN coordinator) rebuilds DHCP/namespaces/bridges/SG rules from the DB so VMs survive a control-plane (pod) restart; all VM disk artifacts persist on the node hostPath.
- **WS-4** — the metadata service binds a VM-reachable address with a per-namespace `169.254.169.254` redirect (namespace/project-aware identification); `user_data` is accepted/persisted/served; config-drive works end-to-end.
- **WS-3** — **reuse** the existing router SNAT + floating-IP DNAT code, auto-wiring the external network to the node uplink; security groups enforced on north-south.
- **WS-5** — real-mode volume create/attach/detach (made **synchronous**, fixing the current async goroutine) and **boot-from-volume**.

Technical approach: extend the existing single binary (`cmd/o3k/main.go` + `internal/*`, `pkg/*`); add one DB migration (`060`); add `qemu-img` + `genisoimage` to the reference image; fix the node-local storage path. No new services, no queues. Full design rationale in [research.md](./research.md); schema/entities in [data-model.md](./data-model.md); behavioral contracts in [contracts/](./contracts/); validation runbook in [quickstart.md](./quickstart.md).

## Technical Context

**Language/Version**: Go 1.26 (stdlib + minimal deps; existing module)
**Primary Dependencies**: Gin (HTTP), pgx (PostgreSQL), `digitalocean/go-libvirt` (hypervisor), `vishvananda/netlink` (netns/bridges), `coreos/go-iptables` (NAT/SG), `ceph/go-ceph` (RBD, `-tags ceph`), `aws-sdk-go-v2` (S3); **gophercloud** for contract tests. Invoked host tools (not Go deps): `qemu-img`, `genisoimage`, `dnsmasq`, `iptables`, `ip` (iproute2).
**Storage**: PostgreSQL 18 (control-plane state); node-local hostPath `/var/lib/o3k` for real-mode data — `images/` (raw bases), `instances/<id>/` (per-instance overlays), `volumes/` (local volumes), `cloud-init/` (config-drive ISOs); RBD/S3 optional backends.
**Testing**: `go test` (unit, stub-mode logic); gophercloud contract tests under `test/contract/`; bash integration under `test/` (OpenStack CLI); real-mode e2e on a Linux host (cannot run in a non-Linux sandbox).
**Target Platform**: Linux single node with KVM + host libvirtd for real mode (stub mode is cross-platform and remains the default); deployed via the SPEC-008 Helm chart on single-node k3s.
**Project Type**: Single Go binary (monolith) — all services constructed in `cmd/o3k/main.go`; no microservices.
**Performance Goals**: VM create < 10 s (real mode); metadata responses < 100 ms; external-dependency calls fail fast < 1 s; reconciler resync on a configurable interval (default 30 s).
**Constraints**: 100% OpenStack Flamingo (2025.2) API/CLI/Terraform compatibility; synchronous operations (no queues/async state machines); IPv4-only (v1); single node (no cross-node placement/routing).
**Scale/Scope**: One node; concurrent VM/volume count bounded by host resources (no scheduler/overcommit). 5 user stories, 31 FRs, 10 SCs. ~6 internal packages + 2 pkg/ packages touched + 1 migration + Dockerfile.

## Constitution Check

*GATE: evaluated against `.specify/memory/constitution.md` v1.0.0. Re-checked after Phase 1 design.*

| Principle | Status | Notes |
|-----------|--------|-------|
| **I. OpenStack Flamingo API compat (NON-NEGOTIABLE)** | ✅ PASS | No proprietary API surface added. `user_data` and `block_device_mapping_v2` are standard Nova fields; floating IPs / volumes / console reuse existing OpenStack-compatible endpoints. Contract tests (gophercloud + `openstack` CLI + a Terraform apply) gate every story. |
| **II. Synchronous operations** | ✅ PASS (with a fix) | The current volume-attach spawns an **async goroutine** for the libvirt call (`internal/nova/volume_attachment.go:126-167`) — a violation. This plan makes attach **synchronous + fail-fast** (revert DB on libvirt error). The startup/periodic **reconciler** is a reconciliation loop (like the accepted VXLAN coordinator), not an async path for an API request — API operations themselves stay synchronous. |
| **III. Fail-fast design** | ✅ PASS | Overlay staging, libvirt attach/detach, metadata DNAT, image conversion all fail fast (< 1 s for external deps). Global startup dep checks (DB ping, libvirt connect) stay fail-fast and run **before** reconciliation. Per-tenant reconciliation is best-effort (isolate + retry) — see Complexity Tracking. |
| **IV. Multi-mode architecture** | ✅ PASS | Every change preserves stub mode and keeps it the default (FR-020). Real-mode code paths are mode-gated; the reconciler only runs when networking mode ≠ stub. |
| **V. Project isolation** | ✅ PASS | Metadata identification is namespace/project-aware (FR-010); per-project network namespaces; all queries scoped by `project_id`. |
| **VI. Single binary** | ✅ PASS | All wiring stays in `cmd/o3k/main.go`; the reconciler is an in-process goroutine. `qemu-img`/`genisoimage`/`dnsmasq` are invoked host tools (same model as today), not separate services. |
| **Test-first + reversible migrations** | ✅ PASS | Contract/unit tests authored RED-first; migration `060` ships with up **and** down. |

**Result: PASS.** No unjustified violations; the one pre-existing violation (async attach) is corrected by this plan. Two patterns are recorded in Complexity Tracking for transparency.

## Project Structure

### Documentation (this feature)

```text
docs/specs/009-single-node-real-mode/
├── spec.md              # Feature spec (done; 3 clarify rounds)
├── plan.md              # This file
├── research.md          # Phase 0 — technical decisions (R1..R12)
├── data-model.md        # Phase 1 — entities, schema delta (migration 060), on-disk layout
├── quickstart.md        # Phase 1 — real-mode e2e runbook (Ubuntu + libvirt + k3s)
├── contracts/           # Phase 1 — OpenStack behavioral contracts + contract-test matrix
│   ├── compute-disk-lifecycle.md   # WS-0
│   ├── network-reconciliation.md   # WS-2
│   ├── metadata-cloudinit.md       # WS-4
│   ├── north-south.md              # WS-3
│   └── block-storage.md            # WS-5
└── checklists/requirements.md      # spec quality checklist (done)
```

### Source Code (repository root — existing layout, files this feature touches)

```text
cmd/o3k/main.go                      # wire reconciler goroutine; bind metadata VM-reachable
internal/
├── nova/
│   ├── handlers.go                  # WS-0 disk-path helper + overlay staging; BDM-v2 parse; user_data; lifecycle
│   └── volume_attachment.go         # WS-5 make attach synchronous + fail-fast
├── neutron/
│   ├── reconciler.go   (NEW)        # WS-2 startup + periodic network-state reconciler
│   ├── network.go / ports.go        # reconcile entry points reused
│   ├── router.go                    # WS-3 external-gateway auto-map to node uplink
│   └── floatingip.go                # WS-3 reuse; ensure SG-gated DNAT
├── cinder/volumes.go                # WS-5 wire volume-from-image; status flow
├── glance/images.go                 # WS-0 image-in-use guard on delete
├── metadata/service.go              # WS-4 VM-reachable bind; namespace/project-aware identity; core surface + EC2 endpoints
├── database/models.go               # WS-5 volumes.source_image_id; user_data persistence
└── common/config.go                 # WS-2 reconcile_interval; WS-4 metadata bind address
pkg/
├── hypervisor/
│   ├── xml_template.go              # overlay disk path; serial console (present); volume disks (present)
│   └── libvirt.go                   # power lifecycle (start/stop/reboot), console log, attach/detach (present)
├── networking/
│   ├── dhcp.go                      # reconcile dnsmasq from DB; lease/host-reservation by MAC→IP
│   ├── router.go                    # SNAT/DNAT (present; reuse)
│   ├── metadata_redirect.go (NEW)   # WS-4 per-namespace 169.254.169.254 → metadata DNAT
│   └── netns.go / security_groups.go# rebuild on reconcile; SG on north-south
└── storage/
    ├── ceph.go / image_store.go     # WS-0 raw normalization on ingest; fix local path → /var/lib/o3k; CoW clone / copy for volume-from-image
config/o3k.yaml                      # reconcile_interval + metadata bind defaults
migrations/060_*.{up,down}.sql       # NEW — volumes.source_image_id (+ indexes)
build/package/Dockerfile             # add qemu-img + genisoimage
charts/o3k/README.md                 # WS-3 operator floating-IP pool recipe (MetalLB / routed subnet)
docs/{STUB_PLACEHOLDERS,WHATS_LEFT,REAL_MODE_TESTING}.md  # reconcile over-claims (polish)
test/
├── contract/                        # gophercloud contract tests (per workstream)
├── terraform/                       # Terraform compatibility tests (per workstream)
└── *.sh                             # real-mode e2e (cirros boot, restart, login, FIP, volume)
```

**Structure Decision**: Single Go binary — extend the existing `internal/`/`pkg/` packages in place. No new top-level structure; the only new files are one reconciler (`internal/neutron/reconciler.go`), one metadata-redirect helper (`pkg/networking/metadata_redirect.go`), and one migration. This honors Principle VI (single binary) and VII-style simplicity (reuse over new abstraction).

## Implementation Phasing (maps to user stories)

Built and validated in spec priority/dependency order; each story is independently testable (Constitution test-first applies per story).

1. **Foundation** — migration `060`; image packaging (`qemu-img`, `genisoimage`); fix node-local storage path (`/var/lib/o3k/volumes`).
2. **WS-0 (P1)** — raw-on-ingest, shared image-path helper, per-instance overlay, image-in-use guard, power lifecycle, serial console.
3. **WS-2 (P2)** — reconciler (`Start(ctx)` initial + ticker), DB-driven idempotent rebuild, best-effort per resource, hostPath persistence.
4. **WS-4 (P3)** — VM-reachable metadata bind + per-namespace DNAT, namespace/project-aware identity, core surface + EC2 endpoints, `user_data` write-path, config-drive end-to-end.
5. **WS-3 (P4)** — external-gateway→uplink auto-map, activate SNAT/floating-IP in real mode, SG enforcement on north-south, document MetalLB/routed-subnet pool.
6. **WS-5 (P5)** — synchronous volume attach/detach, volume-from-image (copy/CoW), boot-from-volume (BDM-v2 parse → `VMSpec.Volumes`).

## Complexity Tracking

> Recorded for transparency. Neither is a constitution violation; both reuse existing, accepted patterns.

| Item | Why needed | Why the simpler alternative is insufficient |
|------|------------|---------------------------------------------|
| Background **reconciler loop** (startup + periodic resync) | VMs are host processes that outlive the control-plane pod; their network state lives in kernel objects lost on pod restart, and a dnsmasq child can die mid-run. The DB is the source of truth and must be replayed. | A pure one-shot startup rebuild misses mid-run drift; doing it per-API-request can't help already-running VMs. The pattern mirrors the **existing VXLAN coordinator** (already accepted), so it introduces no new architectural concept and is not a forbidden async worker/queue. |
| **Best-effort per-resource** reconciliation (vs. global fail-fast) | One tenant's un-rebuildable network must not hold the whole control plane (and every other tenant) hostage at boot. | Global fail-fast on any tenant failure would make a single corrupt record a cluster-wide outage. Global **dependency** failures (DB/libvirt) remain fail-fast and run before reconciliation, so Principle III is preserved for the cases it targets. |

## Phase 0 / Phase 1 outputs

- **Phase 0 → [research.md](./research.md)**: 12 decisions (disk overlay & raw normalization, image-in-use, reconciler design, metadata DNAT & identity, north-south reuse, async-attach fix, boot-from-volume, storage-path fix, image packaging, console, capacity, test strategy).
- **Phase 1 → [data-model.md](./data-model.md)** (entities + migration `060` + on-disk layout), **[contracts/](./contracts/)** (per-workstream OpenStack behavioral contracts + contract-test matrix), **[quickstart.md](./quickstart.md)** (Ubuntu + libvirt + k3s e2e runbook).
- **Agent context**: the automatic `update-agent-context.sh` step is **deferred** — it mutates `CLAUDE.md`, which currently carries the user's *staged, uncommitted* speckit changes that must be left untouched. Run it manually (with `SPECIFY_FEATURE=009-single-node-real-mode`) once those are committed, then trim the Active-Technologies dump by hand.
