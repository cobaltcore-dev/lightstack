# Blueprint test coverage — §3 endpoints · §4 storage backends · §6 Terraform

**Created**: 2026-07-06
**Scope**: every endpoint the blueprint ([`blueprint.md`](blueprint.md) §3.1–3.5) marks **implemented** — plus the storage-backend behaviors (§4) and Terraform resource coverage (§6) — checked for a test that verifies it works.
**Method**: each implemented endpoint (from the actual route registrations in `internal/<service>/`, plus the Nova/Cinder action dispatchers) was mapped to a covering test. An endpoint is **covered** if ≥1 gophercloud contract-test function (`test/contract/<service>/`) or ≥1 integration-script step (`test/*.sh`) exercises that method+path. Grounded in the source and the test files, not the prose.
**Caveat**: contract tests are **not CI-gated** (they need a live instance — see blueprint §8), so "covered" means *a test exists*, not that it runs in CI.

## Summary

| Service | Implemented | Covered | Missing |
|---|--:|--:|--:|
| Keystone (Identity v3) | 60 | 53 | **7** |
| Nova (Compute v2.1) | 106 | 99 | **7** |
| Neutron (Network v2.0) | 95 | 93 | **2** |
| Cinder (Block Storage v3) | 76 | 70 | **6** |
| Glance (Image v2) | 38 | 35 | **3** |
| **Total** | **375** | **350** | **25** |

> Nova's 106 counts the 34 `POST /servers/{id}/action` sub-actions individually; Cinder's 76 counts the wired `os-*` volume/type/backup actions. "Missing" = the route/action is implemented but no contract test or integration script exercises it.

**Beyond §3** (different units — see their own sections below): **§4 storage backends** — 5 of 15 behaviors covered, 10 missing (CI exercises `local` mode only). **§6 Terraform** — sc007 covers 3 of #423's 5 smoke categories, and ~13 o3k-supported provider resources are not exercised (and it is not CI-gated).

---

## Keystone (Identity v3) — 7 missing

| Endpoint | Handler | Suggested test |
|---|---|---|
| `GET /v3/auth/tokens` (validate) | `ValidateToken` | `TestKeystoneValidateToken_Contract` — issue a token, `GET` with `X-Subject-Token`, assert 200 + `token` body; garbage/expired → 401/404. (Scripts only *extract* the token from the POST, never validate it.) |
| `DELETE /v3/auth/tokens` (revoke) | `RevokeToken` | `TestKeystoneRevokeToken_Contract` — issue, `DELETE` with `X-Subject-Token`, assert 204, then confirm the token no longer validates. |
| `GET /v3/projects/{id}/users/{uid}/roles` | `ListUserProjectRoles` | `TestKeystoneListUserProjectRoles_Contract` — raw `GET` after assigning a role; assert the role appears. (Existing tests hit `GET /v3/role_assignments` instead.) |
| `GET /v3/projects/{id}/users/{uid}/roles/{rid}` | `CheckRoleAssignment` | `TestKeystoneCheckRoleAssignmentDirect_Contract` — raw `GET`/`HEAD`; 200/204 when assigned, 404 when not. |
| `GET /v3/groups/{id}/users` | `ListGroupUsers` | `TestKeystoneListGroupUsers_Contract` — add a member, list, assert present. |
| `PUT /v3/groups/{id}/users/{uid}` | `AddUserToGroup` | `TestKeystoneAddUserToGroup_Contract` — assert 204 + membership visible. |
| `DELETE /v3/groups/{id}/users/{uid}` | `RemoveUserFromGroup` | `TestKeystoneRemoveUserFromGroup_Contract` — add then remove, assert 204 + member gone. |

**Weak (route exercised, but no positive assertion):** `GET /v3/users/{id}` and `GET /v3/projects/{id}` are hit only via the post-delete 404 path in the Delete tests — add positive `TestKeystoneGetUser_Contract` / `TestKeystoneGetProject_Contract` fetches.

## Nova (Compute v2.1) — 7 missing

