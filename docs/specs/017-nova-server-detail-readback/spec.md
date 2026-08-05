# Feature Specification: Nova server-detail read-back (Terraform compatibility follow-ups)

**Feature Branch**: `feature/524-…` (feature id: `017-nova-server-detail-readback`)
**Created**: 2026-07-17
**Status**: Draft
**Input**: cobaltcore-dev/cloud-storage#524 — "o3k — Terraform compatibility follow-ups: Nova server-detail read-back + real-mode validation" (follow-up from #481, Terraform provider compatibility verified PASS)

## Clarifications

### Session 2026-07-17

- Q: What happens when a security-group name supplied at instance create doesn't exist in the project? → A: Reject the create with a Nova-compatible client error (HTTP 400, "security group not found"); fail-fast, no instance created. Verified against real Nova (create-server validation → `400 badRequest`, synchronous fault) and the Terraform `openstack_compute_instance_v2` provider (surfaces the 400, `apply` fails; no silent create).
- Q: Is the optional Terraform CI gate (US3) in scope this iteration? → A: No — regression is guarded by the core Go unit tests (assert `key_name` + `security_groups` on the single-instance and list-detail responses; run in the existing CI unit-tests job). The dedicated end-to-end terraform-in-CI smoke is deferred as an optional follow-up.
- Q: How should `key_name` be represented in server-detail when the instance has no keypair? → A: `"key_name": null` (field always present, JSON null). Verified against the real Nova api-ref (Show and List Server Details return `"key_name": null`); the Terraform provider/CLI read null as "no key pair" → no drift.
- Q: What happens when a `key_name` supplied at instance create doesn't resolve to a keypair owned by the requesting user? → A: Reject synchronously with a Nova-compatible HTTP 400 (keypair not found); no instance created — mirrors FR-005a and real Nova v2.1 strict input validation (today Lightstack accepts silently and boots keyless via an async lookup). Verified against the Nova api-ref.
- Q: Should the per-project `default` security-group gap (an explicit `security_groups=["default"]` would 400 in a non-seeded project) be folded into scope? → A: Yes — it is OpenStack-conformant. The Neutron admin docs state "Each project has a default security group … automatically applied to instances and ports unless otherwise specified." Add an idempotent ensure-default at the create path (FR-006a); genuinely-unknown *non-default* names still 400. Verified against Neutron docs.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Keypair survives a repeated apply (Priority: P1)

An operator uses the standard OpenStack Terraform provider to create a compute instance
with a key pair (SSH keypair name). After the first `terraform apply` succeeds, they run
`terraform plan`/`apply` again on the unchanged configuration and expect it to be a no-op.
Today it is not: the instance detail response does not report the key pair that was used,
so the provider sees the `key_pair` attribute as having "disappeared" and proposes to
**destroy and recreate** the instance.

**Why this priority**: Spurious instance replacement on an unchanged apply is the most
damaging kind of compatibility break — it silently destroys running workloads on a
routine re-run. This is the primary defect the ticket exists to fix.

**Independent Test**: Create an instance via Terraform (or `openstack server create
--key-name …`), then read the instance back (`openstack server show`) and run
`terraform plan`. The key name used at boot appears in the detail response and the plan
proposes no changes to `key_pair`.

**Acceptance Scenarios**:

1. **Given** an instance created with a named key pair, **When** its detail is read back, **Then** the response reports that same key pair name.
2. **Given** a successful `terraform apply` of a configuration that sets a key pair, **When** `terraform plan` is run again unchanged, **Then** the plan proposes no replacement caused by the key pair.
3. **Given** an instance created without any key pair, **When** its detail is read back, **Then** the `key_name` field is present with a `null` value (matching upstream Nova), which the provider treats as "no key pair" (no drift).

---

### User Story 2 - Security groups survive a repeated apply (Priority: P1)

The same operator sets one or more security groups on the instance in their Terraform
configuration. After a successful apply, a repeated `plan`/`apply` must be a no-op.
Today it is not: the instance detail response reports **no** security groups, so the
provider sees the configured security groups as removed and proposes to **destroy and
recreate** the instance. (Separately, security groups supplied when the instance is
created are not currently retained at all.)

**Why this priority**: Same blast radius as User Story 1 — an unchanged re-apply must not
replace the instance. Both fields are named together in the ticket as the cause of the
churn; either one alone still triggers replacement, so both are P1.

