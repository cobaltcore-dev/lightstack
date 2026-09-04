# Feature Specification: Single-Node Real Mode (v1)

**Feature Branch**: `bugfix/503-single-node-real-mode-v1`
**Created**: 2026-06-18
**Status**: Draft
**Input**: User description: "issue 503 — single-node real mode (v1)" (cobaltcore-dev/cloud-storage#503, subtask of #476 / lightstack#52). Make a real-mode Lightstack deployment on a single node actually usable end-to-end: a tenant boots a real virtual machine from an image, the VM keeps its network across a control-plane restart, the VM receives its cloud-init data and is loginnable, and the VM can reach the internet and be reached from outside.

> **Scope & sequencing**: This feature covers the **single-node** real-mode workstreams of the post-conditions tracker (lightstack#52 Part A, mirrored as cloud-storage#476): real-mode VM disk (WS-0), startup network reconciliation (WS-2), metadata/cloud-init reachability (WS-4), and single-node north-south networking (WS-3). It is delivered in dependency order **WS-0 → WS-2 → WS-4 → WS-3** (boot the disk → keep its IP → cloud-init/login → external reach). Per clarification (2026-06-18) it additionally covers **real-mode block storage** (Cinder volume create/attach/detach and boot-from-volume) — additive scope beyond the issue's four workstreams. **Multi-node** VM placement, cross-node request forwarding, and routing a floating IP to a *hosting* node (WS-1) are explicitly **out of scope** — they are the v2 ticket (lightstack#52 Part B). This feature changes product runtime behavior; it does not change the SPEC-008 Helm chart manifests, which already grant the single-node real-mode topology (privileged, host networking, host libvirt socket, node-local storage).

## Clarifications

### Session 2026-06-18

- Q: North-south networking — build it new, or reuse what exists? → A: **Reuse the existing implementation.** Real-mode egress masquerade and floating-IP DNAT/return-path are already implemented in the product and are no-ops only in stub mode. This feature makes that path actually function in a zero-configuration single-node real deployment and fills gaps only where an acceptance criterion fails — no rewrite.
- Q: How should a VM receive cloud-init data — metadata service, config drive, or both? → A: **Both.** The standard link-local metadata service must be reachable from the VM, **and** the config-drive ISO path must work end-to-end. A guest may consume either.
- Q: Is `user_data` (cloud-init scripts/config beyond the SSH key) in scope? → A: **Yes — include it.** The create API must accept `user_data`, persist it, and deliver it through both the metadata service and the config drive. Required for Terraform `openstack_compute_instance_v2.user_data` and OpenStack CLI compatibility.
- Q: Two stale assumptions from the originating issue were re-verified against current code — what is the real starting state? → A: (1) The reference image **already includes `dnsmasq`** (added by the Helm work), so first-boot DHCP is not blocked by a missing package; the remaining DHCP gap is purely survival across restart (WS-2). (2) The image is **missing the tooling for config-drive ISO creation and per-instance disk provisioning**, so the image build must add it.
- Q: What is the validation target for the real-mode acceptance criteria (which cannot be exercised in a non-Linux sandbox)? → A: A **single Linux node** (Ubuntu with KVM + libvirt) running a **single-node k3s** cluster with Helm. A standard small cloud image (e.g. cirros) is used as the bootable test image.

### Session 2026-06-18 (/clarify)

- Q: When a tenant uploads an image (e.g. qcow2), how should it be stored so VMs boot reliably? → A: **Normalize to raw on ingest.** Any supported upload format is converted to a raw base when stored (matching OpenStack's net effect of Glance import-conversion + Nova `force_raw_images=True`); the per-instance disk is a qcow2 copy-on-write overlay backing that raw base; the image's `disk_format` is reported as `raw` after conversion.
- Q: How does a VM get internet egress, and what anchors floating IPs (existing code uses a router + external gateway; the issue says "no operator bridge/NIC")? → A: **Standard OpenStack topology, auto-wired to the uplink.** Tenants use the normal router + external-network model; Lightstack auto-maps the external network's gateway to the node's primary uplink so the operator builds no bridge and plugs no NIC. Egress = router SNAT out the uplink; floating IPs = router DNAT, drawn from an operator-supplied external/provider network. A network with no router has no egress (as upstream). Maximizes reuse of the existing SNAT/floating-IP code.
- Q: Must v1 isolate metadata when tenant subnets overlap (same private subnet in two tenants), or may it assume non-overlapping ranges? → A: **Support overlapping subnets (network/namespace-aware identification).** Metadata resolves the calling server from its network-namespace/project context (as OpenStack's per-namespace metadata proxy does), not source IP alone, so two tenants sharing a subnet are correctly distinguished — this fits lightstack's existing per-project network-namespace model (`docs/NETWORKING_MODES.md`, `docs/ARCHITECTURE.md`). Boundary: cross-tenant overlap is covered; the same subnet appearing twice *within one project* (one namespace) is a bounded edge needing per-network handling.
- Q: Is real-mode block storage (Cinder) in scope for #503, or only ephemeral image-boot? → A: **In scope — full block storage.** Real-mode volume create, attach/detach to a running VM, **and** boot-from-volume are all included (additive scope beyond the issue's four workstreams; the GH issue/checklist should be updated to match). Single-node uses node-local backing by default (RBD where configured). The Q1 raw-base decision aids RBD boot-from-volume (copy-on-write clone needs raw).
- Q: What counts as "image in use" for the deletion guard (FR-004), given ephemeral overlays and boot-from-volume? → A: **Pin while any dependent disk exists (upstream semantics).** Deletion is blocked while any instance overlay or copy-on-write (RBD) volume backs off the image; independent full copies (e.g. local boot-from-volume that copies the bytes) do not pin it once the copy completes. Requires tracking image→dependent-disk relationships; safe with RBD CoW (no orphaned children).

### Session 2026-06-18 (/clarify round 2)

- Q: Must a VM's disk and cloud-init data survive a control-plane (pod) restart, and where do they live? → A: **All real-mode VM data on the persistent node hostPath (`/var/lib/lightstack`).** Per-instance overlays, config-drive ISOs, local volumes, and image bases persist on node-local storage and survive pod restarts, so disk-survival matches the WS-2 network-survival guarantee (a VM whose disk vanished on a pod roll would be broken).
- Q: Does the real-mode network reconciler run once at startup, or continuously? → A: **Startup rebuild + periodic resync** (configurable interval), modeled on the existing VXLAN coordinator. It self-heals runtime drift (a dnsmasq child that died, flushed rules), not just pod restarts; the resync is non-disruptive and idempotent.
- Q: If startup reconciliation can't rebuild one tenant's network, does the whole control plane refuse to start? → A: **No — best-effort per resource.** A per-tenant rebuild failure is isolated, logged/surfaced, and retried on the next resync; the API still becomes ready and other tenants are unaffected. Global dependency failures (DB/libvirt unreachable) remain fail-fast via the existing startup checks that run before reconciliation.
- Q: Are floating-IP ingress/egress subject to security groups in v1, or is north-south open? → A: **Enforce security groups on north-south.** Floating-IP traffic is filtered by the VM's security groups (default-deny inbound; tenant opens ports via SG rules), as upstream — a floating IP does not expose all ports. Composes with the existing SG plumbing via standard netfilter ordering (DNAT then filter).
- Q: Does "survives a restart" include a full host reboot, or only a Lightstack pod/control-plane restart? → A: **Pod/control-plane restart only for v1.** A VM survives a Lightstack pod restart/upgrade (it keeps running; networking is rebuilt). After a full host reboot, networking is still rebuilt but **VM auto-resume (compute reconciliation) is out of scope for v1** — matching the issue ACs (pod restart/upgrade). It is a modest follow-up given disks persist (R2-Q1) and networking rebuilds (R2-Q2).

### Session 2026-06-18 (/clarify round 3)

- Q: Which VM lifecycle operations must work in real mode v1? → A: **Core power lifecycle** — create, delete, start (power-on), stop (power-off), soft/hard reboot. Deferred and documented out of scope: snapshot (create-image-from-instance), resize, rescue, suspend/pause; migrate is multi-node (already out).
- Q: Is VM console access in scope for v1, and at what level? → A: **Serial console + console log** (`openstack console log show` / serial console). Deferred and documented out of scope: graphical noVNC console (needs a VNC/websocket proxy).
- Q: What metadata content surface must v1 serve? → A: **Core cloud-init surface** — instance-id, hostname, public-keys, user_data, meta_data.json — over both OpenStack (`/openstack/...`) and EC2-compatible (`/latest/...`) endpoints. Deferred and documented out of scope: network_data.json (static guest network config — DHCP covers it) and vendor_data.
- Q: IPv4-only for v1, or dual-stack? → A: **IPv4-only.** Tenant networks, DHCP, metadata, floating IPs, and security groups are IPv4 in v1. Deferred and documented out of scope: IPv6 / dual-stack (DHCPv6/RA, v6 SG rules, and v6's NAT-free no-floating-IP ingress model).
- Q: Does v1 do host capacity accounting, or rely on the host? → A: **Best-effort, host/libvirt-enforced.** No lightstack-side capacity claims or overcommit ratios in v1; a genuine allocation failure puts the instance in ERROR with a clear message (fail-fast). No fixed concurrency target — bounded by host resources. Placement-style accounting deferred (multi-node concern).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Boot a real virtual machine from an image (Priority: P1)

A tenant, using the standard OpenStack CLI or the Terraform provider against a single-node real-mode lightstack, creates a server from a previously uploaded image. The server actually boots: a real guest comes up from that image's contents, reaches a running state, and is visible as active. Creating several servers from the same image yields independent VMs whose disks do not interfere with one another or with the source image.

**Why this priority**: This is the gating capability. Until a real VM boots from an image, none of the other capabilities (networking durability, login, external reach) have anything to act on. It is the minimum viable slice of "real mode" and everything else builds on it.

**Independent Test**: On a single Linux node with libvirt, configure real mode, upload a small cloud image, and create a server via the OpenStack CLI; confirm a running guest exists and reports active. Create a second server from the same image and confirm both run independently.

**Acceptance Scenarios**:

1. **Given** a single-node real-mode installation and an uploaded bootable image, **When** a tenant creates a server referencing that image, **Then** a real guest boots from the image's contents and the server reports an active/running state — with no disk-path or "image not found" error.
2. **Given** one uploaded image, **When** two servers are created from it, **Then** each server has its own writable disk and changes made inside one server are not visible in the other or in the source image.
3. **Given** a server that is using an image, **When** a tenant attempts to delete that image, **Then** the deletion is rejected with a clear, OpenStack-standard "image in use" error and the image remains usable.
4. **Given** a running server, **When** the server is deleted, **Then** its per-instance disk artifacts are removed and the source image is left intact.
5. **Given** a running real-mode server, **When** a tenant stops, starts, or reboots it via the OpenStack CLI/Terraform, **Then** the underlying libvirt domain reflects the change and the server's reported power state matches.
6. **Given** a booted real-mode server, **When** a tenant requests its console log, **Then** the guest's boot/serial output is returned.

---

### User Story 2 - VM networking survives a control-plane restart (Priority: P2)

A tenant's running server keeps its network identity — its DHCP-assigned IP address and connectivity — across a restart or upgrade of the Lightstack control plane (for example, a Kubernetes pod roll). The operator does not have to touch anything; when the control plane comes back, it rebuilds the network state for already-running VMs from the system's own records. The VM's disk and cloud-init data live on node-local persistent storage, so they survive the restart alongside the networking.

**Why this priority**: Real VMs are host processes that outlive the control-plane pod, so a control-plane restart must not silently strand them without networking. This is sequenced immediately after boot because every later network-dependent capability (metadata reachability, north-south) must be validated on network state that survives the control-plane lifecycle, and it is the smallest self-contained change after WS-0.

**Independent Test**: Boot a server in real mode and confirm it obtained an IP. Restart the Lightstack control plane. Without any manual action, confirm the server still has the same IP and its connectivity is intact, and that repeating the restart does not create duplicate or conflicting network resources.

**Acceptance Scenarios**:

1. **Given** a running server that has a DHCP-assigned IP, **When** the Lightstack control plane is restarted or upgraded, **Then** the server retains the same IP and network connectivity without manual intervention.
2. **Given** a control plane that has just restarted, **When** it rebuilds network state from persisted records, **Then** the rebuild is idempotent — running it again produces no duplicate networks, addresses, or rules and causes no disruption to running VMs.
3. **Given** the control plane is restarting, **When** it comes back up, **Then** the network reconciliation is driven entirely by persisted state (no reliance on in-memory state lost at restart).
4. **Given** a running server with data written to its disk, **When** the Lightstack control plane is restarted or upgraded, **Then** the server's disk and any local volumes are intact and the VM keeps running — its disk artifacts persist on node-local storage.
5. **Given** a running server whose DHCP service is killed out-of-band (not a full restart), **When** the next resync interval elapses, **Then** the service is respawned and the server's networking is restored without manual intervention.

---

### User Story 3 - Reach and log into the VM via cloud-init (Priority: P3)

A tenant creates a server with an SSH keypair and optional `user_data`. The booting guest fetches its cloud-init data — through the standard link-local metadata endpoint and/or an attached config drive — receives the injected SSH key and the supplied `user_data`, and the tenant can log in over SSH using the matching private key. The metadata a server receives is its own; one server cannot read another server's or another tenant's data.

**Why this priority**: A VM that boots but cannot be configured or logged into is not usable. This delivers the first genuinely usable VM. It is sequenced after networking durability so cloud-init delivery is validated over a stable, restart-surviving network.

**Independent Test**: Create a server with a named keypair and a small `user_data` script. After boot, confirm the guest received the metadata (key present, `user_data` applied) and that SSH login with the private key succeeds. Confirm a second server cannot retrieve the first server's metadata.

**Acceptance Scenarios**:

1. **Given** a server created with a keypair, **When** the guest boots and runs cloud-init, **Then** it obtains its metadata from the standard link-local endpoint and/or the config drive and the tenant can SSH in using the injected key.
2. **Given** a server created with `user_data`, **When** the guest boots, **Then** the `user_data` is delivered to cloud-init and applied (verifiable by its documented effect).
3. **Given** two servers in different tenants whose networks use the same (overlapping) subnet, **When** each requests its metadata, **Then** each receives only its own data and neither can read the other's.
4. **Given** a Terraform configuration that sets `key_pair` and `user_data` on a compute instance, **When** it is applied against lightstack, **Then** it succeeds with no provider modification and the resulting VM reflects both.

---

### User Story 4 - VM internet egress and floating-IP ingress (Priority: P4)

A tenant's server can reach external networks (the internet) for outbound traffic once its network is attached to a router with an external gateway (the standard OpenStack model), using the node's existing uplink — the operator does not have to pre-create an external bridge or configure host NICs. For inbound access, the tenant allocates a floating IP from an operator-supplied pool and associates it with the server as a standard OpenStack API object; external clients can then reach the server at that address, on the single node.

**Why this priority**: External connectivity completes the "usable VM" picture, but it is only meaningful once the VM boots, keeps its network, and is configured/loginnable. On a single node, egress and ingress are self-contained (no cross-node routing), making this the natural final slice.

**Independent Test**: From a booted server, confirm outbound reachability to an external host. Allocate a floating IP from the configured pool, associate it with the server, and confirm an external client can reach the server at that address; disassociate and confirm access stops.

**Acceptance Scenarios**:

1. **Given** a booted server on a tenant network whose router has an external gateway, **When** it initiates outbound traffic to an external host, **Then** the traffic reaches the destination masqueraded out the node's primary uplink, with no operator-provided external bridge or NIC setup.
2. **Given** a server and an operator-configured floating-IP pool, **When** a tenant allocates a floating IP and associates it with the server, **Then** external traffic addressed to the floating IP reaches the server and the server's outbound traffic appears to originate from it.
3. **Given** a server with an associated floating IP, **When** the tenant disassociates or releases the floating IP, **Then** external access via that address stops and the address returns to the pool.
4. **Given** floating IPs managed as standard API objects, **When** a tenant manages them via the OpenStack CLI or Terraform, **Then** allocation, association, disassociation, and release behave exactly as with upstream OpenStack, with no client modification.
5. **Given** a server with a floating IP but no inbound security-group rule for a port, **When** an external client connects to that port, **Then** the connection is blocked; **When** the tenant adds an allow rule for that port, **Then** the connection succeeds.

---

### User Story 5 - Block storage in real mode: attach volumes and boot from volume (Priority: P5)

A tenant creates a block-storage volume that is actually provisioned on real backing storage, attaches it to a running real VM and sees a usable block device inside the guest, detaches it, and can also boot a server directly from a volume (a bootable volume created from an image). All of this works through the standard OpenStack CLI and Terraform, exactly as upstream.

**Why this priority**: This extends real mode beyond ephemeral image-boot to persistent block storage — the remaining piece of a fully usable single-node real cloud. It is sequenced last because it builds on real-mode compute (US1) and is additive scope agreed during clarification, not part of the issue's original acceptance criteria.

**Independent Test**: On the single real-mode node, create a volume, attach it to a running VM and confirm the guest sees a new block device, write then read data, and detach it; separately, create a bootable volume from an image and boot a server from it, confirming the server runs from the volume.

**Acceptance Scenarios**:

1. **Given** a real-mode installation, **When** a tenant creates a volume, **Then** real backing storage is provisioned and the volume becomes available.
2. **Given** a running real VM and an available volume, **When** the tenant attaches the volume, **Then** the guest sees a new block device whose data persists across detach and re-attach; **When** the tenant detaches it, **Then** the device is removed from the guest and the volume returns to available.
3. **Given** an image, **When** a tenant boots a server from a new volume created from that image, **Then** the server boots from the volume and the volume's persistence follows the standard delete-on-termination flag.
4. **Given** volumes managed via the OpenStack CLI or Terraform, **When** a tenant performs create/attach/detach/delete and boot-from-volume, **Then** behavior matches upstream OpenStack with no client modification.

---

### Edge Cases

- A tenant references an image whose backing data is missing or unreadable → server creation fails fast with a clear error and the server is marked failed, rather than booting a broken or empty disk.
- The node lacks capacity (RAM/CPU/disk) for a requested VM → the create fails fast and the instance is marked ERROR with a clear message; v1 does no overcommit accounting (host-enforced).
- The control plane restarts while a server is mid-creation → the system converges to a consistent state on recovery; no half-built or orphaned network resources remain.
- The control plane is restarted twice in succession → reconciliation remains idempotent; no duplicate leases, bridges, namespaces, or rules accumulate.
- One tenant's network state is corrupt or un-rebuildable at startup → the failure is isolated and retried on the next resync; the API still becomes ready and other tenants' networking is unaffected.
- The node itself is fully rebooted (not just a Lightstack pod restart) → networking is rebuilt on startup, but stopped VMs are not auto-resumed in v1 (host-reboot VM resume is out of scope — see Out of Scope); disks persist, so a follow-up or the operator can restart them.
- A server is created with no keypair and no `user_data` → it still boots normally; it is simply not key-loginnable.
- `user_data` exceeds the OpenStack-standard size limit → the request is rejected with a clear error consistent with upstream behavior.
- The floating-IP pool is exhausted → allocation fails with a clear, OpenStack-standard error.
- A metadata request arrives from an address that maps to no known server → it is denied (no data returned).
- Two tenant networks use the same (overlapping) subnet → metadata is still attributed to the correct server via namespace/project context. (The same subnet used twice *within a single project* shares one namespace and is a known bounded limitation — see Clarifications.)
- An image is deleted that is *not* in use → deletion succeeds normally (the in-use guard must not block legitimate deletes).
- A bootable volume is created from an image → on a copy-on-write (RBD) backend the image stays pinned until the dependent disk is gone; on a local backend that copies the bytes, the image is not pinned once the copy completes.

## Requirements *(mandatory)*

### Functional Requirements

**Real-mode VM disk (WS-0)**

- **FR-001**: A server created in real mode MUST boot from the contents of its referenced image; the running guest's root disk MUST be derived from that image.
- **FR-001a**: In real mode, the core VM power lifecycle MUST function against libvirt — create, delete, start (power-on), stop (power-off), and soft/hard reboot — behaving as upstream OpenStack via the API/CLI/Terraform provider. (Snapshot, resize, rescue, suspend/pause, and migrate are out of scope for v1 — see Out of Scope.)
- **FR-001b**: In real mode, the serial console log and serial console access MUST be available (`openstack console log show` and serial console), behaving as upstream — supporting boot/cloud-init diagnosis. (The graphical noVNC console is out of scope for v1 — see Out of Scope.)
- **FR-002**: Each server MUST have its own writable disk that is isolated from other servers and from the source image; writes by one server MUST NOT be visible to another server or alter the source image.
- **FR-003**: Uploaded images MUST be normalized to a raw base when stored (any supported upload format, e.g. qcow2, is converted to raw and the image's `disk_format` is reported as `raw`), and the per-instance disk MUST be a copy-on-write overlay backing that raw base. The image's stored location and the server's boot location MUST refer to the same, correctly-formatted artifact, so boot never fails on a path or format mismatch.
- **FR-004**: An image MUST NOT be deletable while any disk depends on it — i.e. while any instance's ephemeral overlay or any copy-on-write (RBD) volume backs off it; the attempt MUST fail with a clear "image in use" error consistent with OpenStack semantics. Independent full copies (e.g. a local boot-from-volume that copies the bytes) MUST NOT pin the image once the copy completes.
- **FR-005**: Deleting a server MUST remove that server's per-instance disk artifacts while leaving the source image intact.

**Startup network-state reconciliation (WS-2)**

- **FR-006**: After a control-plane restart or upgrade, a running server MUST retain its DHCP-assigned IP address and network connectivity without manual intervention.
- **FR-006a**: A running server's disk artifacts — its per-instance overlay, config-drive image, and any local volumes — MUST persist on node-local storage and survive a control-plane restart or upgrade, so the VM (a host process) stays intact and bootable alongside its retained networking.
- **FR-007**: On startup, the control plane MUST rebuild real-mode network state (address service, namespaces, bridges, and security-group rules) from persisted records.
- **FR-007a**: Beyond the startup rebuild, the control plane MUST periodically re-sync real-mode network state from persisted records on a configurable interval (modeled on the existing VXLAN coordinator), self-healing runtime drift such as a dnsmasq child that died or rules that were flushed. The resync MUST be non-disruptive and idempotent (per FR-008).
- **FR-008**: Network-state reconciliation MUST be idempotent and non-disruptive — repeated runs MUST NOT create duplicate or conflicting resources, and MUST NOT interrupt the networking of already-running VMs.
- **FR-008a**: Reconciliation MUST be best-effort per resource: a failure to rebuild one tenant/network MUST be isolated, logged/surfaced, and retried on the next resync (FR-007a) — it MUST NOT block control-plane readiness or affect other tenants. (Global dependency failures — DB or libvirt unreachable — remain fail-fast via the existing startup dependency checks, which run before reconciliation.)

**Metadata, cloud-init, and login (WS-4)**

- **FR-009**: A booting guest MUST be able to retrieve its cloud-init metadata from the standard link-local metadata endpoint (the OpenStack `169.254.169.254` contract) from within its tenant network.
- **FR-009a**: The metadata service MUST serve the core cloud-init surface — instance-id, hostname, public-keys, `user_data`, and `meta_data.json` — over both the OpenStack (`/openstack/...`) and EC2-compatible (`/latest/...`) endpoints, so common guests find a datasource. (`network_data.json` and `vendor_data` are out of scope for v1 — see Out of Scope.)
- **FR-010**: The metadata returned MUST correspond to the requesting server, identified using the request's network-namespace/project context (not source IP alone) so that overlapping tenant subnets are correctly distinguished. A server MUST NOT be able to retrieve another server's or another tenant's metadata.
- **FR-011**: An SSH key associated with a server at creation (via keypair) MUST be delivered to the guest such that the tenant can log in over SSH with the matching private key.
- **FR-012**: The server-create operation MUST accept `user_data`, persist it for the server, and deliver it to the guest's cloud-init via **both** the metadata service and the config drive.
- **FR-013**: Cloud-init delivery via config drive MUST work end-to-end (the config-drive image is produced and attached to the guest), so a guest that does not use the network metadata endpoint is still provisioned.

**Single-node north-south networking (WS-3)**

- **FR-014**: A server whose tenant network is connected to a router with an external gateway MUST have outbound (egress) reachability to external networks, masqueraded out the node's existing primary uplink. Lightstack MUST auto-map the external network to that uplink so the operator pre-creates no external bridge and configures no host NICs. Egress follows the standard OpenStack router model — a network with no router has no external reachability, as upstream.
- **FR-015**: Floating IPs MUST be available as standard OpenStack API objects; allocating, associating, disassociating, and releasing them MUST behave as in upstream OpenStack.
- **FR-016**: When a floating IP is associated with a server, external traffic addressed to it MUST reach the server, and the server's corresponding outbound traffic MUST appear to originate from the floating IP (on the single node).
- **FR-016a**: Floating-IP ingress and egress traffic MUST be subject to the server's security groups (default-deny inbound; the tenant opens specific ports via security-group rules), consistent with upstream OpenStack — a floating IP MUST NOT expose ports the security groups do not permit.
- **FR-017**: The floating-IP pool MUST be modeled as a standard OpenStack external/provider network whose address range is operator-configurable; the operator supplies that range and the routing mechanism that delivers it to the node (e.g. MetalLB or a routed/L2 subnet), and that mechanism MUST be documented.

**Cross-cutting compatibility**

- **FR-018**: All capabilities in this feature MUST remain 100% compatible with the OpenStack API, the OpenStack CLI, and the Terraform OpenStack provider — usable with no client-side modification.
- **FR-019**: All operations MUST be synchronous — the API call completes the work (or fails) before returning; no queues or asynchronous state machines are introduced.
- **FR-020**: Stub mode behavior MUST be preserved and remain the default; enabling real mode is an explicit operator choice and MUST NOT be required for non-real deployments.

**Block storage (Cinder) real mode** *(additive scope — see Clarifications)*

- **FR-021**: Creating a volume in real mode MUST provision real backing storage (node-local by default on a single node; RBD where configured), and the volume MUST reach an available state usable by VMs without manual host steps.
- **FR-022**: A volume MUST be attachable to, and detachable from, a running real VM; once attached the guest MUST see a usable block device, and data written to it MUST persist across detach and re-attach.
- **FR-023**: A server MUST be bootable from a volume (a bootable volume created from an image), with the root volume's persistence governed by the standard delete-on-termination semantics.
- **FR-024**: Volume create, attach, detach, delete, and boot-from-volume MUST behave as in upstream OpenStack via the API/CLI/Terraform provider, with no client-side modification.

### Key Entities

- **Server (instance)**: A tenant virtual machine. Has an identity, a referenced image, one or more network ports (each with an assigned IP), a writable per-instance disk, and optional keypair and `user_data`.
- **Image**: A bootable template stored (as a raw base) by the image service. Tracks the disks that depend on it (instance overlays, copy-on-write volume clones); protected from deletion while any dependent disk exists.
- **Per-instance disk**: The server's own writable root disk, derived from the image and isolated per server; removed when the server is deleted.
- **Network / Subnet / Port**: The tenant network topology that provides a server its DHCP-assigned address and connectivity; rebuilt from persisted records after a control-plane restart.
- **Metadata**: The per-server cloud-init payload — instance-id, hostname, injected SSH key(s), and `user_data` (the core surface) — delivered to the guest via the link-local endpoint (OpenStack + EC2-compatible) and/or config drive.
- **Floating IP**: An externally routable address drawn from an operator-supplied pool and associable with a server for inbound access.
- **Keypair**: A tenant SSH public key injected into a server at creation to permit login.
- **Volume**: A block-storage device provisioned on real backing storage (node-local or RBD), attachable to a server or usable as a bootable root device; its lifecycle is independent of the server unless delete-on-termination is set.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Using only the standard OpenStack CLI or Terraform, a tenant can create a server in real mode and it reaches a running/active state within 3 minutes, with no disk-path or image errors.
- **SC-002**: From a server created with a keypair, an operator can establish an SSH session using the matching private key within 2 minutes of the server becoming active.
- **SC-003**: A running server retains its IP address and connectivity across a control-plane restart in 100% of trials, with zero manual intervention.
- **SC-004**: A booted server can reach a known external host, and can be reached from outside the node via its associated floating IP, in 100% of trials.
- **SC-005**: Two servers created from the same image are fully isolated — in repeated trials, writes inside one server are never observable in the other or in the source image.
- **SC-006**: Attempting to delete an in-use image is rejected 100% of the time with a clear error, while deleting an unused image succeeds 100% of the time.
- **SC-007**: A Terraform configuration that creates a compute instance with a keypair and `user_data`, plus a floating-IP association, applies cleanly against Lightstack with no provider modification, and the resulting VM reflects the keypair, the `user_data`, and external reachability.
- **SC-008**: After two consecutive control-plane restarts, the system shows no duplicate or orphaned network resources (addresses, bridges, namespaces, or rules).
- **SC-009**: A tenant can create a volume, attach it to a running real VM (guest sees the device), detach it, and separately boot a server from a volume — all via the standard OpenStack CLI/Terraform — with written data persisting across detach/re-attach in 100% of trials.
- **SC-010**: When a running server's DHCP service is killed out-of-band, its networking is restored automatically within one resync interval, with no manual intervention.

## Assumptions

- **Real mode is opt-in.** "Real mode" here means the compute service runs against libvirt and the network service runs against the host's real networking; both are enabled by operator configuration. Stub remains the default and is unaffected.
- **IP family: IPv4-only in v1.** All tenant networking, DHCP, metadata, floating IPs, and security-group rules are IPv4; IPv6 / dual-stack is deferred (see Out of Scope).
- **Capacity posture.** v1 does no scheduler-side capacity accounting or overcommit ratios; host/libvirt enforces resource limits and a failed allocation surfaces as an ERROR. There is no fixed concurrency target — it is bounded by the single node's resources.
- **Host prerequisites.** The single node provides KVM and a host libvirt daemon (the control plane connects to it via the host socket and does not bundle a hypervisor), mirroring the existing KVM Docker Compose stack.
- **Image runtime tooling.** The Lightstack container image used for real mode includes the tooling required to provision per-instance disks and to build config-drive images; the reference image build is updated accordingly. (DHCP tooling is already present.)
- **Node-local storage is acceptable on a single node, and is persistent.** Image bases, per-instance disk overlays, config-drive images, and local volumes all live on the node's persistent storage (`/var/lib/lightstack`) and survive pod restarts; shared backends are a multi-node concern.
- **Supported image formats.** Tenants may upload common cloud-image formats (raw, qcow2); uploads are normalized to a raw base on ingest (see FR-003 and Clarifications), mirroring OpenStack's `force_raw_images` behavior, so any supported format boots without the tenant pre-converting. Trade-off accepted: raw bases use full size on disk while per-instance overlays remain thin.
- **Floating-IP pool routing.** The operator supplies the floating-IP range as a standard external/provider network plus a routing mechanism (e.g. MetalLB or a routed/L2 subnet) that delivers that range to the node; Lightstack programs the per-address forwarding on the router. Egress and floating IPs both follow the standard OpenStack router model (see Clarifications).
- **Validation environment.** Acceptance is validated on a single Linux node (Ubuntu + KVM/libvirt) with a single-node k3s cluster and Helm, using a small standard cloud image (e.g. cirros). Real-mode acceptance cannot be exercised in a non-Linux sandbox.

## Dependencies

- **SPEC-008 Helm chart** — provides the single-node real-mode deployment topology (privileged pod, host networking, host libvirt socket, node-local persistence). This feature does not modify the chart's manifests.
- **SPEC-007 prerequisites** (admin-password bootstrap, configurable catalog scheme) — already delivered.
- **Existing real-mode product plumbing** — the libvirt hypervisor integration, network namespaces/bridges/DHCP, security groups, and the L3 router NAT + floating-IP API (the last of which WS-3 reuses).
- **lightstack container image** — must bundle the real-mode runtime tooling named in the Assumptions.
- **Block-storage backends and libvirt volume attach** — node-local and RBD Cinder backends plus libvirt attach/detach, exercised by real-mode block storage (US5).

## Out of Scope

- **Multi-node VM placement and cross-node request forwarding** (WS-1) — tracked as the v2 ticket (lightstack#52 Part B / cloud-storage#476).
- **Routing a floating IP to a hosting node in a multi-node cluster** — single-node ingress only here.
- **Host-reboot VM auto-resume (compute reconciliation)** — v1 rebuilds networking on startup but does not auto-restart VMs stopped by a full node reboot; survival is guaranteed across Lightstack pod/control-plane restarts (per the issue ACs), not node reboots. A modest follow-up given disks persist and networking rebuilds.
- **Advanced VM lifecycle operations** — snapshot (create-image-from-instance), resize, rescue, and suspend/pause are deferred beyond v1's core power lifecycle (create/delete/start/stop/reboot); live/cold migrate is inherently multi-node.
- **Graphical (noVNC) console** — v1 provides serial console + log only; the graphical noVNC console (and its VNC/websocket proxy) is deferred.
- **Extended metadata** — `network_data.json` (static guest network config; DHCP covers v1) and `vendor_data` are deferred beyond the core cloud-init surface.
- **IPv6 / dual-stack networking** — v1 is IPv4-only; IPv6 (DHCPv6/RA, v6 security-group rules, and IPv6's NAT-free ingress model) is deferred.
- **Object storage / Swift API** — Lightstack consumes S3/RGW only as an image backend; a tenant-facing object-storage API is a separate, centrally-decided item.
- **Non-Linux hosts** — real mode targets Linux; other platforms remain stub-only.
- **Any asynchronous/queue-based execution model** — explicitly excluded by FR-019.
