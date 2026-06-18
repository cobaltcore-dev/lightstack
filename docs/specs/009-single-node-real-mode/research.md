# Phase 0 Research: Single-Node Real Mode (v1)

All spec clarifications are resolved (3 rounds), so there are no open `NEEDS CLARIFICATION` items. This document records the technical decisions that turn the clarified spec into an implementation, each grounded in the current code (file:line) found during the audit.

Format per item: **Decision / Rationale / Alternatives considered / Touches**.

---

## R1 — Real-mode VM disk: per-instance qcow2 overlay over a raw base (WS-0)

**Decision**: At VM create, before `CreateVM`, stage `/var/lib/o3k/instances/<instance-id>/disk.qcow2` as a qcow2 overlay backing the Glance raw base (`qemu-img create -f qcow2 -b /var/lib/o3k/images/image-<image-id>.raw -F raw <overlay>`), chown/chmod for the host qemu uid, then point libvirt at the overlay. Introduce a single shared image-path helper used by both Glance's write sites and Nova's read site so they cannot diverge.

**Rationale**: Mirrors the upstream Nova default (qcow2 overlay over a base, `force_raw_images`). Today Nova hardcodes `ImagePath = /var/lib/o3k/images/<imageUUID>.qcow2` (`internal/nova/handlers.go:465`) — the **image** UUID, a file that never exists, so no real VM boots; and all VMs of an image would collide on one file. An overlay gives per-VM isolation (FR-002) at near-zero cost (CoW, no copy → keeps VM-create < 10 s). The XML already declares the disk as qcow2 (`pkg/hypervisor/xml_template.go:113`), so only the path/source changes.

**Alternatives considered**: Full raw copy per instance (rejected — slow, space-heavy, the spec confirmed overlay); qcow2-over-qcow2 (rejected — raw base is RBD-compatible and the upstream default).

**Touches**: `internal/nova/handlers.go` (path helper + overlay staging + delete `instances/<id>/` on VM delete), `pkg/storage/image_store.go` (shared path helper), `pkg/hypervisor/xml_template.go` (overlay source path).

## R2 — Normalize images to a raw base on ingest (WS-0)

**Decision**: On Glance upload, convert any supported format to raw (`qemu-img convert -O raw`), store as `image-<id>.raw`, and report the image's `disk_format` as `raw`.

