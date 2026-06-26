# Feature Specification: Ensure Node-Local Storage Directories Exist in Real Mode

**Feature Branch**: `bugfix/510-real-mode-helm-deploy-node-local-storage-subdirs-not-pre-created` (feature id `011-ensure-storage-subdirs`)
**Created**: 2026-06-25
**Status**: Draft
**Source**: [cobaltcore-dev/cloud-storage#510](https://github.com/cobaltcore-dev/cloud-storage/issues/510) — "[BUG] - Real-mode Helm deploy: node-local storage subdirs not pre-created"
**Input**: User description: "Real-mode Helm deploy: node-local storage subdirs not pre-created"

## Overview

On a fresh real-mode deployment, the platform's node-local storage area is created as a
single base directory but its per-service subdirectories (for images, volumes, instances,
config-drive, and DHCP state) are not. The image service is the lone service that, finding
its configured directory absent, silently relocates its data to an ephemeral, pod-local
fallback location instead of creating the configured directory. The upload still reports
success, but the compute service later looks for the image's backing data at the configured
node-local location, does not find it, and the virtual machine fails to boot.

The result: every fresh real-mode deployment (and every redeploy after the node-local data
has been removed) cannot boot a virtual machine until an operator manually creates the
missing directories — a failure that is silent and misleading because image upload reports
success.

## Clarifications

### Session 2026-06-25

- Q: When the canonical local image dir is absent at startup, what happens to the legacy `~/.o3k/images` fallback? → A: Remove it entirely — the image service always resolves to and creates the configured node-local dir in every mode (matching the other local-storage services); development/non-Linux use relies on stub mode.
- Q: What automated regression coverage must the spec mandate to pin this failure mode so it cannot silently reappear? → A: All three tiers — a CI-gating unit test (image service resolves/creates canonical `/var/lib/o3k/images`, never a home-relative path), a CI-gating chart-render test (real-mode pre-creates the subdirs; stub omits them), and a host end-to-end test (deploy → image → VM running); each demonstrated RED on pre-fix behavior, GREEN after.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Boot a VM on a fresh real-mode deployment with no manual setup (Priority: P1)

An operator performs a first-time real-mode deployment on a clean node, uploads an image,
and creates a virtual machine. The VM reaches a running state without the operator having to
manually create any storage directories beforehand.

**Why this priority**: This is the core defect. Today this flow fails (VM goes to an error
state) on every fresh deployment, which blocks real-mode end-to-end use out of the box. Fixing
it restores the product's primary value proposition for production-style deployments.

**Independent Test**: Perform a real-mode deployment on a node whose node-local storage area
does not yet exist, upload an image, create a VM, and confirm it reaches a running state — with
no manual directory creation at any step.

**Acceptance Scenarios**:

1. **Given** a clean node with no pre-existing node-local storage area, **When** the operator
   deploys in real mode, uploads an image, and creates a VM, **Then** the VM reaches a running
   (active) state without any manual directory creation.
2. **Given** the same fresh deployment, **When** the operator inspects the node-local storage
   area after the platform has started, **Then** all required per-service subdirectories
   (images, volumes, instances, config-drive, DHCP state) exist on the persistent node-local
   location.
3. **Given** a fresh real-mode deployment, **When** an image is uploaded and then a VM that
   uses it is created, **Then** the compute service finds the image's backing data at the same
   node-local location the image service wrote it to (no divergence).

---

### User Story 2 - Image storage never silently relocates to an ephemeral location (Priority: P1)

When the image service's configured node-local storage directory does not yet exist, it creates
that directory and uses it, rather than silently falling back to a pod-local, ephemeral
directory that other services do not read from.

**Why this priority**: This is the root cause of the silent failure and applies beyond the
chart-based deployment (e.g. a bare-binary or service-managed deployment where the directory is
also absent on first run). Resolving it makes the platform correct regardless of how the
node-local area was provisioned, and removes the "success but wrong place" trap.

**Independent Test**: Start the image service in real mode against a node-local base path that
exists but has no image subdirectory, upload an image, and confirm the data is written under the
configured node-local image directory — not under any ephemeral/home-relative fallback path.

**Acceptance Scenarios**:

1. **Given** the configured node-local image directory is absent at startup, **When** the image
   service initializes its local store, **Then** it creates the configured directory and uses it.
2. **Given** an image has been uploaded, **When** the storage location is inspected, **Then** the
   image's data resides at the configured node-local image location and not at any ephemeral,
   home-relative fallback location.
3. **Given** the image service cannot create or use the configured node-local directory (e.g.
   insufficient permission), **When** initialization runs, **Then** the condition is surfaced
   (visible in logs / startup) rather than silently masked by relocating data elsewhere.

---

### User Story 3 - Redeploy after teardown still boots a VM on the first try (Priority: P2)

After a teardown that removes the node-local storage trees, the operator deploys again and boots
a VM on the first attempt, with no manual remediation.

**Why this priority**: Real-mode deployments are routinely torn down and recreated (the teardown
capability removes the node-local data by design). The directory-creation guarantee must be
idempotent so the second and subsequent deployments behave like the first. Important for an
iterative workflow, but secondary to making the first deployment work at all.

**Independent Test**: Deploy real mode, remove the node-local storage trees (as a teardown would),
deploy again, and confirm a VM boots to a running state on the first attempt with no manual steps.

**Acceptance Scenarios**:

1. **Given** a prior real-mode deployment whose node-local storage trees were removed, **When**
   the operator deploys again, **Then** the required subdirectories are recreated automatically.
2. **Given** that redeployment, **When** the operator uploads an image and creates a VM, **Then**
   the VM reaches a running state without manual directory creation.

---

### Edge Cases

- **Partial pre-existence**: some required subdirectories already exist and others do not. The
  platform must create the missing ones and leave existing ones (and their contents) intact.
- **Silent-success trap**: an image upload reports success while the backing data is not at the
  location compute reads from. This divergence must not occur; the write location and the read
  location must be the same.
- **Restrictive base-directory permissions**: when the node-local base directory is auto-created
  with restrictive ownership/permissions, the platform must still be able to create and use the
  required subdirectories (or surface a clear error if it cannot).
- **Non-chart deployment**: a deployment that does not use the chart (e.g. binary run directly,
  or under a service manager) where the node-local base path is also absent on first run.
- **Stub / non-real mode**: deployments that do not use node-local storage must be unaffected and
  must not require any node-local directories to exist.
- **Persistence across pod/process restart**: the created directories and their contents must live
  on the persistent node-local location and survive a restart, not on ephemeral per-pod storage.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: On a real-mode deployment, every node-local storage subdirectory required by the
  real-mode services — images, volumes, instances, config-drive (cloud-init), and DHCP state —
  MUST exist before the platform begins serving requests.
- **FR-002**: The image service MUST create and use its configured node-local image directory when
  that directory is absent at startup, and MUST NOT silently relocate its data to an ephemeral,
  pod-local, or home-relative fallback directory.
- **FR-003**: The location where the image service stores a local image and the location where the
  compute service reads that image's backing data MUST be identical (no divergence between write
  and read paths).
