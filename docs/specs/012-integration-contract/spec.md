# Feature Specification: Integration Contract (Values Schema + Status Conventions)

**Feature Branch**: `012-integration-contract`  
**Created**: 2026-06-29  
**Status**: Draft  
**Input**: cloud-storage#474 — "[FEAT] Integration contract (values schema + status conventions)"

## Clarifications

### Session 2026-06-29

- Q: What form must the status-derivation contract take — prose, a documented deterministic recipe, or a shipped script? → A: A documented deterministic recipe — the exact `helm status` / pod-readiness fields → state mapping plus copy-paste example commands; no shipped executable.
- Q: How strict is cross-field validation (reject vs advisory for inconsistent input combinations)? → A: Balanced — reject release-breaking/unsafe combos; keep benign capacity/topology preferences advisory (warn or auto-correct, e.g. real mode → 1 replica).
- Q: What triggers the Failed classification? → A: Timeout OR terminal failure — Failed when the readiness bound elapses, or Helm reports the release failed, or a pod hits a terminal state (CrashLoopBackOff/ImagePullBackOff) first; Provisioning until then.
- Q: Does the schema validate credential strength or just presence? → A: Presence-only — reject empty inline `jwtSecret`/`adminPassword`, no min-length/complexity; recommended strength is documented guidance only. `existingSecret`-provided values are never schema-validated.
- Q: What endpoints does the contract guarantee — Keystone-anchored (catalog-driven) or full enumeration of all six? → A: Identity-anchored — guarantee the Keystone `/v3` endpoint (in-cluster Service name + advertised host/scheme when set); the other five services are discovered from the OpenStack service catalog returned in the token. The six in-cluster ports stay documented as non-authoritative debugging info; catalog correctness requires `endpointHost` to be set.

## User Scenarios & Testing *(mandatory)*

The contract has two halves and one publication requirement. The orchestrator that consumes
it is **sci-in-a-box**, which drives an o3k instance's lifecycle through Helm (explicitly
**not** a CRD): it submits configuration as Helm values (the input contract) and observes the
running release to derive lifecycle state and endpoints (the output/status contract).

### User Story 1 - Reject invalid configuration before anything is deployed (Priority: P1)

As the orchestrator (or an operator) installing or upgrading o3k via Helm, when I submit
configuration values, the chart validates the **complete** set of inputs against a published
schema and rejects structurally invalid configuration **up front — before any workload is
created** — with a message that names the offending value. A complete, valid configuration
installs normally.

**Why this priority**: This is the input half of the contract and the highest-leverage safety
property. Without it, a typo or a missing required value (database reference, credentials,
image) yields a broken or insecure release that fails opaquely at runtime. Fail-fast at
submission is what lets an automated orchestrator trust the chart.

**Independent Test**: Submit install/upgrade (and dry-run/lint/template) with a matrix of
invalid inputs — value outside the allowed set, out-of-range replica count, missing required
image/database/credentials, malformed external-database reference, unknown/misspelled key —
and confirm each is rejected before execution with a clear message; confirm a complete valid
configuration proceeds.

**Acceptance Scenarios**:

1. **Given** a values set with an out-of-range or wrong-typed input (e.g. replica count 0, a
   mode outside the allowed set), **When** install or upgrade is attempted, **Then** it is
   rejected at validation time and no workload is created.
2. **Given** a values set missing a required input (external database reference, signing/admin
   credentials unless an operator-supplied existing secret is named, or the container image),
   **When** install is attempted, **Then** it is rejected before workloads start, naming the
   missing input.
3. **Given** a values set containing an unknown/misspelled top-level key, **When** install is
   attempted, **Then** it is rejected (the key is not silently ignored).
4. **Given** a complete, valid values set, **When** install or upgrade is attempted, **Then**
   validation passes and the release proceeds.

---

### User Story 2 - Derive lifecycle state and endpoints from the release (Priority: P2)

As sci-in-a-box, after I install or upgrade an o3k release, I can deterministically classify it
as **Provisioning**, **Ready**, or **Failed** using only Helm release status plus pod
readiness, and I can resolve the Keystone endpoint (a stable, release-scoped Service) and read
the other service URLs from its OpenStack service catalog — without o3k-specific internal
knowledge.

