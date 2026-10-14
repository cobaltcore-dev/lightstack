# SPEC ↔ Contract-Test Cross-Index

This index maps every specification in `docs/specs/NNN-*/` to the gophercloud-based
contract tests in `test/contract/` that pin it, and — in the other direction —
every contract test file to the spec it cites (if any). It exists to close the
"Specifications → Contracts" link required by issue #16, and to serve as the
scoping tool for issue #105 (testing gaps): every test file with a `—` in Table 2
is unspec'd coverage, and every implemented, API-observable spec with no pinning
test in Table 1 is a genuine gap.

Everything below is generated from test-source evidence (header-comment
citations of the form `SPEC-NNN FR-xxx`), not from memory. The exact commands
used are in [Maintenance](#maintenance) so this file can be regenerated and
kept current. Where a mapping is inferred from file/topic naming rather than an
explicit citation, it is marked **(inferred)**.

> **Ambiguity note — the duplicate SPEC-025.** Two spec dirs share the number
> 025: `025-followup` and `025-nova-gaps-client-discovery`. Both were
> implemented on branch `feature/547` from cloud-storage#547, and both use
> overlapping FR numbering (FR-001…FR-056) and US1–US9 user stories, so a bare
> `SPEC-025` citation is ambiguous on its face. In practice every test
> citation disambiguates itself with a `cloud-storage#547` or finding-ID
> reference (B4, B7, B13, B14, B16, F16) that belongs to
> **025-nova-gaps-client-discovery**; all SPEC-025 rows below are attributed to
> that dir. `025-followup` has no citation that distinguishes it.

---

## Table 1: Spec → contract tests

| Spec (dir) | Name | `test/contract/` file(s) | Requirement coverage | Notes |
|---|---|---|---|---|
| SPEC-000 (`000-api-compliance`) | OpenStack API Compliance Framework | — (the suite as a whole) | — | Meta-spec: the entire `test/contract/` suite is its deliverable **(inferred)**; no test cites SPEC-000 directly. |
| SPEC-001 (`001-modular-architecture`) | Modular Architecture Transformation | — | — | Architecture spec, not API-observable via gophercloud. |
| SPEC-002 (`002-authentication-enhancement`) | Enhanced Authentication System | `nova/rbac_test.go` | T048 (task ref) | Historical origin only: the file header says the tests are "originally T048 of SPEC-002" and that the remaining SPEC-002 tests were deliberately **not** revived. Effectively superseded by SPEC-023. |
| SPEC-003 (`003-barbican`) | Barbican — Key Management | — | — | Future service (not implemented); expected empty. |
| SPEC-004 (`004-designate`) | Designate — DNS as a Service | — | — | Future service (not implemented); expected empty. |
| SPEC-005 (`005-octavia`) | Octavia — Load Balancing | — | — | Future service (not implemented); expected empty. |
| SPEC-006 (`006-v070-foundation`) | v0.7.0 Foundation | — | — | Architecture/quality spec, not API-observable. |
| SPEC-007 (`007-helm-prereqs-bootstrap-catalog`) | Helm Prerequisites — Bootstrap + Catalog | — | — | Helm-level; verified by unit/render tests, not gophercloud contract tests. |
| SPEC-008 (`008-helm-charts`) | Helm Charts for Kubernetes Deployment | — | — | Helm-level; verified by chart/unit tests. |
| SPEC-009 (`009-single-node-real-mode`) | Single-Node Real Mode (v1) | `ws0_disk_test.go` (WS-0), `ws4_userdata_test.go` (WS-4), `ws5_volume_test.go` (WS-5) | WS-0: FR-003, FR-004; WS-5: FR-021, FR-022; WS-4: (workstory ref only) | Verified in both directions: the spec's `contracts/compute-disk-lifecycle.md`, `contracts/metadata-cloudinit.md`, and `contracts/block-storage.md` each name the corresponding test file. |
| SPEC-010 (`010-helm-teardown`) | Teardown via Helm Pre-Delete Hooks | — | — | Helm/host-level; verified by unit + host e2e, not contract tests. |
| SPEC-011 (`011-ensure-storage-subdirs`) | Ensure Node-Local Storage Directories | — | — | Host-level, not API-observable. |
| SPEC-012 (`012-integration-contract`) | Integration Contract (Values Schema + Status) | — | — | Helm values/status conventions, not API-observable via gophercloud. |
| SPEC-013 (`013-readiness-liveness-probes`) | Readiness/liveness endpoints | — | — | k8s health endpoints; verified outside `test/contract/`. |
| SPEC-014 (`014-ceph-rgw-s3-backend`) | Ceph RGW (S3) — Glance backend & tenant object storage | `keystone/ec2_authz_test.go`, `keystone/ec2_credentials_test.go` | FR-005, FR-007 | Only the EC2-credentials (S3-auth) slice is contract-pinned; the RGW/Glance-backend slice is host-verified. |
| SPEC-015 (`015-identity-uuid-404`) | Identity resource not-found for malformed ids | — | — | **Genuine gap.** API-observable (Keystone 404 shape), implemented, but no contract test cites it. Conformance was verified once via host testing (`CONFORMANCE.md`, DevStack comparison #73), not pinned by a regression test. Candidate for #105. |
| SPEC-016 (`016-terraform-openstack-provider-verification`) | Verify Terraform OpenStack-provider compatibility | — | — | Verification campaign (manual + integration), not a behavior spec; no dedicated contract tests expected. `server_readback_test.go` grew out of this work **(inferred)** but cites only SPEC-017. |
| SPEC-017 (`017-nova-server-detail-readback`) | Nova server-detail read-back | `server_readback_test.go` | finding F1 (spec ref) | Spec's `contracts/nova-server-detail.md` also names `test/contract/` (gophercloud) as the verifying layer — mapping verified in both directions. |
| SPEC-018 (`018-horizon-dashboard-verification`) | Verify Horizon dashboard compatibility | — | — | Verification campaign (browser-level report); not contract-test-pinned by design. |
| SPEC-019 (`019-horizon-compat-api-fixes`) | Horizon compat — Cinder/Neutron/Glance API fixes (F4, F5, F6, F9) | — | — | **Genuine gap.** API-observable, implemented. Its four contracts (cinder-volume-response, glance-image-delete, keystone-catalog-endpoints, neutron-network-list-filters) are plausibly exercised incidentally by `cinder/volume_lifecycle_test.go`/`cinder/update_test.go`, `glance/image_lifecycle_test.go`, `keystone/service_catalog_test.go`, and `neutron/network_lifecycle_test.go` **(inferred)** — but nothing cites SPEC-019. Candidate for #105. |
| SPEC-020 (`020-horizon-create-ui-enablement`) | Horizon compat — create/write UI enablement (F2, F3, F7, F8) | `nova/image_fixture_test.go` | FR-026 | Only the Nova image-fixture slice is cited. The Cinder/Neutron quota-and-extensions contracts are plausibly covered by `cinder/quotas_test.go`, `neutron/quotas_test.go`, `neutron/extensions_test.go`, `nova/limits_test.go` **(inferred)**. |
| SPEC-021 (`021-real-novnc-console`) | Horizon compat — a real console / noVNC (F1) | `nova/console_test.go` | FR-005, FR-025, FR-026 | Header notes the tests were rewritten with SPEC-021; one behaviour (serial-log fallback) is deliberately unchanged. |
| SPEC-022 (`022-project-tenant-isolation`) | Project/Tenant Lifecycle & Isolation Fixes | — | — | **Gap (soft).** `contracts/keystone-domain-resolution.md` requires "test/contract/keystone/ (existing, must stay green)" — a reverse pin on the uncited keystone suite, but no test cites SPEC-022. Candidate for #105. |
| SPEC-023 (`023-tenant-scoping-authz`) | Tenant Scoping & Authorization Model | `nova/rbac_test.go` | FR-008a | Reverse pin too: `contracts/usage-and-filters.md` mandates that `nova/tenant_usage_test.go` "MUST continue to pass unchanged" — that file cites nothing itself. `keystone/identity_links_test.go` mentions SPEC-023 only as runbook context, not as a pin. |
| SPEC-024 (`024-neutron-api-fidelity-realmode`) | Neutron API Fidelity & Real-Mode Networking | `neutron/error_envelope_test.go`, `neutron/network_lifecycle_test.go` | US2/B10: FR-010; F11: FR-035 | Only 2 of the spec's ~6 contract areas (error envelope, external networks) are pinned; port/DHCP, subnet listing, security-group shape, ownership are unpinned. |
| SPEC-025 (`025-nova-gaps-client-discovery`) | Nova Gaps & Client Discovery (#547) | `nova/version_document_test.go`, `keystone/identity_links_test.go`, `nova/boot_network_failure_test.go`, `nova/action_by_name_test.go`, `nova/server_addresses_test.go`, `nova/limits_usage_test.go`, `nova/deleted_action_history_test.go`, `nova/boot_from_new_volume_test.go`, `nova/selflink_absolute_test.go` | US1: FR-002; US2: FR-007/010/011/012/013; US3: FR-016a, FR-017; US5: FR-029/030/032/033; US6: FR-035, FR-036; US7: FR-040, FR-041, FR-043/043a/043b; US9: FR-049/050/054/055/056; plus FR-007 (selflinks) | Best-pinned spec in the repo (9 test files). Spec's `contracts/` dir names the same test areas — mapping verified in both directions. |
| SPEC-025 (`025-followup`) | SPEC-025 Follow-ups (pseudo-ticket) | — | — | No citation distinguishes it from its parent spec (see ambiguity note above). If a follow-up FR is pinned anywhere, it is silently counted under `025-nova-gaps-client-discovery`. |
| SPEC-027 (`027-database-pool-injection`) | Inject the DB pool through service constructors (ADR-001) | — | — | Internal architecture; verified by unit tests, not API-observable. |

---

## Table 2: Contract tests by service (issue #105 gap list)

97 `*_test.go` files under `test/contract/` (plus two non-test `helpers.go`).
Files with `—` are unspec'd coverage — candidates for #105 back-filling.

### Nova (35 files, 11 cited)

| Test file | Spec citation |
|---|---|
| `nova/action_by_name_test.go` | SPEC-025 (US5, FR-029/030/032/033) |
| `nova/advanced_server_actions_test.go` | — |
| `nova/aggregates_test.go` | — |
| `nova/availability_zones_test.go` | — |
| `nova/boot_from_new_volume_test.go` | SPEC-025 (US9, FR-049/050/054/055/056) |
| `nova/boot_network_failure_test.go` | SPEC-025 (US3, FR-016a/017) |
| `nova/console_test.go` | SPEC-021 (FR-005/025/026) |
| `nova/deleted_action_history_test.go` | SPEC-025 (US7, FR-043/043a/043b) |
| `nova/diagnostics_test.go` | — |
| `nova/flavor_actions_test.go` | — |
| `nova/flavor_extra_specs_keys_test.go` | — |
| `nova/flavor_extra_specs_test.go` | — |
| `nova/flavors_negative_test.go` | — |
| `nova/flavors_test.go` | — |
| `nova/hypervisors_test.go` | — |
| `nova/image_fixture_test.go` | SPEC-020 (FR-026) |
| `nova/instance_actions_test.go` | — |
| `nova/limits_test.go` | — |
| `nova/limits_usage_test.go` | SPEC-025 (US7, FR-040/041) |
| `nova/metadata_test.go` | — |
| `nova/migration_test.go` | — |
| `nova/migrations_list_test.go` | — |
| `nova/rbac_test.go` | SPEC-002 (T048, historical) + SPEC-023 (FR-008a) |
| `nova/security_group_actions_test.go` | — |
| `nova/selflink_absolute_test.go` | SPEC-025 (FR-007) |
| `nova/server_actions_test.go` | — |
| `nova/server_addresses_test.go` | SPEC-025 (US6, FR-035/036) |
| `nova/server_groups_test.go` | — |
| `nova/server_lifecycle_test.go` | — |
| `nova/server_tags_test.go` | — |
| `nova/server_update_test.go` | — |
| `nova/servergroups_test.go` | — |
| `nova/services_test.go` | — |
| `nova/tenant_usage_test.go` | — (reverse-pinned by SPEC-023 `contracts/usage-and-filters.md`: must keep passing) |
| `nova/version_document_test.go` | SPEC-025 (US1, FR-002) |

### Keystone (14 files, 3 cited)

| Test file | Spec citation |
|---|---|
| `keystone/application_credentials_test.go` | — |
| `keystone/credentials_test.go` | — |
| `keystone/domains_test.go` | — |
| `keystone/ec2_authz_test.go` | SPEC-014 (FR-005/007) |
| `keystone/ec2_credentials_test.go` | SPEC-014 (FR-005/007) |
| `keystone/groups_test.go` | — |
| `keystone/identity_links_test.go` | SPEC-025 (US2, FR-007/010/011/012/013) |
| `keystone/password_test.go` | — |
| `keystone/projects_test.go` | — |
| `keystone/role_assignments_test.go` | — |
| `keystone/roles_test.go` | — |
| `keystone/service_catalog_test.go` | — (plausibly exercises SPEC-019 `keystone-catalog-endpoints` **(inferred)**) |
| `keystone/services_test.go` | — |
| `keystone/users_test.go` | — (reverse-pinned as a class by SPEC-022 `keystone-domain-resolution.md`: "existing, must stay green") |

### Neutron (18 files, 2 cited)

| Test file | Spec citation |
|---|---|
| `neutron/address_scopes_test.go` | — |
| `neutron/auto_allocated_topology_test.go` | — |
| `neutron/availability_zones_test.go` | — |
| `neutron/error_envelope_test.go` | SPEC-024 (US2/B10, FR-010) |
| `neutron/extensions_test.go` | — (plausibly exercises SPEC-020 quota/extensions contracts **(inferred)**) |
| `neutron/l3_agents_test.go` | — |
| `neutron/metering_test.go` | — |
| `neutron/network_ip_availability_test.go` | — |
| `neutron/network_lifecycle_test.go` | SPEC-024 (F11, FR-035); plausibly also SPEC-019 network-list-filters **(inferred)** |
| `neutron/port_forwarding_test.go` | — |
| `neutron/qos_test.go` | — |
| `neutron/quotas_test.go` | — (plausibly exercises SPEC-020 `neutron-quotas-and-extensions` **(inferred)**) |
| `neutron/rbac_test.go` | — (topic-adjacent to SPEC-023, but no citation) |
| `neutron/routers_test.go` | — |
| `neutron/service_providers_test.go` | — |
| `neutron/subnet_pools_test.go` | — |
| `neutron/topology_test.go` | — |
| `neutron/trunks_test.go` | — |

### Cinder (18 files, 0 cited)

| Test file | Spec citation |
|---|---|
| `cinder/availability_zones_test.go` | — |
| `cinder/backups_test.go` | — |
| `cinder/groups_test.go` | — |
| `cinder/limits_test.go` | — |
| `cinder/manage_test.go` | — |
| `cinder/qos_specs_test.go` | — |
| `cinder/quotas_test.go` | — (plausibly exercises SPEC-020 `cinder-extensions-and-quota-sets` **(inferred)**) |
| `cinder/services_test.go` | — |
| `cinder/snapshot_metadata_test.go` | — |
| `cinder/snapshot_update_test.go` | — |
| `cinder/update_test.go` | — (plausibly exercises SPEC-019 `cinder-volume-response` **(inferred)**) |
| `cinder/volume_actions_advanced_test.go` | — |
| `cinder/volume_lifecycle_test.go` | — (plausibly exercises SPEC-019 `cinder-volume-response` **(inferred)**) |
| `cinder/volume_metadata_test.go` | — |
| `cinder/volume_transfers_test.go` | — |
| `cinder/volume_type_access_test.go` | — |
| `cinder/volume_type_extra_specs_test.go` | — |
| `cinder/volume_types_test.go` | — |

### Glance (7 files, 0 cited)

| Test file | Spec citation |
|---|---|
| `glance/cache_test.go` | — |
| `glance/image_data_test.go` | — |
| `glance/image_lifecycle_test.go` | — (plausibly exercises SPEC-019 `glance-image-delete` **(inferred)**) |
| `glance/import_test.go` | — |
| `glance/members_test.go` | — |
| `glance/metadefs_test.go` | — |
| `glance/tasks_test.go` | — |

### Top level (5 files, 4 cited)

| Test file | Spec citation |
|---|---|
| `server_readback_test.go` | SPEC-017 (finding F1) |
| `template_test.go` | — (documentation template, not a live contract) |
| `ws0_disk_test.go` | SPEC-009 (WS-0, FR-003/004) |
| `ws4_userdata_test.go` | SPEC-009 (WS-4) |
| `ws5_volume_test.go` | SPEC-009 (WS-5, FR-021/022) |

---

## Summary counts

- **20 of 97** contract test files cite a spec (verified by
  `grep -rlE 'SPEC-[0-9]' test/contract/ --include='*_test.go' | wc -l`).
  By service: Nova 11/35, Keystone 3/14, Neutron 2/18, top level 4/5,
  **Cinder 0/18, Glance 0/7**.
- **19 of 28** spec dirs have no pinning contract tests:
  000, 001, 003, 004, 005, 006, 007, 008, 010, 011, 012, 013, 015, 016, 018,
  019, 022, 025-followup, 027.
  Of those, absence is **expected** for:
  - future services: **003, 004, 005** (Barbican, Designate, Octavia — not implemented);
  - non-API-observable / infra specs: **001, 006, 007, 008, 010, 011, 012, 013, 027**;
  - verification campaigns rather than behavior specs: **016, 018**;
  - the meta-spec **000**, whose deliverable is the suite itself.
- **Genuine gaps** (implemented, API-observable specs with no pinning contract
  test — the #105 back-fill list on the spec side):
  - **SPEC-015** (identity not-found shape) — verified once by hand, never pinned.
  - **SPEC-019** (Horizon-compat Cinder/Neutron/Glance/Keystone fixes) — likely
    incidentally covered, never cited.
  - **SPEC-022** (project/tenant isolation) — reverse-pinned only ("existing
    tests must stay green"), never cited.
  - **SPEC-025-followup** — indistinguishable from its parent spec in citations.
- **Unspec'd test clusters** (the #105 back-fill list on the test side):
  all of **Cinder (18 files)** and **Glance (7 files)**, 16 of 18 Neutron files,
  11 of 14 Keystone files, and 24 of 35 Nova files carry no spec citation.

---

## Maintenance

This index is generated from test-source evidence. To refresh it:

```sh
# All spec citations with file:line (Table 1 / citation column source):
grep -rnoE 'SPEC-[0-9]{3}' test/contract/ --include='*.go' | sort | uniq -c | sort -rn

# Requirement-level citations (FR coverage column):
grep -rnoE 'FR-[0-9]+[a-z]?' test/contract/ --include='*.go' | sort

# Count of test files citing a spec vs total (Summary counts):
grep -rlE 'SPEC-[0-9]' test/contract/ --include='*_test.go' | wc -l
find test/contract -name '*_test.go' | wc -l

# Per-service breakdown:
for s in cinder glance keystone neutron nova; do
  echo "$s: $(ls test/contract/$s/*_test.go | wc -l) test files, $(grep -rlE 'SPEC-[0-9]' test/contract/$s/*_test.go 2>/dev/null | wc -l) cited"
done

# Reverse direction — which spec contracts reference test files:
grep -rn 'test/contract' docs/specs/0*/contracts/*.md
```

Guidance:

- **New contract tests must cite the spec they pin** in their header comment,
  in the form `// SPEC-NNN FR-xxx` (see `nova/console_test.go` or
  `keystone/ec2_credentials_test.go` for the pattern). A test without a
  citation is invisible to this index and counts as unspec'd coverage.
- When adding the citation, update Table 1 (spec row), Table 2 (file row), and
  the summary counts in the same change.
- Beware the duplicate **SPEC-025** number: cite follow-up work as
  `SPEC-025-followup FR-xxx` (or reference its source finding) so it can be
  distinguished from `025-nova-gaps-client-discovery`.
- Inferred mappings (marked **(inferred)** above) should be replaced with real
  citations as they are confirmed — do not add new inferred rows.