- **FR-004**: A virtual machine created immediately after an image upload, on a node where the
  node-local storage area did not previously exist, MUST be able to locate its backing image and
  reach a running state.
- **FR-005**: Directory creation MUST be idempotent: it MUST succeed whether none, some, or all of
  the required subdirectories already exist, and MUST NOT remove or alter the contents of
  directories that already exist.
- **FR-006**: All real-mode services that use node-local storage MUST handle their storage
  directories consistently (create the configured directory if absent); no service may be an
  outlier that silently uses a different location.
- **FR-007**: The required directories MUST be created on the persistent node-local storage
  location so their contents survive a pod/process restart.
- **FR-008**: If the platform cannot create or use a required node-local directory, the condition
  MUST be surfaced (e.g. in logs or at startup) rather than masked by writing data to an
  alternate, unread location.
- **FR-009**: Deployments that do not use node-local real-mode storage (e.g. stub mode) MUST be
  unaffected and MUST NOT require any node-local directories to exist.
- **FR-010**: No deployment runbook or operator procedure may require manually creating the
  node-local storage subdirectories as a prerequisite for booting a virtual machine.
- **FR-011**: The fix MUST be locked by committed automated regression tests at three tiers, each
  demonstrated to fail against the pre-fix behavior and to pass after the fix (RED→GREEN):
  (a) a CI-gating **unit test** asserting the image service resolves and creates the canonical
  node-local image directory (`/var/lib/o3k/images`) and never a home-relative / `~/.o3k` path, in
  any mode; (b) a CI-gating **chart-render test** asserting a real-mode deployment pre-creates all
  required subdirectories on the node-local mount and a stub deployment does not; (c) a **host
  end-to-end test** (fresh real-mode deploy → image upload → VM reaches running state) in the
  real-mode validation harness. Tiers (a) and (b) run on every build and are the guard that
  prevents silent recurrence; tier (c) provides full-stack confirmation.