**Why this priority**: This is the output half of the contract. An orchestrator drives
lifecycle: it must know when an instance is usable (Ready), still coming up (Provisioning), or
broken (Failed), and where to reach its APIs. Today the endpoints are documented but the state
derivation is not, so each consumer would otherwise have to invent it.

**Independent Test**: Against a real cluster, follow the documented convention: a healthy
release resolves to Ready with the documented endpoints reachable; a release whose pods never
become ready resolves to Provisioning and then Failed within the documented bound; the
derivation is identical in intent for single-node and multi-node topologies.

**Acceptance Scenarios**:

1. **Given** a release whose workloads become ready within the readiness bound, **When** the
   orchestrator applies the documented convention, **Then** the release is classified Ready and
   the documented endpoints resolve to the running services.
2. **Given** a release whose workloads do not become ready within the bound, **When** the
   convention is applied, **Then** the release is classified Provisioning while in progress and
   Failed once the bound/failure condition is met.
3. **Given** either a single-node or a multi-node deployment, **When** the convention is
   applied, **Then** the same Provisioning/Ready/Failed outcome is derived using the
   topology-appropriate workload object.
4. **Given** a Ready release, **When** the orchestrator resolves the Keystone endpoint and reads
   the service catalog from its token, **Then** it obtains the correct, catalog-advertised URLs
   for all services (honoring `endpointHost`/scheme), with the node-local metadata endpoint not
   present in the catalog.

---

### User Story 3 - One published, versioned contract document (Priority: P3)

As a consumer or operator, I can find a **single contract document published alongside the
chart** that ties the inputs (schema) to the outputs (status + endpoints), so I can integrate
against o3k without reading its source. The document travels with the chart and matches the
version I deployed.

**Why this priority**: Discoverability and drift-prevention. A contract that lives only in
tribal knowledge or scattered files erodes; publishing it with the chart and linking it from
the README keeps it findable and version-matched.

**Independent Test**: Inspect a packaged chart artifact: the contract document is present (not
excluded from packaging), the chart README links to it, and its described inputs/status/
endpoints match the actual schema and release notes.

**Acceptance Scenarios**:

1. **Given** the packaged chart, **When** a consumer opens the contract document, **Then** it
   describes every input, the status-derivation convention, and the endpoint-resolution
   convention in one place.
2. **Given** the chart README, **When** a consumer looks for the contract, **Then** the README
   links to the contract document.
3. **Given** a change to an input or a status convention, **When** the chart is updated,
   **Then** the contract document is updated in lockstep (verified by tests/gates).

---

### Edge Cases

- Unknown/misspelled top-level key in values → rejected (not silently ignored); free-form maps
  (annotations, labels, resource requests, scheduling constraints) remain open.
- Required credentials omitted **but** an operator-supplied existing secret is named → accepted
  (the existing-secret path satisfies the requirement).
- Mutually constrained inputs (e.g. destructive teardown selected without the supporting input
  it requires) → rejected before execution.
- A partially-ready release (some pods up, some not) → Provisioning until the readiness bound is
  satisfied; Failed early if a pod hits a terminal state (CrashLoopBackOff / ImagePullBackOff) or
  Helm reports the release failed, otherwise Failed when the bound elapses.