| Endpoint / Action | Handler | Suggested test |
|---|---|---|
| `GET /v2.1/images` | `ListImages` (Glance proxy) | `nova/images_test.go · TestNovaListImages_Contract` — assert the Nova→Glance proxy returns 200 + `images` array. |
| `GET /v2.1/images/detail` | `ListImagesDetail` | `TestNovaListImagesDetail_Contract` — verify detailed image fields are proxied. |
| `GET /v2.1/os-hypervisors/detail` | `ListHypervisorsDetail` | extend `hypervisors_test.go` with `TestNovaListHypervisorsDetail_Contract` (only brief list + `/statistics` are tested). |
| `GET /v2.1/servers/{id}/os-volume_attachments/{volume_id}` | `GetVolumeAttachment` | add a "show single attachment" step to `test/volume_attach_test.sh` (only list/attach/detach exist). |
| `confirmResize` action | `ConfirmResizeInstance` | step in `advanced_actions_test.sh` (or `TestNovaConfirmResize_Contract`): resize → `{"confirmResize": null}` → 202. |
| `revertResize` action | `RevertResizeInstance` | companion `{"revertResize": null}` → 202. |
| `shelveOffload` action | `ShelveInstance` (dispatch `handlers.go:1194`) | step sending `{"shelveOffload": null}` — the `shelve` key is tested, this distinct branch is not. |

**Note:** `PATCH /servers/{id}` shares `UpdateServer` with `PUT` (covered via gophercloud `servers.Update`); the PATCH verb itself is not independently exercised. `rbac_test.go` was revived by SPEC-023: it had been disabled as `rbac_test.go.broken` because of a gophercloud API drift (`ResetState` takes a bare `ServerState` in v1.14.1, not the `ResetStateOpts` struct it was written against). Its reset-state and evacuate tests are the contract-level cover for FR-008a. Three of its five original tests were deliberately not revived — one never compiled and duplicated `TestQuotaWritesRequireAdmin`, one asserted only that an admin sees their own server (true with or without isolation), and one needed a `reader` role assignment the harness cannot make; cross-project coverage lives in `internal/nova/authz_test.go` instead.

## Neutron (Network v2.0) — 2 missing

| Endpoint | Handler | Suggested test |
|---|---|---|
| `GET /v2.0/security-group-rules/{id}` | `GetSecurityGroupRule` | after creating a rule, `GET` it and assert `id` + `protocol`/`direction`/`port_range_*` round-trip. `security_groups_test.sh` only lists + deletes rules; no contract file exists for SG. |
| `PUT /v2.0/trunks/{id}/remove_subports` | `RemoveSubports` | extend `trunks_test.go · TestTrunkSubports` — remove the added subport, then `GET` the trunk and assert `sub_ports` empty. (Only `add_subports` is tested.) |

**Weak:** `PUT /subnets/{id}`, `PUT /ports/{id}`, `PUT /security-groups/{id}` — the handlers are verified only via the **PATCH** verb (`neutron_patch_test.sh`); add a PUT case alongside each. `DELETE /metering/metering-label-rules/{id}` is exercised only as unasserted cleanup — add a dedicated delete-and-verify test.

## Cinder (Block Storage v3) — 6 missing

| Endpoint / Action | Handler | Suggested test |
|---|---|---|
| `POST /v3/volumes/{id}/action` `os-attach` | `VolumeAction` (`volumes.go:609`) | contract test posting `{"os-attach":{instance_uuid…}}` → 202 + status `in-use`. The attach *workflow* is validated only through **Nova** (`os-volume_attachments`), which writes the `volumes` row directly and never hits this Cinder route. |
| `os-detach` | `VolumeAction` (`volumes.go:639`) | attach then `{"os-detach":{}}` → 202 + status `available`. |
| `GET /v3/snapshots` | `ListSnapshots` | snapshots are created/fetched/deleted but never **listed** — add `snapshots.List().AllPages()` asserting the snapshot appears. |
| `GET /v3/snapshots/detail` | `ListSnapshotsDetail` | list-detail variant, assert full snapshot fields. |
| `GET /v3/types/default` | `GetDefaultVolumeType` | distinct handler, untested — assert 200 + a `volume_type` with non-empty `id`/`name`. |
| `GET /v3/backups/detail` | `ListBackupsDetail` | only plain `GET /backups` is tested — add `backups/detail` asserting size/volume_id/status. |

**Note:** version-discovery `GET /` and `GET /v3` (`GetVersions`/`GetVersionV3`) have no dedicated Cinder test either (out of the §3 functional scope).

## Glance (Image v2) — 3 missing