### Key Entities *(include if feature involves data)*

- **Node-local storage area**: the per-node base directory under which real-mode services keep
  their on-disk state; auto-created today but only as the bare base directory.
- **Per-service subdirectory**: a directory under the node-local storage area dedicated to one
  kind of state — images (VM base images), volumes (block storage), instances (per-VM disks),
  config-drive (cloud-init/metadata media), and DHCP state.
- **Image backing data**: the local file an uploaded image is stored as, which the compute service
  reads when creating a VM that boots from that image.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a fresh real-mode deployment, an operator can boot a virtual machine to a running
  state on the first attempt with zero manual filesystem preparation.
- **SC-002**: 100% of images uploaded in real-mode local storage are readable by the compute
  service at the location it uses to boot — no upload results in image data placed where compute
  does not look.
- **SC-003**: After a teardown that removes the node-local storage trees, the next deployment boots
  a virtual machine on the first attempt with no manual remediation.
- **SC-004**: The documented manual workaround (operator pre-creating the storage subdirectories
  before deploying) is no longer required and is removed from deployment/runbook documentation.
- **SC-005**: First-successful-VM on a fresh deployment requires zero manual remediation steps
  (previously: create directories, restart the platform, re-upload the image, and recreate the VM).
- **SC-006**: Stub-mode and other non-real-mode deployments continue to deploy and pass their
  existing checks unchanged.
- **SC-007**: A regression in image-storage path resolution is caught automatically before merge:
  the CI-gating unit test fails if the image service resolves local image storage to any
  non-canonical or home-relative path, and the CI-gating chart-render test fails if a real-mode
  render omits subdirectory pre-creation — both verified RED on the pre-fix code and GREEN after.

## Assumptions

- The target context is real-mode deployment on a single node (per the single-node real-mode
  feature); multi-node deployments apply the same per-node node-local layout on each node.
- The primary trigger is local image storage mode; the directory-creation guarantee also covers
  the volumes, instances, config-drive, and DHCP directories used by the other real-mode services.
- The node-local base path is the platform's standard location and may be configured by the
  deployment; requirements are expressed relative to that configured base.
- In real mode the platform runs with sufficient privileges to create directories on the
  node-local location (consistent with how it already writes there at runtime).
- The legacy home-relative fallback storage location (`~/.o3k`) is removed entirely (see
  Clarifications 2026-06-25): the image service always resolves to and creates the configured
  node-local directory, in every mode, matching how the other local-storage services already
  behave. Development and non-Linux use rely on stub mode, which does not touch node-local storage.

## Dependencies

- Builds on the single-node real-mode capability (per-instance disk overlay over a base image,
  config-drive, DHCP) and the deployment chart that provisions the node-local storage mount.
- Related to, but independent of, the teardown capability that removes node-local data: teardown
  removes the trees; this feature guarantees they are recreated on the next deployment.

## Out of Scope

- Changing which storage backend is selected, or the behavior of non-local backends beyond their
  own directory creation.
- Redesigning node-local storage into shared/network or claim-based persistent storage.
- The teardown capability itself.
- Multi-node shared image distribution or caching.