- Multi-node topology where cross-node behavior is a known deferred limitation (issue #52) →
  status derivation still resolves Ready/Provisioning/Failed for what is deployed; the contract
  documents the deferred-scope caveat rather than misreporting health.
- TLS enabled vs disabled → the scheme in the resolved endpoint URLs reflects the configured
  scheme.

## Requirements *(mandatory)*

### Functional Requirements

**Input contract (schema)**

- **FR-001**: The chart MUST publish a machine-readable values schema that documents and
  validates **every** supported input, including (at minimum) container image reference,
  deployment topology/profile, replica count, external database reference(s), credentials,
  exposure/TLS settings, per-service modes, real-mode settings, multi-node settings, cache
  settings, data-retention/teardown settings, and observability settings.
- **FR-002**: Every input in the schema MUST carry a human-readable description — the schema
  **documents**, not only validates.
- **FR-003**: The chart MUST reject structurally invalid configuration **at submission time —
  before any workload is created** — for both install and upgrade, and the same validation MUST
  be observable in dry-run/lint/template.
- **FR-004**: Validation MUST reject: values outside an allowed set, out-of-range or
  wrong-typed values, and missing required inputs (container image, external database
  reference, and signing/admin credentials unless an operator-supplied existing secret is
  named).
- **FR-004a**: Credential validation is **presence-only** — the schema rejects empty/missing
  inline `jwtSecret`/`adminPassword` but does NOT enforce a minimum length or complexity;
  recommended credential strength is documented as guidance in the contract, not structurally
  enforced. Credentials supplied via an existing secret are never schema-validated (the chart
  does not read them).
- **FR-005**: The external database reference inputs MUST be fully described and validated
  (today the single largest gap — an untyped object).
- **FR-006**: The schema MUST define how unknown/misspelled keys are treated. Default: reject
  unknown keys at the top level and on typed objects so typos fail fast; free-form maps
  (annotations, labels, resource requests, scheduling constraints) remain open. *(Assumption —
  see Assumptions.)*
- **FR-007**: Where a required input is conditional (credentials required unless an existing
  secret is named; the destructive-teardown image required only when destructive teardown is
  selected), the chart MUST reject a submission that violates the condition before workloads
  start. *(Whether enforced structurally in the schema or at render time — the latter already
  exists as fail-fast template guards — is an implementation choice for planning.)*
- **FR-007a**: The contract MUST classify each cross-field constraint as **rejected** (a
  structurally invalid or unsafe combination fails install/upgrade before workloads start) or
  **advisory** (a benign capacity/topology preference that is auto-corrected or surfaced as a
  warning, not blocked). Release-breaking/unsafe combinations are rejected; benign preferences —
  e.g. real mode pinning to a single replica, or multi-node + cache without a shared Redis — are
  auto-corrected or warned, matching the chart's existing behavior.
- **FR-008**: The "version" input is the **product image tag** (`image.tag`) that selects which
  o3k build is deployed; when unset it falls back to the chart's product version (`appVersion`).
  The schema MUST name and document it as the version input and type it (an image tag is an
  open-ended string — registry-defined — so validation is type + description, not an allowed-set
  enum). The multi-node `minProductVersion` is a SEPARATE, advisory-only compatibility floor
  (documented as such — not enforced today; tied to deferred multi-node, issue #52), NOT "the
  version."

**Output / status contract**

- **FR-009**: The contract MUST define a deterministic mapping from Helm release status + pod
  readiness to exactly one of **Provisioning / Ready / Failed**, delivered as a **documented
  recipe** — naming the exact `helm status` / pod-readiness fields and the mapping rules, with
  copy-paste example commands — precise enough for the orchestrator to implement reliably. It is
  NOT prose-only, and the chart does NOT ship an executable that emits the state.
- **FR-010**: The contract MUST document the readiness gate the orchestrator relies on (a
  bounded wait with a timeout) and the **Failed** trigger: a release is Failed when the readiness
  bound elapses without readiness, OR Helm reports the release failed, OR a pod reaches a
  terminal state (e.g. CrashLoopBackOff / ImagePullBackOff) before the bound — so the orchestrator
  can fail fast rather than always waiting out the full timeout. Until a terminal signal or the
  bound, the release is Provisioning.
- **FR-011**: The status derivation MUST be defined for both topologies, naming the
  topology-appropriate workload object to watch (single-node vs multi-node).
- **FR-012**: The contract MUST document endpoint resolution as **identity-anchored**: the
  guaranteed entry point is the Keystone `/v3` endpoint — the in-cluster Service name
  (`<release>-o3k:35357`) and, when configured, the advertised host/scheme. The other five
  services (compute, placement, network, volumev3, image) are discovered from the OpenStack
  **service catalog** returned in the Keystone token, NOT enumerated as a separate guarantee.
  The chart's release notes MAY list the six in-cluster ports as a debugging convenience, marked
  non-authoritative. The node-local metadata endpoint is explicitly excluded.
- **FR-012a**: The contract MUST state that catalog correctness depends on `endpointHost` being
  set to a consumer-reachable host (with the scheme matching TLS): unset, the catalog advertises
  `localhost`, which is unusable by an external orchestrator. This couples the input
  `endpointHost` to the output endpoint contract.

**Documentation / publication**

- **FR-013**: A **single** contract document MUST be published alongside the chart, tying inputs
  (schema) to outputs (status + endpoints) and framed for the orchestrator (sci-in-a-box) as the
  consumer.
- **FR-014**: The chart README MUST link to the contract document.
- **FR-015**: The contract document MUST be included in the packaged chart artifact (not
  excluded by packaging-ignore rules).
- **FR-016**: Automated chart tests/gates MUST assert that invalid inputs are rejected and that
  the documented contract stays in sync with the schema and release notes (so they do not
  drift).

### Key Entities *(include if feature involves data)*

- **Values schema**: the machine-readable description + validation of all chart inputs; the
  input half of the contract.
- **Input value set**: the configuration an operator/orchestrator submits (image, topology,
  replicas, database reference, credentials, exposure, modes, retention, observability, …).
- **Release status signal**: Helm release status combined with pod readiness, from which
  lifecycle state is derived.
- **Lifecycle state**: the derived classification — Provisioning, Ready, or Failed.
- **Endpoint set**: the Keystone entry-point URL plus the per-service URLs discovered from the
  OpenStack service catalog (advertised per `endpointHost`/scheme); the node-local metadata
  endpoint is not part of it.
- **Contract document**: the published artifact tying inputs to outputs, versioned with the
  chart.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of supported inputs appear in the schema with a description and a validation
  rule (type / allowed-set / range / required, as applicable) — no undocumented or untyped
  input remains.
- **SC-002**: Every invalid-input case in the test matrix (out-of-set value, out-of-range,
  missing-required, malformed database reference, unknown key) is rejected before any workload
  is created, and every valid case proceeds — 0 invalid cases reach execution.
- **SC-003**: Using only Helm release status and pod readiness, an integrator classifies a
  release as Provisioning, Ready, or Failed with no o3k-specific source knowledge by following
  the documented convention — demonstrated for both topologies.
- **SC-004**: Following the contract, an integrator resolves the Keystone endpoint and obtains
  all other service URLs from the token's service catalog on the first attempt — matching the
  catalog the running release actually serves (honoring `endpointHost`/scheme), with no
  node-local metadata endpoint present.
- **SC-005**: The packaged chart contains the contract document, the README links it, and an
  automated gate fails if the schema, release notes, and contract drift out of sync.
- **SC-006**: An integrator unfamiliar with o3k internals completes an install → detect-Ready →
  resolve-endpoints loop using only the published contract (no source reading).

## Assumptions

- **Contract-only scope.** This feature formalizes/validates **existing** chart inputs and
  documents the status + endpoint conventions. It does NOT add new runtime features or change
  what o3k deploys. The deferred multi-node cross-node behavior (issue #52) is out of scope and
  appears only as a documented caveat.
- **Strict by default.** Unknown top-level/typed-object keys are rejected (per the user's
  stated lean in the issue checklist); free-form maps stay open. Revisit if operators rely on
  passing through unrecognized keys.
- **Conditional requirements enforced before workloads start.** The "required unless existing
  secret" and "teardown image required only under destructive teardown" rules are enforced
  pre-execution; the mechanism (schema conditional vs the existing render-time fail-fast guards)
  is a planning decision, not a spec constraint.
- **Consumer.** sci-in-a-box is the primary consumer/orchestrator; the contract is also usable
  by any Helm-based operator. The interface is Helm values + release status — explicitly NOT a
  CRD.
- **Built on existing artifacts.** The endpoint and status conventions extend what the chart
  already ships (release notes already list endpoints; the schema already validates a subset);
  this feature completes and formalizes them rather than starting fresh.

## Dependencies

- The existing o3k Helm chart — its values, schema, release notes, README, and test harness;
  this feature extends them.
- Helm's built-in values-schema validation and its release-status / pod-readiness reporting, as
  the enforcement and signal mechanisms the contract is defined against.
