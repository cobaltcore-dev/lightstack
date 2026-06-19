# Tasks: Single-Node Real Mode (v1)

**Input**: Design documents from `docs/specs/009-single-node-real-mode/`
**Prerequisites**: plan.md, spec.md, research.md (R1–R12), data-model.md, contracts/ (5)

**Tests**: REQUIRED. The constitution mandates Test-First (Article III, NON-NEGOTIABLE) and contract tests with real OpenStack clients (Article I); each story's contracts are in `contracts/`. Write the test tasks first and confirm they FAIL before implementing. Real-mode (host) tests are authored now but executed on the Linux/k3s host (not in CI/sandbox); gophercloud contract tests + stub-mode unit tests run in CI.

**Organization**: by user story (priority order WS-0 → WS-2 → WS-4 → WS-3 → WS-5) so each is independently implementable and testable.

## Format: `[ID] [P?] [Story] Description with file path`

- **[P]**: parallelizable (different files, no dependency on an incomplete task)
- **[Story]**: US1=WS-0, US2=WS-2, US3=WS-4, US4=WS-3, US5=WS-5

## Path conventions

Existing single Go binary. Real paths: `cmd/o3k/`, `internal/{nova,neutron,cinder,glance,metadata,common,database}/`, `pkg/{hypervisor,networking,storage}/`, `migrations/`, `build/package/`, `config/`, `test/`, `test/contract/`, `test/terraform/`, `charts/o3k/`.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: harness + config plumbing the stories rely on.

- [X] T001 [P] Scaffold the contract-test harness: create `test/contract/` (Go package + gophercloud auth/helpers against a running O3K) and `test/terraform/` (OpenStack provider config pointing at O3K) with a smoke test. *(contract harness already existed in `test/contract/`; added `test/terraform/` README + `provider.tf.example` + `.gitignore`)*
- [X] T002 [P] Add config fields in `internal/common/config.go` and `config/o3k.yaml`: `neutron.reconcile_interval` (default `30s`) and a metadata bind address (default a VM-reachable value, not `localhost`). *(added `NeutronConfig.ReconcileInterval` + new `MetadataConfig.BindAddress`; round-trip verified)*

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ CRITICAL**: blocks all user stories.