**Independent Test**: Create an instance via Terraform (or `openstack server create
--security-group …`) with a security group, read it back, and run `terraform plan`. The
detail response lists the security group(s) the instance was created with and the plan
proposes no changes to `security_groups`.

**Acceptance Scenarios**:

1. **Given** an instance created with one or more named security groups, **When** its detail is read back, **Then** the response lists those security groups by name.
2. **Given** an instance created with no explicit security group, **When** its detail is read back, **Then** the response lists the effective default group (matching upstream behaviour) so a repeated plan is a no-op.
3. **Given** a successful `terraform apply` of a configuration that sets security groups, **When** `terraform plan` is run again unchanged, **Then** the plan proposes no replacement caused by security groups.

---

### User Story 3 - Regression is caught in CI (Priority: P2)

A maintainer wants a later change that drops `key_name` or `security_groups` from the
server-detail response to fail the build, so this class of drift cannot silently regress.

**Why this priority**: Regression protection matters, but it rides on US1/US2 being done
first. It is satisfied cheaply by the automated read-back tests that already run in CI; the
heavier end-to-end Terraform-in-CI smoke is deferred as an optional follow-up (see Out of
Scope).

**Independent Test**: Automated read-back tests assert `key_name` and `security_groups` on
the single-instance and list-detail responses and run in the standard CI test job; removing
either field from the response makes the build fail.

**Acceptance Scenarios**:

1. **Given** the read-back fix is present, **When** the CI test job runs, **Then** the read-back tests pass.
2. **Given** the read-back regresses (a field is dropped again), **When** the CI test job runs, **Then** those tests fail and block the change.

---

### Edge Cases

- **No key pair supplied**: detail returns `"key_name": null` (field present, JSON null), matching upstream Nova, so the provider does not read drift.
- **No security group supplied**: detail must report the effective default security group (not an empty list), so the provider does not read drift.
- **Unknown security group at create**: if a supplied security-group name cannot be resolved in the instance's project, the create is rejected with a Nova-compatible client error (HTTP 400, "security group not found") and no instance is created — matching upstream Nova (synchronous fault) and how the Terraform provider surfaces it.
- **Unknown key pair at create**: if a supplied `key_name` does not resolve to a keypair owned by the requesting user, the create is rejected with a Nova-compatible client error (HTTP 400, keypair not found) synchronously and no instance is created — matching real Nova v2.1 strict validation (Lightstack previously accepted it and booted keyless via an async lookup).
- **Multiple security groups**: all groups the instance was created with are listed, in a stable form the provider can compare.
- **Security group added/removed after create** (via the existing server action): detail continues to reflect the instance's current group membership, not only what was set at create.
- **List vs. show consistency**: the fields must appear consistently whether details are read one instance at a time or via the bulk "list with details" view, so tools that use either path agree.
- **Existing instances created before this change**: reading back an instance that predates persisted values must not error; it degrades to "no key pair"/default group rather than failing.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The single-instance detail response MUST include the key pair name that the instance was created with, as the `key_name` field; when the instance has no key pair, `key_name` MUST be present with a `null` value (matching upstream Nova).
- **FR-002**: The key pair name supplied at instance creation MUST be retained for the life of the instance so it can be read back (it is currently used only transiently and then discarded).
- **FR-002a**: If a `key_name` supplied at instance creation does not resolve to a keypair owned by the requesting user, the create MUST be rejected with a Nova-compatible client error (HTTP 400, keypair not found) synchronously, before the instance is created — matching upstream Nova v2.1 strict input validation and the Terraform provider (keypairs are user-owned). This replaces today's silent accept-and-boot-keyless behavior.
- **FR-003**: The "list instances with details" response MUST report the key pair name consistently with the single-instance detail response.
- **FR-004**: The single-instance detail response MUST include the instance's security groups, listed by name, in the shape the OpenStack Terraform provider and CLI expect.
- **FR-005**: Security groups supplied at instance creation MUST be retained/associated with the instance so they can be read back (they are currently dropped at creation).
- **FR-005a**: If any security-group name supplied at instance creation cannot be resolved in the instance's project, the create MUST be rejected with a Nova-compatible client error (HTTP 400, "security group not found") and MUST NOT create the instance (fail-fast, matching upstream Nova and the Terraform provider).
- **FR-006**: When no security group is supplied at creation, the detail response MUST report the effective default security group (matching upstream behaviour) rather than an empty list.
- **FR-006a**: The system MUST ensure a project's `default` security group exists — creating it if absent, matching OpenStack's guarantee that every project has a default security group — so that a create referencing `security_groups: [{"name":"default"}]` succeeds in any project, not only the seeded default project. Genuinely-unknown *non-default* names still fail per FR-005a. This holds whether `default` is named explicitly or implied by omitting `security_groups`; the guarantee is provided by Neutron (`neutron.EnsureDefaultSecurityGroup`) and used by both Nova at create and Neutron at port creation.
- **FR-007**: The "list instances with details" response MUST report security groups consistently with the single-instance detail response.
- **FR-008**: A repeated `terraform plan`/`apply` against an unchanged configuration MUST NOT propose replacing the instance due to key pair or security-group read-back; the second apply MUST be a no-op.
- **FR-009**: The added fields MUST match the upstream Nova response shape for `key_name` and `security_groups` so existing Terraform provider scripts, the OpenStack CLI, and the Horizon dashboard consume them without modification.
- **FR-010**: All other fields already present in the instance detail and list-detail responses, and the instance create/delete flows, MUST remain unchanged (no regression).
- **FR-011** *(US3)*: The project's automated test suite MUST assert `key_name` and `security_groups` on the single-instance and list-detail responses and run in CI, so that dropping either field fails the build. (A dedicated end-to-end Terraform-in-CI smoke is deferred — see Out of Scope.)