| Endpoint | Handler | Suggested test |
|---|---|---|
| `GET /v2/schemas/image` | `GetImageSchema` | `TestGlanceGetImageSchema_Contract` — 200 + `.name == "image"` with non-empty `.properties`. (`glance_schemas_test.sh` only checks `member`/`members`.) |
| `GET /v2/schemas/images` | `GetImagesSchema` | `TestGlanceGetImagesSchema_Contract` — 200 + images-collection schema. |
| `GET /v2/stores/info` | `GetStoresInfo` | `TestGlanceGetStoresInfo_Contract` — 200 + non-empty `stores` array (each with `id`/`default`). Only `GET /stores` is tested. |

## Storage backends (blueprint §4) — 10 of 15 behaviors untested

**CI reality:** the `contract-tests` CI job exercises **`local` mode only** — `build/package/Dockerfile` bakes `config/o3k.yaml` (`storage_mode: local` for both Glance and Cinder), `deployments/docker-compose.yml` adds no S3/MinIO/Ceph service, and no CI job passes `-tags ceph`. So every non-`local` backend is unexercised. The chart tests (`charts/o3k/tests/*_test.yaml`) validate only that the chart *accepts/rejects* `storageMode` values (config plumbing), never backend I/O.

**Covered (5):** Glance `stub` + `local` (`test/contract/glance/image_data_test.go`, `pkg/storage/paths_test.go`); Cinder `local` (`test/contract/cinder/volume_lifecycle_test.go`, `test/contract/ws5_volume_test.go`); Nova VM-disk qcow2 overlay (`test/ws0_disk_real_test.sh`) + boot-from-volume (`test/ws5_boot_from_volume_test.sh`).

**Missing (10):**

| Backend / behavior | Suggested test | CI-feasible? |
|---|---|---|
| Glance `rbd` (placeholder) | unit: `NewImageStore("rbd")` ops return the "ceph not configured" error | ✅ stub — no Ceph |
| Glance `local,rbd` (log-only) | unit: upload writes the local raw file + returns success, RBD only logged | ✅ no Ceph |
| Glance `rbd,s3` (S3-skipped branch) | unit: assert the "reader consumed → S3 skipped" branch (`image_store.go:195-203`) | ✅ skip logic (RBD leg needs `ceph` build) |
| Glance `s3` (AWS/MinIO/RGW) | integration: `storage_mode: s3` + `s3_endpoint`, PUT `…/file` → GET identical bytes, path-style | ❌ needs MinIO/LocalStack |
| Glance `local,s3` (replication) | integration: upload lands in local **and** S3; S3 failure keeps the local copy | ❌ needs MinIO/S3 |
| Cinder `stub` (CRUD) | unit: create-dup → "exists", delete-missing → "not found", `VolumeExists` | ✅ trivial (low priority) |
| Cinder `rbd` (real go-ceph) | build-tagged (`//go:build ceph`) test of create/delete/snapshot/health in `ceph_rbd.go` (0% coverage) | ❌ needs `-tags ceph` + real Ceph |
| Cinder `local,rbd` (rollback) | unit (default `!ceph` build): RBD leg fails → assert local qcow2 rolled back (`ceph.go:94-107`) | ✅ `!ceph` stub fails the RBD leg |
| Ceph build-tag guard | unit: default build → "not compiled (build with -tags ceph)"; + a `go build -tags ceph ./...` CI job | ✅ stub assert (compile job needs librados/librbd) |
| Fail-fast timeouts (Ceph 1 s / S3 5 s) | unit: assert `.timeout` fields (`ceph.go:65`, `image_store.go:95`) + a slow-backend op errors within budget | ✅ yes |

**Weak (real-host only, no unit test):** Nova `StageOverlay` (`pkg/hypervisor/disk.go`) is covered by `ws0_disk_real_test.sh` on a real host, but `disk_test.go` tests only `InstancePaths`, not the `qemu-img create -b … -F raw` overlay — add a CI unit test (needs only `qemu-img`).

## Terraform coverage (blueprint §6) — #423 gaps + o3k-supported resources not exercised

**What sc007 covers:** one real-mode module (`test/terraform/sc007/main.tf`, unmodified provider `~> 3.0`) — 10 resource types + 2 data sources: networks/subnets (tenant + external), router + interface, keypair, instance (+ `user_data`), floating IP + associate, `blockstorage_volume_v3`, `compute_volume_attach_v2`; data sources `images_image_v2` (lookup) + `networking_port_v2` (the FIP port-id workaround). **Not CI-gated** (`test/terraform/README.md` says so; `ci.yml:261` has only a "Planned" TODO). The README is stale — it lists removed `ws3/ws4/ws5/` dirs; only `sc007/` remains.

