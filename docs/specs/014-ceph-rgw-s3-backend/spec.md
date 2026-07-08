# Feature Specification: Ceph RGW (S3) — Glance backend & tenant object storage

**Feature Branch**: `feature/480-o3k-validate-ceph-rgw-s3-backend-glance-images`
**Created**: 2026-07-07
**Status**: Draft
**Input**: cloud-storage#480 "o3k — Validate Ceph RGW (S3) backend (Glance images)" (title + body + comments), widened per the scope proposal to cover both the Glance image backend and tenant-facing object storage.

## Overview

o3k must work with **Ceph RGW** (the RADOS Gateway's S3-compatible object storage) in two ways:

- **A — Glance image backend:** o3k stores and serves VM image data in Ceph RGW. The image **data round-trip** already exists; this feature proves it end-to-end against a real RGW, documents the RGW-specific quirks, locks it in with an automated test, **and implements the pure-S3 boot path** (fetch the image to local + normalize to a raw base) so VMs boot from RGW-backed images.
- **B — Tenant-facing object storage (new):** a tenant (for example a workload inside a VM) can read and write objects in Ceph RGW buckets using credentials issued by o3k's identity service, with RGW trusting o3k as the authentication authority.

Both share one RGW fixture and one validation harness, which is why they're delivered together.

## Clarifications

### Session 2026-07-08

- Q: Is booting a VM from an RGW-backed image a gating acceptance criterion for capability A, or is A scoped to image data round-trip only? → A: **Gating.** Capability A must make VM boot work end-to-end from a pure-S3 image, including the required code (fetch the image to local + normalize it to a raw base). Capability A is therefore "validate + implement," not validate-only.
- Q: What kind of S3 credential does o3k issue to tenants (long-lived keys vs temporary/STS)? → A: **Long-lived**, project-scoped, explicitly **revocable** access/secret keys (OpenStack EC2-credentials style). Temporary/STS tokens are out of scope for this feature; credential expiry is optional, not required.
- Q: How should image/object transfer size relate to the operation timeout? → A: **No total-duration cap.** The fail-fast timeout guards connectivity/first-byte only; transfers stream to completion regardless of size. Validate up to ~10 GB.
- Q: How is an S3-stored image materialized as the local raw base Nova boots from (Nova has no boot-time download today; pure-S3 keeps no local copy)? → A: **Lazy fetch on boot.** When the local raw base is missing, the compute service fetches the image from RGW via the image store and normalizes it to raw before staging the overlay. This makes **pure-S3** Glance bootable and works **multi-node** (any node materializes the image from the shared bucket on demand).
- Q: Do tenants get per-project bucket namespaces or a shared global namespace? → A: **Per-project namespaces** (the OpenStack/Swift `AUTH_<project>` account model, implemented in RGW via `keystone implicit tenants`, scoped to S3). Each project has a private bucket namespace; bucket names may repeat across projects; cross-tenant access requires explicit tenant-qualified addressing and is denied without permission.
- Q: Must o3k support HTTPS RGW endpoints with an internal/custom CA, or only public CAs / HTTP? → A: **Support HTTPS + a configurable custom/internal CA bundle.** Plain HTTP stays supported for dev/CI fixtures; an insecure-skip-TLS-verify option may exist for testing only and is never the default.
- Q: Is capability B gated on in-guest-VM access, or on the credential/authentication chain? → A: **Gated on the auth chain**, validated with a direct S3 client (issue → sign → RGW-validates-via-o3k → put/get + isolation + revocation). Actual in-guest-VM access, and the tenant-network / cross-cluster routing to RGW it needs, are **manual/deployment-parity** setup (wired by hand or by the test setup), not CI-gating.
- Q: Should lazily-fetched node-local image bases be cleaned up, or is unbounded cache growth acceptable? → A: **Reclaim unused bases.** o3k MUST bound node-local image-cache growth by reclaiming cached raw bases no longer referenced by any instance on that node. The reclamation policy (LRU / size-cap / reference-count) is a plan detail.
- Q: Must a lazily-fetched image base be integrity-checked before boot? → A: **Yes.** Verify the fetched data against the image's known checksum before use; a mismatch fails the boot (instance `ERROR`) and never boots a corrupt/partial image.
- Q: What service type does the catalog advertise for S3 object storage, given no Swift API? → A: The **`s3`** service type (not the Swift `object-store` type), plus the endpoint documented for direct S3-client configuration — so a Swift API is not falsely advertised.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator runs Glance on Ceph RGW (Priority: P1)

An operator deploys o3k with an existing Ceph RGW cluster and configures the image service to keep image data in an RGW bucket over the S3-compatible endpoint. Uploading and downloading images works exactly as it does with local storage, and booting a VM from an RGW-backed image works.

**Why this priority**: This is the original ticket ask and the minimum viable outcome — o3k images living in Ceph RGW. Everything else builds on the same fixture.

**Independent Test**: Configure the image service for S3 storage against a real RGW endpoint + bucket, upload an image, download it, and confirm the downloaded bytes match the source. Delivers a working, verifiable RGW image backend on its own.

**Acceptance Scenarios**:

1. **Given** o3k configured to store images in Ceph RGW with valid credentials and an existing bucket, **When** an operator uploads an image, **Then** the image data is stored in the RGW bucket and the image becomes usable.
2. **Given** an image stored in RGW, **When** it is downloaded, **Then** the retrieved content is byte-for-byte identical to what was uploaded (checksum match).
3. **Given** an image stored in RGW, **When** a VM is booted from it, **Then** the VM starts successfully.
4. **Given** the RGW endpoint, bucket, or credentials are wrong or unreachable, **When** an image operation is attempted, **Then** o3k fails promptly with a clear error rather than silently succeeding or hanging.

---

### User Story 2 - Tenant stores objects in Ceph RGW via o3k identity (Priority: P2)

A tenant obtains S3 credentials scoped to their project from o3k, then uses any standard S3 client (including from inside a VM) to create buckets and read/write objects in Ceph RGW. RGW validates those requests against o3k, so no separate, out-of-band object-storage account is needed.

**Why this priority**: This is the new capability that turns "RGW is o3k's internal image store" into "RGW is object storage o3k tenants can use." It is net-new work and depends on the same fixture as P1.

**Independent Test**: Obtain project-scoped S3 credentials from o3k, point an S3 client at the RGW endpoint, and perform bucket create + object put/get. Confirm invalid credentials are rejected and one project cannot reach another project's objects.

**Acceptance Scenarios**:

1. **Given** an authenticated tenant, **When** they request S3 credentials from o3k, **Then** they receive an access key / secret scoped to their project.
2. **Given** valid o3k-issued S3 credentials, **When** the tenant performs object read/write against RGW, **Then** the operations succeed.
3. **Given** invalid or revoked credentials, **When** a request is made to RGW, **Then** it is rejected.
4. **Given** two tenants in different projects, **When** one attempts to access the other's bucket/objects, **Then** access is denied.

---

### User Story 3 - Validated, documented, reproducible RGW integration (Priority: P3)

A maintainer can trust the RGW integration because it is exercised automatically in CI against a real RGW, the RGW-specific configuration and quirks are documented, and there is a one-command way to stand up a real (Rook/Ceph) RGW for manual, deployment-parity validation.

**Why this priority**: Turns a one-off manual check into a durable guarantee. Required by the ticket's acceptance criteria (documentation + CI test) and protects both P1 and P2 from regressions.

**Independent Test**: Run the CI pipeline and confirm the RGW integration test executes and gates. Follow the documented runbook to bring up the real RGW fixture and reproduce the round-trip.

**Acceptance Scenarios**:

1. **Given** a change to o3k, **When** CI runs, **Then** an integration test exercises image read/write (and tenant object read/write) against a real RGW and reports pass/fail.
2. **Given** the documentation, **When** an operator follows it, **Then** they can configure and use the RGW backend without reading the source.
3. **Given** the runbook, **When** an engineer follows it, **Then** they can stand up a non-degraded real RGW and reproduce the validation.

### Edge Cases

- **Bucket does not exist**: o3k does not create buckets; the operator/tenant must pre-create them. A missing bucket must produce a clear error.
- **Large images**: image transfers MUST NOT be capped by a fixed total-duration timeout — the fail-fast timeout guards connectivity/first-byte only, and transfers stream to completion (validated up to ~10 GB).
- **Endpoint quirks**: the integration must work with RGW's addressing/region/signature expectations (path-style addressing, a required region, S3 signature validation) and with both plain-HTTP and HTTPS endpoints — including HTTPS backed by an internal/custom CA (via a configurable CA bundle).
- **Credential lifecycle**: revoking a tenant's credentials must stop further access.
- **Cross-tenant isolation**: a tenant must never read or write another project's objects.
- **RGW/identity trust misconfiguration**: if RGW cannot validate against o3k, tenant requests must be denied, not silently allowed.
- **Non-raw image format for boot**: an image stored in RGW in a non-raw format (e.g., qcow2) must still produce a bootable VM — o3k normalizes/prepares it for boot as it does for the local backend.
- **Missing local base on boot**: if the compute node has no local raw base, it is fetched from RGW on demand and **verified against the image checksum** before boot; a fetch, checksum, or normalize failure must fail the boot clearly (instance `ERROR`), not hang or silently boot a corrupt/empty disk.
- **Image-cache growth**: node-local bases fetched for boot are reclaimed once no instance on the node references them, so a node's image cache does not grow without bound.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: o3k MUST store and retrieve VM image data in a Ceph RGW bucket via its S3-compatible endpoint when configured to use S3 image storage.
- **FR-002**: An image uploaded through o3k MUST be retrievable with byte-for-byte identical content (verified by checksum).
- **FR-003**: o3k MUST let an operator point the image backend at a specified RGW endpoint and bucket, using operator-supplied S3 credentials.
- **FR-004**: When the configured RGW endpoint, bucket, or credentials are invalid or unreachable, o3k MUST surface a prompt, clear failure rather than silently mis-reporting success (e.g., treating a real error as "object not found").
- **FR-005**: A tenant MUST be able to obtain **long-lived, project-scoped** S3 credentials (access key + secret, OpenStack EC2-credentials style) from o3k's identity service, and MUST be able to revoke them.
- **FR-006**: o3k's identity service MUST be able to authoritatively validate S3-signed requests, so that Ceph RGW configured to trust o3k accepts requests bearing o3k-issued credentials.
- **FR-007**: A tenant presenting valid o3k-issued credentials MUST be able to create buckets and read/write objects in RGW; invalid or revoked credentials MUST be rejected. (Credentials are long-lived; expiry is optional and not required for this feature.)
- **FR-008**: Object access MUST be isolated by project using **per-project bucket namespaces** (the OpenStack/Swift account model): each project has a private namespace, bucket names MAY repeat across projects, and a tenant MUST NOT read or write another project's buckets or objects (cross-tenant access requires explicit tenant-qualified addressing and MUST be denied without permission).
- **FR-009**: o3k's service catalog MUST advertise the RGW object-storage endpoint under the **`s3`** service type (NOT the Swift `object-store` type, since o3k exposes no Swift API), and the S3 endpoint MUST also be documented for direct client configuration (S3 clients take an explicit endpoint + credentials rather than reading the catalog).
- **FR-010**: The RGW integration (Glance backend read/write, and tenant object read/write) MUST be covered by an automated integration test that runs in CI against a real RGW.
- **FR-011**: RGW configuration, credential handling, and known S3/RGW quirks (path-style addressing, required region/signature, bucket pre-creation, TLS handling, operation timeout) MUST be documented under `docs/`.
- **FR-012**: A reproducible, single-command way to stand up a real, non-degraded Ceph RGW (via Rook) for manual/deployment-parity validation MUST be provided and documented.
- **FR-013**: When a VM is booted from an image stored in Ceph RGW (S3 mode) and the local raw base is absent, the compute service MUST **lazily fetch** the image from RGW and normalize it to the required raw base before boot. This MUST work for **pure-S3** Glance and on **any compute node** (each node materializes the image from the shared bucket on demand), giving parity with the local backend.
- **FR-014**: Image and object transfers MUST NOT be bounded by a fixed total-duration timeout; the fail-fast timeout MUST apply only to connectivity/first-byte. Transfers MUST stream to completion for images up to at least ~10 GB.
- **FR-015**: o3k MUST connect to HTTPS RGW endpoints and MUST support trusting a configurable custom/internal CA bundle for that endpoint. Plain HTTP MUST remain supported for dev/CI fixtures. An insecure-skip-TLS-verify option MAY be provided for testing only and MUST NOT be the default.
- **FR-016**: Node-local image bases fetched for boot MUST NOT accumulate unbounded — o3k MUST reclaim cached raw bases no longer referenced by any instance on that node, keeping compute-node disk usage bounded. (The reclamation policy is a plan/implementation detail.)
- **FR-017**: When an image is lazily fetched from RGW for boot, o3k MUST verify the fetched data against the image's known checksum before a VM uses it; a mismatch MUST fail the boot (instance `ERROR`) rather than boot a corrupt or partial image.

### Key Entities *(include if feature involves data)*

- **Image**: a VM image managed by o3k; its data is stored as an object in Ceph RGW; identified and integrity-checked by checksum.
- **Bucket**: an RGW container for objects, living in a **per-project namespace** (bucket names may repeat across projects); pre-created by the operator (image backend) or tenant (object storage); the isolation unit for access control.
- **Object**: arbitrary tenant data stored in a bucket.
- **S3 credential**: a **long-lived, revocable** access key / secret pair issued by o3k's identity service (OpenStack EC2-credentials style), scoped to a user + project, used to sign S3 requests to RGW.
- **Project (tenant)**: the ownership and isolation boundary for images, buckets, objects, and credentials.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Following only the documentation, an operator can configure the image service against a real RGW and complete an image upload→download with identical content.
- **SC-002**: 100% of automated image round-trips preserve content integrity (checksum match) for images up to at least ~10 GB; 0 corrupted, truncated, or timed-out transfers.
- **SC-003**: A tenant can obtain credentials and complete an object read/write against RGW with no operator action beyond the initial trust setup.
- **SC-004**: 0 successful cross-project accesses in isolation tests (one tenant cannot reach another's objects).
- **SC-005**: 0 successful accesses using invalid or revoked credentials.
- **SC-006**: The integration test runs on every change in CI and visibly gates merges (pass/fail).
- **SC-007**: A new engineer can stand up the real RGW fixture and reproduce the end-to-end round-trip in under 30 minutes using the runbook.
- **SC-008**: Ceph in the manual fixture reports a healthy (non-degraded) state during validation.
- **SC-009**: A VM boots successfully from an image stored in Ceph RGW (S3 mode) in 100% of boot attempts during validation.
- **SC-010**: A VM boots from an RGW-backed image on a compute node that holds no local copy — the node fetches and normalizes it on demand (demonstrates pure-S3 / multi-node boot).
- **SC-011**: A compute node's local image cache stays bounded — bases unreferenced by any instance are reclaimed, so repeated boot/delete cycles do not grow node disk usage without bound.
- **SC-012**: A corrupt or partial lazily-fetched base is detected via checksum and fails the boot; 0 VMs boot from a corrupt base.

## Assumptions

- **Protocol for tenant access is S3.** Tenant object storage is delivered via the S3-compatible interface. A Swift API and the Horizon Swift UI are **out of scope** and tracked separately (o3k#15).
- **o3k does not operate Ceph.** The RGW/Ceph cluster is provided externally (standalone, Rook, or sci-in-a-box). o3k does not deploy, manage, or heal Ceph, and does not auto-create buckets.
- **Image-backend credentials are supplied out-of-band** (via o3k's environment), consistent with current behavior; there is no new configuration field required for them.
- **Widened scope is assumed approved.** Capability B (tenant object storage) was proposed to the reviewer (@senolcolak). If it is declined, B is deferred to o3k#15 and this feature reduces to capability A (User Story 1 + the P3 validation for A only).
- **CI validates against a lightweight real RGW** (a containerized all-in-one Ceph/RGW), not a Kubernetes/Rook stack, which is impractical on CI runners. The Rook two-cluster setup is the manual deployment-parity path (User Story 3 / FR-012). In CI the tenant S3 client is exercised **directly** — capability B is gated on the credential/authentication chain, not on a guest VM. The in-guest-VM access and the tenant-network / cross-cluster routing to RGW it requires are environmental setup (wired manually or by the test setup) and are validated manually.
- **RGW↔o3k trust and network wireup are part of the reproducible setup.** Configuring RGW to trust o3k (implicit tenants, identity URL, admin credentials) and routing clients to the RGW endpoint are covered by the fixture script + o3k Helm chart + documented steps (FR-011/FR-012), wired manually or by the test setup.
- **Credential issuance uses the standard OpenStack EC2-credentials interface** (`openstack ec2 credentials create` / the equivalent Identity API), consistent with o3k's CLI/Terraform compatibility goal.
- **Glance image data lives in a single operator-owned bucket**; image isolation is enforced by o3k's Glance API (project scoping), not by the object-storage layer. (Tenant object storage — capability B — is the per-project-namespaced path.)

## Out of Scope

- A Swift object-storage API and the Horizon Swift UI (tracked in o3k#15).
- The Ceph RBD (block) backend for Cinder (sibling ticket, cloud-storage#479).
- Deploying, upgrading, or operating the Ceph cluster itself.
- A Kubernetes **operator** that fully automates end-to-end provisioning of Ceph/RGW + o3k + their wireup. Reproducible setup for this feature is the fixture script + o3k Helm chart + documented wireup (FR-012); a turnkey operator is a separate, future effort.
- Managing tenant object-storage **quotas** — rely on RGW-native quotas; o3k does not manage them in this feature.

## Dependencies

- An external Ceph cluster with RGW reachable over an S3 endpoint (standalone/Rook for manual validation; a containerized RGW for CI).
- o3k's identity service acting as the authentication authority that RGW trusts (for capability B).