### Key Entities *(include if feature involves data)*

- **Instance (server)**: the compute instance whose detail is read back; gains a retained key pair name and a resolvable set of associated security groups exposed on read.
- **Key pair**: a named SSH public key belonging to a user; the instance records which key pair name it was booted with.
- **Security group**: a named network access group; an instance is associated with one or more, defaulting to the effective default group when none is specified.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a successful `terraform apply` of a configuration that sets a key pair and security groups, an immediate `terraform plan` reports **0 changes** (no instance replacement).
- **SC-002**: `openstack server show <id>` for an instance created with a key pair and a security group displays a non-empty key name and a non-empty security-group list matching what was supplied at creation.
- **SC-003**: The `key_name` and `security_groups` fields in both the show and list-detail responses match the upstream Nova response shape, so the unmodified OpenStack Terraform provider, CLI, and Horizon parse them without error.
- **SC-004**: Existing instance create/delete behaviour and every other detail/list-detail field are unchanged — the current unit and integration suites continue to pass.
- **SC-005** *(US3)*: When either read-back field is intentionally removed from the response, the CI test job fails; when present, it passes.

## Assumptions

- **Persistence is required, not a one-line read-back.** The ticket estimates "~1 line each", but the code shows neither value is available to read back today: the key pair name is parsed at create and used only to build cloud-init, never stored; and security groups supplied at create are not persisted at all (only a later add-security-group action records them). This feature therefore includes retaining both at creation, then reading them back.
- **Default security-group semantics follow upstream Nova**: an instance created with no explicit group reports the `default` group so that a provider re-plan is a no-op. Because OpenStack guarantees every project has a `default` security group, one is ensured (auto-created) per project at create — referenced explicitly or implied by omission (FR-006a) — not only seeded for the default project.
- **Real-mode Terraform validation is already DONE** and out of scope here — it was validated during #481 on k3s + node-local storage (`docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING_REAL_MODE.md`). Ceph/RGW-backed storage for that path was dropped (not supported).
- **Scope is limited to `key_name` and `security_groups` read-back** (plus the create-time persistence needed to support it) and CI regression coverage via the read-back tests — not broader server-detail field parity, and not the dedicated Terraform-in-CI job (deferred; see Out of Scope).
- Reference material: `docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md` (finding F1), the compatibility scenario under `test/terraform/`, and #481.

## Out of Scope

- Real-mode Terraform validation (already completed under #481 / SPEC-016).
- Ceph/RGW-backed storage for the real-mode Terraform path (dropped; unsupported).
- A dedicated end-to-end "Terraform validation" CI job (apply → assert an empty plan against a running stub-mode instance) — deferred as an optional follow-up; regression is guarded this iteration by the read-back tests running in the existing CI test job.
- Adding server-detail fields other than `key_name` and `security_groups`.
- Redesigning keypair or security-group data models beyond what is needed to read the two fields back.