- [X] T003 [P] Add migration `migrations/060_add_volume_source_image.up.sql` / `.down.sql` (`volumes.source_image_id UUID` + index, reversible) and add `SourceImageID` to the Volume struct in `internal/database/models.go`. *(up/down live-verified via `make db-up` + `o3k-migrate`: 0→60 clean, `source_image_id`+index present, reverses cleanly)*
- [X] T004 [P] Add `qemu-img` (qemu-utils) and `genisoimage` to the runtime packages in `build/package/Dockerfile` (`dnsmasq` already present). *(Alpine pkgs `qemu-img` + `cdrkit` (provides genisoimage); also pre-creates `instances/` + `cloud-init/` dirs; image build validated on host)*
- [X] T005 [P] Fix the Cinder local-volume base path from `~/.o3k/volumes` to `/var/lib/o3k/volumes` via a `resolveLocalPath`/`LocalVolumeDir` helper in `pkg/storage/ceph.go` (+ unit test in `pkg/storage/ceph_test.go`). NOTE: an identical fix exists on `feature/53` (`80de929`) — reuse/cherry-pick to avoid a merge conflict. *(added `LocalVolumeDir()`; 2 unit tests pass)*
- [X] T006 [P] Add a shared image-path helper (raw base path keyed off backend) in `pkg/storage/image_store.go`; make Nova's read site (`internal/nova/handlers.go`) and Glance's write sites call it so they cannot diverge. *(added `LocalImageDir`/`LocalImagePath`/`imageFileName`/`rbdImageName`/`s3ImageKey`; refactored all 17 Glance sites; Nova's read site adopts `LocalImagePath` in T010/US1)*

**Checkpoint**: schema, image tooling, persistent storage path, and the shared path helper are ready — stories can begin.

---

## Phase 3: User Story 1 — Boot a real VM from an image (Priority: P1) 🎯 MVP

**Goal**: a real VM boots from a Glance image via a per-instance qcow2 overlay; isolated per VM; image deletion guarded; core power lifecycle + serial console.

**Independent Test**: real mode, upload cirros, `openstack server create` → `ACTIVE`; the host shows a libvirt domain whose disk is `instances/<id>/disk.qcow2` backing `images/image-<id>.raw`; a second VM is isolated; `image delete` while in use → 409; `server stop/start/reboot` reflected; `console log show` returns boot output. (contracts/compute-disk-lifecycle.md)

### Tests (write first, MUST fail) ⚠️

- [ ] T007 [P] [US1] Contract test `test/contract/ws0_disk_test.go` (gophercloud): create-from-image→ACTIVE, two-VM isolation, image-delete→409, server-delete, `disk_format=raw` after upload.
- [ ] T008 [P] [US1] Host e2e tests `test/ws0_disk_real_test.sh`, `test/ws0_image_inuse_test.sh`, `test/ws0_console_test.sh` (`virsh domblklist`, `qemu-img info` backing-file/`-F raw`, sentinel-file isolation).

### Implementation

- [ ] T009 [US1] Normalize uploaded images to a raw base on ingest (`qemu-img convert -O raw`) in `pkg/storage/image_store.go`; report `disk_format=raw` in `internal/glance/images.go`. (uses T006)
- [ ] T010 [US1] Stage a per-instance qcow2 overlay in `internal/nova/handlers.go` (`qemu-img create -f qcow2 -b <raw> -F raw /var/lib/o3k/instances/<id>/disk.qcow2`; chown/chmod for the host qemu uid; set `spec.ImagePath` to the overlay). (uses T006, T009)
- [ ] T011 [US1] Remove `/var/lib/o3k/instances/<id>/` on VM delete in `internal/nova/handlers.go`.
- [ ] T012 [US1] Image-in-use guard in `internal/glance/images.go` `DeleteImage` (reject 409 while any `instances.image_id` or `volumes.source_image_id` references it). (uses T003)
- [ ] T013 [US1] Core power lifecycle (start / stop / soft+hard reboot) against libvirt in `pkg/hypervisor/libvirt.go` and the Nova action handlers; reflect `power_state`/`status`.
- [ ] T014 [US1] Serial console log (`openstack console log show`) — read the domain serial log in `pkg/hypervisor/libvirt.go` and expose via `internal/nova`.
- [ ] T015 [US1] Capacity fail-fast: a host allocation failure marks the instance `ERROR` with a clear message in `internal/nova/handlers.go` (no overcommit accounting).

**Checkpoint**: US1 fully functional and independently testable — MVP boundary.

---

## Phase 4: User Story 2 — VM networking survives a control-plane restart (Priority: P2)

**Goal**: a startup + periodic reconciler rebuilds network state from the DB (idempotent, best-effort); VM keeps its IP + disk across a pod restart; self-heals a killed dnsmasq.

**Independent Test**: boot a VM, note its IP; restart o3k; same IP + connectivity, disk intact; kill dnsmasq → restored within one interval; two restarts → no duplicate netns/bridges/leases; a corrupt tenant doesn't block readiness. (contracts/network-reconciliation.md)

### Tests (write first, MUST fail) ⚠️

- [ ] T016 [P] [US2] Host e2e tests `test/ws2_restart_survival_test.sh`, `test/ws2_selfheal_test.sh`, `test/ws2_partial_failure_test.sh`.

### Implementation

- [ ] T017 [US2] New reconciler `internal/neutron/reconciler.go`: struct + `NewNetworkReconciler(...)` + `Start(ctx)` (initial sync + ticker on `reconcile_interval`) + `Stop()`, modeled on `internal/neutron/vxlan_coordinator.go`.
- [ ] T018 [US2] Reconcile sync in `internal/neutron/reconciler.go`: rebuild bridges / netns / taps / security-group rules from the DB, idempotent and best-effort per resource (one tenant's failure is logged + retried, never blocks readiness); reuse the build funcs in `internal/neutron/network.go` and `ports.go`.
- [ ] T019 [US2] DB-driven dnsmasq MAC→IP host reservations in `pkg/networking/dhcp.go` so a rebuilt dnsmasq hands back the same IP (stable IP per FR-006).
- [ ] T020 [US2] Wire `go reconciler.Start(ctx)` into `cmd/o3k/main.go` after the VXLAN-coordinator block (~L185), gated on `networkingMode != "stub"`; add `reconciler.Stop()` to the shutdown block.

**Checkpoint**: US1 + US2 both work; VMs survive a control-plane restart.

---

## Phase 5: User Story 3 — Reach & log into the VM via cloud-init (Priority: P3)

**Goal**: metadata is VM-reachable (per-namespace `169.254.169.254` DNAT), namespace/project-aware (overlapping subnets), serves the core surface on OpenStack + EC2 endpoints; `user_data` accepted/persisted/served; config-drive works end-to-end.

**Independent Test**: create with `--key-name` + `--user-data`; SSH login with the key; `user_data` applied; metadata returns own data only (overlapping-subnet isolation); config-drive provisions without the network redirect; Terraform `key_pair`+`user_data` applies. (contracts/metadata-cloudinit.md)

### Tests (write first, MUST fail) ⚠️

- [ ] T021 [P] [US3] Contract test `test/contract/ws4_userdata_test.go` (gophercloud): create with `UserData`+`KeyName`, read back `instance_userdata`.
- [ ] T022 [P] [US3] Host/guest tests `test/ws4_metadata_test.sh`, `test/ws4_isolation_test.sh`, `test/ws4_configdrive_test.sh` and `test/terraform/ws4/`.

### Implementation

- [ ] T023 [US3] Bind the metadata server on a VM-reachable address in `internal/metadata/service.go` and `cmd/o3k/main.go` (replace `metadata.NewService("localhost:8775")`).
- [ ] T024 [US3] New `pkg/networking/metadata_redirect.go`: per-namespace `169.254.169.254:80 → metadata` DNAT + a network-context marker on the forwarded request. **Program it synchronously at subnet/namespace creation** in `internal/neutron/` (the same point DHCP is started today), so a newly-created network has metadata reachability immediately — not only after the next reconcile tick (Constitution II, synchronous).
- [ ] T025 [US3] Namespace/project-aware instance identity in `internal/metadata/service.go` (resolve by network context + source IP; correct under overlapping subnets).
- [ ] T026 [US3] Serve the core surface (instance-id, hostname, public-keys, `user_data`, `meta_data.json`) on both OpenStack (`/openstack/...`) and EC2 (`/latest/...`) endpoints in `internal/metadata/service.go`.
- [ ] T027 [US3] `user_data` write-path: add `user_data` to `CreateServerRequest` and persist to `instance_userdata` in `internal/nova/handlers.go`.
- [ ] T028 [US3] Config-drive end-to-end in `internal/nova/handlers.go` + `pkg/hypervisor/xml_template.go`: ensure `GenerateCloudInitISO` runs (genisoimage from T004), embeds key + `user_data`, and the ISO is attached.
- [ ] T029 [US3] Replay the per-namespace metadata DNAT (from T024) in the reconciler `internal/neutron/reconciler.go` so it is restored after a restart (T024 programs it at create time; this restores it on boot/resync). (depends on US2 T017)

**Checkpoint**: US1–US3 work; VMs are loginnable and cloud-init-provisioned.

---

## Phase 6: User Story 4 — North-south networking (Priority: P4)

**Goal**: reuse the existing router SNAT + floating-IP DNAT; auto-map the external network to the node uplink (no operator bridge/NIC); security groups enforced on north-south.

**Independent Test**: VM on a router with an external gateway reaches the internet (egress out the uplink); allocate+associate a floating IP → reachable from outside (only on SG-allowed ports); disassociate → access stops; Terraform floating-IP applies. (contracts/north-south.md)

### Tests (write first, MUST fail) ⚠️

- [ ] T030 [P] [US4] Host tests `test/ws3_egress_test.sh`, `test/ws3_floatingip_test.sh`, `test/ws3_secgroup_test.sh` and `test/terraform/ws3/`.

### Implementation

- [ ] T031 [US4] Auto-detect the node's primary uplink (default-route iface) and map a router's external gateway to it in `internal/neutron/router.go` + `pkg/networking/router.go` (no operator bridge/NIC).
- [ ] T032 [US4] Activate egress SNAT in real mode (reuse `EnableSNAT`) via `configureExternalGateway` in `internal/neutron/router.go`.
- [ ] T033 [US4] Floating-IP DNAT + return path in real mode (reuse `internal/neutron/floatingip.go` + `pkg/networking/router.go` `AddFloatingIP`); model the FIP pool as a standard external/provider network.
- [ ] T034 [US4] Enforce security groups on north-south (DNAT in PREROUTING then the SG filter chain) in `pkg/networking/security_groups.go` + `pkg/networking/router.go`.
- [ ] T035 [US4] Add NAT / SG / floating-IP replay to the reconciler in `internal/neutron/reconciler.go`. (depends on US2 T017)
- [ ] T036 [US4] Document the operator-supplied floating-IP pool (MetalLB / routed-or-L2 subnet) in `charts/o3k/README.md` and `docs/specs/009-single-node-real-mode/quickstart.md`.

**Checkpoint**: US1–US4 work; VMs reach the internet and are reachable via floating IPs.

---

## Phase 7: User Story 5 — Block storage in real mode (Priority: P5)

**Goal**: real-mode volume create; synchronous attach/detach (fixing the async goroutine); volume-from-image; boot-from-volume.

**Independent Test**: create a volume (real backing); attach to a running VM (synchronous; guest sees the device); write/detach/re-attach → data persists; boot-from-volume boots off the volume; volume-from-image sets `source_image_id` and pins the image; Terraform volume applies. (contracts/block-storage.md)

### Tests (write first, MUST fail) ⚠️

- [ ] T037 [P] [US5] Contract test `test/contract/ws5_volume_test.go` (gophercloud): create/attach/detach/delete, assert synchronous attach + status transitions.
- [ ] T038 [P] [US5] Host tests `test/ws5_attach_real_test.sh`, `test/ws5_boot_from_volume_test.sh`, `test/ws5_volume_from_image_test.sh` and `test/terraform/ws5/`.

### Implementation

- [ ] T039 [US5] Make volume attach synchronous + fail-fast in `internal/nova/volume_attachment.go`: fold the libvirt `AttachDevice` call into the request path, revert the DB attachment on libvirt failure, remove the background goroutine.
- [ ] T040 [US5] Volume-from-image: honor `ImageRef` in `internal/cinder/volumes.go` (local copy of the raw base / RBD CoW clone via `pkg/storage/ceph.go`); set `volumes.source_image_id` and `bootable=true`. (uses T003, T006)
- [ ] T041 [US5] Boot-from-volume: parse `block_device_mapping_v2` in `CreateServerRequest` and populate `VMSpec.Volumes` (root = the volume) in `internal/nova/handlers.go`; honor `delete_on_termination` on server delete.

**Checkpoint**: all five stories independently functional.

---

## Phase 8: Polish & Cross-Cutting Concerns

- [ ] T042 [P] Stub-mode unit tests for the new logic (image-path helper, overlay-staging guards, reconciler sync funcs, metadata identity) alongside the sources as `*_test.go`.
- [ ] T043 [P] Reconcile the over-claiming status docs to reflect the closed real-mode gaps: `docs/STUB_PLACEHOLDERS.md`, `docs/WHATS_LEFT.md`, `docs/REAL_MODE_TESTING.md`; add a real-mode single-node recipe to `charts/o3k/README.md`.
- [ ] T044 Run the `quickstart.md` e2e on the Ubuntu + libvirt + single-node k3s host (cirros) and confirm SC-001…SC-010.

---

## Dependencies & Execution Order

### Phase dependencies
- **Setup (P1)**: no deps.
- **Foundational (P2)**: after Setup; **blocks all stories**.
- **US1 (P3)** → **US2 (P4)** → **US3 (P5)** → **US4 (P6)** → **US5 (P7)**: priority/dependency order. Each is independently testable after Foundational; US3's and US4's *restart durability* tasks (T029, T035) depend on US2's reconciler (T017), but their first-boot behavior does not.
- **Polish (P8)**: after the desired stories.

### Cross-story dependencies (minimal)
- T029 (metadata DNAT replay) and T035 (NAT/SG replay) depend on T017 (reconciler).
- T012 (image-in-use) and T040 (volume-from-image) depend on T003 (migration).
- T010/T040 depend on T006 (shared path helper); T010 also on T009.

### Within a story
- Tests (RED) before implementation. Same-file impl tasks run sequentially (notably `internal/nova/handlers.go` and `internal/metadata/service.go` are hot files).

### Parallel opportunities
- Setup T001‖T002; Foundational T003‖T004‖T005‖T006 (different files).
- Each story's test tasks are [P] (different test files).
- With staff, after Foundational the stories can proceed in parallel; the only true cross-links are T029/T035→T017.

---

## Parallel Example: Foundational

```bash
# All four foundational tasks touch different files — run together:
T003  migrations/060_* + internal/database/models.go
T004  build/package/Dockerfile
T005  pkg/storage/ceph.go (+ ceph_test.go)
T006  pkg/storage/image_store.go (+ Nova read site)
```

## Parallel Example: User Story 1 tests

```bash
T007  test/contract/ws0_disk_test.go      # gophercloud
T008  test/ws0_disk_real_test.sh + ws0_image_inuse_test.sh + ws0_console_test.sh
```

---

## Implementation Strategy

### MVP first
1. Phase 1 Setup → 2. Phase 2 Foundational → 3. Phase 3 US1 → **STOP & validate** (real VM boots, isolated, lifecycle, console). This is the gating MVP.

### Incremental delivery
US1 (boot) → US2 (survive restart) → US3 (login/cloud-init) → US4 (north-south) → US5 (block storage). Each adds value without breaking the previous; validate against its contract + the matching SCs before moving on.

### Scope note
This is a large feature (additive block storage + lifecycle + console beyond the original four workstreams). Real-mode acceptance (US1–US5 host tests + T044) runs on the Linux/k3s node, not in CI. RBD block-storage paths (T040 CoW) need a `-tags ceph` build + a real Ceph fixture and are validated separately from the local-backend single-node run.

---

## Notes
- [P] = different files, no incomplete dependency. [Story] maps the task to its user story.
- Verify each test FAILS before implementing (Constitution III).
- Commit after each task or logical group (DCO-signed: `git commit -s`).
- Stop at any checkpoint to validate a story independently.
- Requirement-coverage notes: **FR-021** (volume create provisions real backing) = T005 (local-path fix) + the existing Cinder `CreateVolume`; **FR-006a** (per-instance disk / config-drive / local-volume artifacts persist on the node hostPath) = T005 + T010 + the existing `cloud-init`/`images` paths, validated by T016. Neither needs a dedicated implementation task.
- Total: **44 tasks** — Setup 2, Foundational 4, US1 9, US2 5, US3 9, US4 7, US5 5, Polish 3.