**vs #423's required smoke set:** compute instances ✅, networks ✅, volumes ✅; **security groups ❌** (API exists, untested); **object storage ❌** (no Swift API — impossible).

**Missing Terraform tests** (o3k-supported resources not exercised, prioritized):

| Terraform resource | o3k API | Priority / note |
|---|---|---|
| `openstack_networking_secgroup_v2` + `_rule_v2` | ✅ Neutron (§3.3) | **#423 priority** — create secgroup + rules, attach to the instance |
| `openstack_images_image_v2` (as resource) | ✅ Glance create + `PUT …/file` (§3.5) | image *upload* via TF untested (sc007 only reads); validation-critical for #422 |
| `openstack_compute_flavor_v2` | ✅ Nova (§3.2) | flavor create untested (sc007 uses an existing `m1.small`) |
| `openstack_blockstorage_snapshot_v3` | ✅ Cinder (§3.4) | snapshot lifecycle untested |
| `openstack_blockstorage_volume_type_v3` | ✅ Cinder (§3.4) | volume-type create untested |
| `openstack_networking_trunk_v2` | ✅ Neutron (§3.3) | parent port + subport trunk untested |
| `openstack_networking_qos_policy_v2` (+ bw-limit rule) | ✅ / ⚠️ bw-limit only (§3.3) | QoS policy + bandwidth-limit rule; **not** DSCP / min-bandwidth |
| `openstack_networking_port_v2` (as resource) | ✅ Neutron (§3.3) | managed port untested (sc007 only reads a port) |
| `openstack_identity_{project,user,role}_v3` | ✅ Keystone (§3.1) | core IAM untested via TF |
| `openstack_identity_role_assignment_v3` | ⚠️ project↔user only (§3.1) | project-scoped only; avoid group/domain scopes |
| `openstack_blockstorage_qos_v3` | ⚠️ registered-but-thin (§3.4) | low confidence; would raise the contract signal |
| `openstack_networking_{subnetpool,rbac_policy,addressscope}_v2` | ✅ Neutron (§3.3) | lower priority |
| boot-from-volume via `compute_instance_v2` `block_device` | ✅ host-validated | works on host (`ws5_boot_from_volume`), never exercised via TF |

**Do NOT add (would fail against o3k):** `openstack_compute_secgroup_v2`, `openstack_compute_floatingip_v2` / `_associate_v2` (legacy nova-network — ❌ §3.2; use the Neutron equivalents), `openstack_identity_ec2_credential_v3` (OS-EC2 not registered — §3.1), `openstack_networking_qos_dscp_marking_rule_v2` (§3.3), and all `openstack_objectstorage_*` (no Swift API).

---

## How this list was produced

Seven parallel audits (the five §3 services, plus §4 storage backends and §6 Terraform) cross-referenced:
- **Routes** — `internal/keystone/handlers.go`, `internal/nova/handlers.go` (+ the `ServerAction` dispatcher), `internal/neutron/network.go`, `internal/cinder/volumes.go` (+ `VolumeAction`/`VolumeTypeAction`/`BackupAction`), `internal/glance/images.go`.
- **Contract tests** — `test/contract/<service>/*.go` (82 files, 308 `Test*` functions).
- **Integration scripts** — `test/*.sh`.
- **Storage (§4)** — `pkg/storage/*_test.go`, the `ws0`/`ws5` host scripts, and `build/package/Dockerfile` + `deployments/docker-compose.yml` + `.github/workflows/ci.yml` (to establish what CI actually exercises).
- **Terraform (§6)** — `test/terraform/**` (`.tf` + `README.md`) and `.github/workflows/ci.yml`.

The §3 gaps (25) are almost entirely read-side variants (`/detail`, show-one, schema/info GETs), token-lifecycle and group-membership sub-routes, and a few action-dispatch branches — none on the core validation path, which is well covered. The larger exposure is **§4**: CI verifies only `local` storage, so every Ceph/RBD and S3/RGW/MinIO path — the backends #422 depends on — is unverified; and **§6**: the Terraform smoke is one un-gated topology that omits #423's security groups (object storage being impossible without a Swift API).