**Rationale**: The store already names files `image-<id>.raw` (`pkg/storage/image_store.go:218`) but writes the uploaded bytes unconverted — so a qcow2 upload is mislabeled raw and the overlay's `-F raw` backing-format pin (R1) would be wrong. Converting on ingest makes the label truthful, makes the overlay correct, and is forward-compatible with RBD (which requires raw). Matches OpenStack's Glance import-conversion + Nova `force_raw_images` net effect (the user's "make it like OpenStack" decision).

**Alternatives considered**: Preserve uploaded format + detect backing format per image (rejected by clarification Q1 — more code; raw bases are the common production/RBD case); raw-only uploads (rejected — breaks cirros/cloud-image compatibility → FR-018).

**Touches**: `pkg/storage/image_store.go` (`uploadImageLocal` and friends), `internal/glance/images.go` (report `disk_format=raw`).

## R3 — Image-in-use guard: pin while any dependent disk exists (WS-0)

**Decision**: Block `DELETE /v2/images/<id>` when any instance overlay (`instances.image_id = <id>`) **or** any volume cloned from it (`volumes.source_image_id = <id>`, new column — see R11/data-model) exists. Independent full copies (local volume-from-image) do not set `source_image_id` once the copy completes, so they don't pin. Return the OpenStack-standard 409 "image in use".

**Rationale**: `internal/glance/images.go:437-479` has **no** in-use check today, so deleting an image orphans running VMs' backing files (corruption). Pinning by dependent disk matches upstream (RBD CoW children pin a Glance image until flattened). Tracking `source_image_id` is the minimal schema needed and is also required for boot-from-volume (R11).

**Alternatives considered**: Pin on any historical reference (rejected — over-blocks legitimate deletes vs upstream); ephemeral-only pinning (rejected — unsafe with RBD CoW). Both rejected by clarification Q5.

**Touches**: `internal/glance/images.go` (in-use query before delete), migration `060` (`volumes.source_image_id`).

## R4 — Network-state reconciler: startup + periodic, best-effort, idempotent (WS-2)

**Decision**: Add `internal/neutron/reconciler.go` modeled on the **existing VXLAN coordinator** (`internal/neutron/vxlan_coordinator.go`): a `NewNetworkReconciler(...)` + `Start(ctx)` that does an initial sync then a ticker-driven resync on a configurable interval, with a `Stop()`. Each pass reads the DB (networks → bridges, subnets+DHCP → dnsmasq, ports → taps/bridge attach, security groups → iptables chains, routers/floating-IPs → NAT) and rebuilds kernel state **idempotently** (check-then-create). Per-resource failures are isolated, logged, and retried next tick; they do not block readiness. Wire `go reconciler.Start(ctx)` into `cmd/o3k/main.go` after the VXLAN-coordinator block (≈ line 185), gated on `networkingMode != "stub"`; add `reconciler.Stop()` to the shutdown block (≈ lines 268-276).

**Rationale**: dnsmasq is an o3k child process whose PID lives in an in-memory map (`pkg/networking/dhcp.go`) lost on restart, and `internal/neutron/network.go` builds kernel state only on API calls — nothing replays it on boot. The VXLAN coordinator already proves the pattern (initial + periodic DB-driven idempotent sync) and is constitution-accepted, so this is reuse, not a new concept. Periodic resync self-heals a dead dnsmasq / flushed rules (clarification R2-Q2); best-effort isolation keeps one bad tenant from blocking the cloud (R2-Q3).

**Alternatives considered**: One-shot startup rebuild (rejected R2-Q2 — misses mid-run drift); event-driven only (rejected — can't heal out-of-band drift); global fail-fast on any tenant failure (rejected R2-Q3).

**Touches**: `internal/neutron/reconciler.go` (NEW), `internal/neutron/network.go`/`ports.go` (reuse build funcs), `pkg/networking/dhcp.go` (rebuild from DB), `cmd/o3k/main.go` (wire + shutdown), config (`neutron.reconcile_interval`).

## R5 — Stable IP across restart via DB-driven dnsmasq host reservations (WS-2)

**Decision**: Drive dnsmasq from the DB as MAC→IP host reservations (each port's `mac_address` + `fixed_ips`), so a rebuilt dnsmasq hands the VM the **same** IP it already has. The port's `fixed_ip` in the DB is authoritative; lease files are not relied upon.

**Rationale**: FR-006 requires the *same* IP after restart. Ports already persist `mac_address` and `fixed_ips` (JSONB) (`migrations/001`, `models.go`). Reservation-by-MAC is deterministic and matches OpenStack (Neutron writes dnsmasq host files from port allocations).

**Alternatives considered**: Preserve/replay lease files (rejected — fragile, not the source of truth); re-allocate IPs (rejected — would change the VM's IP, violating FR-006).

**Touches**: `pkg/networking/dhcp.go` (host-reservation config generation), reconciler (R4).

## R6 — Node-local persistence + fix the Cinder local path (WS-2 / FR-006a)

**Decision**: All real-mode VM data lives under the persistent hostPath `/var/lib/o3k`: `images/` (raw bases, already correct), `instances/<id>/disk.qcow2` (overlays, R1), `cloud-init/<id>.iso` (config drive, already there — `xml_template.go:171`), and `volumes/` (local volumes). **Fix Cinder's local path** from `~/.o3k/volumes` (`pkg/storage/ceph.go:36-38`) to `/var/lib/o3k/volumes` via a `resolveLocalPath` helper.

**Rationale**: The Helm chart mounts `/var/lib/o3k` as the node's persistent hostPath; the home-dir path is ephemeral inside the pod, so local volumes vanish on a pod restart — breaking FR-006a (disk survives restart) and FR-022 (volume data persists). Glance already uses `/var/lib/o3k/images` correctly; Cinder is the outlier. (Note: the SPEC-009-teardown branch fixed the same bug independently; this branch needs its own fix.)

**Alternatives considered**: Leave Cinder on `~/.o3k` and special-case the chart (rejected — fragile, contradicts the persistence guarantee).

**Touches**: `pkg/storage/ceph.go` (`resolveLocalPath`/`LocalVolumeDir`), `build/package/Dockerfile` (pre-create dirs — already creates `volumes`/`images`).

## R7 — Metadata reachability: per-namespace 169.254.169.254 → metadata DNAT (WS-4)

**Decision**: Bind the metadata HTTP server (root namespace) on a host address reachable from tenant namespaces — concretely the per-network bridge gateway IP (or a dedicated metadata address on that bridge), port `8775` — not `localhost`. Inside each tenant network namespace, program a DNAT redirect `169.254.169.254:80 → <bridge metadata addr>:8775` (iptables in the netns, plus the route). **Program this synchronously when the subnet/namespace is created** (the same point DHCP is started today) so reachability is immediate, and **replay it in the reconciler (R4)** so it survives restart. Change `metadata.NewService("localhost:8775")` (`cmd/o3k/main.go:217`) accordingly.

**Rationale**: Today metadata binds `localhost:8775` with zero link-local routing (`internal/metadata/service.go`, only comments reference `169.254.169.254`), so guests can't fetch cloud-init. Per-namespace DNAT is exactly how upstream Neutron's metadata path works and composes with the existing netns model. Adding it to the reconciler makes it durable (R4).

**Alternatives considered**: DHCP option-121 route to a host-bound metadata (rejected as primary — DNAT in-namespace is more robust and standard; option-121 can be a secondary hint); a metadata agent per namespace as a separate process (rejected — violates single-binary Principle VI).

**Touches**: `pkg/networking/metadata_redirect.go` (NEW), `internal/metadata/service.go` (bind), `cmd/o3k/main.go` (bind addr), reconciler (R4).

## R8 — Metadata identity: namespace/project-aware (overlapping subnets) (WS-4)

**Decision**: Resolve the calling instance using the request's **network/namespace context** (injected by the per-namespace redirect, e.g. a network-id marker) combined with source IP, not source IP alone. This distinguishes two tenants that share a subnet.

**Rationale**: `instanceFromRequest` (`internal/metadata/service.go:62-84`) looks up `ports.fixed_ip = clientIP LIMIT 1` — ambiguous when subnets overlap, which OpenStack multi-tenancy allows. Per-project namespaces (`docs/NETWORKING_MODES.md`, `ARCHITECTURE.md`) already separate the traffic, so the redirect can tag the network/project context — the upstream Neutron metadata-proxy approach. Required for FR-010 isolation (clarification Q3).

**Alternatives considered**: Assume non-overlapping subnets + source-IP-only (rejected Q3 — weak isolation); per-network metadata listeners (heavier; DNAT + context marker is sufficient on one node).

**Touches**: `pkg/networking/metadata_redirect.go` (context marker), `internal/metadata/service.go` (identity lookup using network context).

## R9 — `user_data` write-path + config-drive end-to-end (WS-4)

**Decision**: Add `user_data` to `CreateServerRequest`, persist it to the existing `instance_userdata` table on create, and serve it through both the metadata service (core surface) and the config-drive ISO. Ensure the config-drive path works by adding `genisoimage` to the image (R12).

**Rationale**: The metadata read-side already serves `user_data` from `instance_userdata` (audit), but the write-side is missing — `CreateServerRequest` (`internal/nova/handlers.go:235-244`) has no `user_data` field and the table is never populated. The config-drive ISO is generated and attached as a CDROM (`xml_template.go:167`, `GenerateCloudInitISO`→`genisoimage` at `:221`) but `genisoimage` is absent from the image, so it silently fails. Both are needed for Terraform `user_data` compatibility (FR-012) and the "both channels" decision (Q-R1).

**Alternatives considered**: Metadata-only or config-drive-only (rejected by clarification — "both"); new user_data table (rejected — `instance_userdata` already exists).

**Touches**: `internal/nova/handlers.go` (parse + persist `user_data`), `internal/metadata/service.go` (serve core surface on OpenStack + EC2 endpoints), `build/package/Dockerfile` (`genisoimage`).

## R10 — North-south: reuse the existing router NAT, auto-wire to the uplink, enforce SGs (WS-3)

**Decision**: Reuse the already-implemented `EnableSNAT()` and floating-IP `AddFloatingIP()` (DNAT+return) in `pkg/networking/router.go` and the floating-IP API (`internal/neutron/floatingip.go`). Make them fire in single-node real mode by (a) auto-mapping a router's external gateway to the node's primary uplink (auto-detect default-route iface) instead of requiring an operator bridge/NIC, and (b) modeling the floating-IP pool as a standard external/provider network. Ensure north-south traffic traverses the security-group chains (default-deny inbound).

**Rationale**: This code already exists and is correct; it only no-ops in stub mode (audit of `router.go`/`floatingip.go`). So WS-3 is activation + zero-config wiring, not new construction (clarification Q2). Auto-mapping to the uplink delivers the issue's "no operator bridge/NIC" while keeping the standard OpenStack router model (FR-014). SGs on north-south are an upstream security property (FR-016a, clarification R2-Q4); netfilter ordering (DNAT in PREROUTING, then the SG filter chain sees the fixed IP) composes with the existing SG plumbing.

**Alternatives considered**: Zero-config router-less egress (rejected Q2 — non-OpenStack); rebuild north-south from scratch (rejected — duplicates working code); k8s LoadBalancer-per-FIP (rejected in SPEC-008 — too heavy).

**Touches**: `internal/neutron/router.go` (external-gateway→uplink auto-map), `pkg/networking/router.go` (reuse; uplink detection), `internal/neutron/floatingip.go` (reuse), `pkg/networking/security_groups.go` (ensure north-south coverage), reconciler (R4, replay NAT/SG).

## R11 — Block storage: synchronous attach, volume-from-image, boot-from-volume (WS-5)

**Decision**:
1. **Make attach synchronous + fail-fast**: fold the libvirt `AttachDevice` call into the request path (remove the background goroutine at `internal/nova/volume_attachment.go:126-167`) and revert the DB attachment on libvirt failure.
2. **Volume-from-image**: honor the `ImageRef`/`imageRef` already accepted by `CreateVolumeRequest` (`internal/cinder/volumes.go:236`, currently ignored) — for local, copy the raw base into the volume; for RBD, CoW-clone the image; set `volumes.source_image_id` and `bootable=true`.
3. **Boot-from-volume**: parse `block_device_mapping_v2` in `CreateServerRequest`, create/locate the bootable volume, and populate `VMSpec.Volumes` so the existing XML volume-disk loop (`xml_template.go` + `GenerateDiskXML`) boots from it; honor `delete_on_termination`.

**Rationale**: Attach today updates the DB synchronously but does the libvirt call in a goroutine that never reverts on failure — a Constitution II violation and a correctness bug (DB claims success when the hypervisor failed). The XML side for volumes already exists (`VMSpec.Volumes` loop, `GenerateDiskXML`); only the create flow doesn't populate it and BDM isn't parsed. `volumes` has no `source_image_id` (audit) so a migration is required (R3 reuses it for image pinning).

**Alternatives considered**: Keep attach async (rejected — violates Principle II); skip boot-from-volume (rejected — clarification Q4=C chose full block storage).

**Touches**: `internal/nova/volume_attachment.go` (sync + revert), `internal/cinder/volumes.go` (volume-from-image), `internal/nova/handlers.go` (BDM-v2 → `VMSpec.Volumes`), `pkg/storage/ceph.go` (CoW clone / copy), migration `060` (`volumes.source_image_id`).

## R12 — Cross-cutting decisions

**Image packaging** — Add `qemu-img` (qemu-utils) and `genisoimage` to `build/package/Dockerfile` (`dnsmasq` already present). *Rationale*: qemu-img is required for R1/R2/R11 and is never installed; genisoimage is required for the config-drive path (R9) and is absent — its failure is currently swallowed. *Alternatives*: bundle a Go ISO/qcow2 library (rejected — heavier than invoking the standard tools, which is the existing model).

**Console** — Serial console + `openstack console log show` only; noVNC deferred. *Rationale*: the serial device is already in the XML and the boot log is needed to validate WS-0/WS-4; `instances` even has `console_vnc_port`/`password` columns (migration 005) for a later noVNC story. *Alternatives*: noVNC now (rejected — needs a VNC/websocket proxy; clarification R3-Q2).

**Capacity** — Best-effort, host/libvirt-enforced; a failed allocation → instance `ERROR` with a clear message; no scheduler/overcommit. *Rationale*: single node; placement accounting is a multi-node feature (clarification R3-Q5). *Alternatives*: placement-style ratios (rejected for v1).

**IP family** — IPv4 only. *Rationale*: validation target + all ACs are IPv4; IPv6 changes DHCP/SG/ingress model (clarification R3-Q4).

**Test strategy** — Contract-first per Constitution: gophercloud + `openstack` CLI contract tests under `test/contract/` and a Terraform apply, authored RED before implementation; stub-mode unit tests for mode-detection/logic; real-mode e2e bash on the Linux host (cirros). *Rationale*: Principle I requires real-client contract tests; real-mode ACs can't run in a non-Linux sandbox, so e2e is host-gated (mirrors SPEC-008/009).

**Migration `060`** — `volumes.source_image_id UUID NULL` (+ index); reversible up/down. Next free number after `059_qos_specs_multitenancy`.
