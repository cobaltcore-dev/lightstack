# Feature Specification: Neutron API Fidelity & Real-Mode Networking

**Feature Branch**: `feature/546` (feature dir `024-neutron-api-fidelity-realmode`)
**Created**: 2026-08-11
**Status**: Draft
**Input**: cloud-storage#546 — "o3k — Neutron API fidelity & real-mode networking (B6, B9, B10, B11, F10, F11)", title + body + 1 comment. Sub-issue of #536; absorbs F10 and F11 from the closed #537.

## Overview

Six Neutron defects, grouped because they share one surface: the network service's request
handling. Two of them stop a user from doing anything at all — a port cannot be created, so a guest
never reaches the network, and a Neutron client cannot parse a Lightstack error because it is wrapped in
another service's envelope. The remaining four are fidelity gaps: a requested owner is silently
discarded, a list filter is ignored, the same rule reads back differently than it was written, and
the concept of an external network is absent so egress cannot be configured from the dashboard.

The common thread is **answers that look successful but are not**: `201` for a request that was
partly discarded, `200` with a list that ignored the filter, a create response that disagrees with
the read of the same object. A client cannot detect any of these from the status code, which is why
the existing contract suites pass while the behaviour is wrong.

## Clarifications

### Session 2026-08-11

- Q: How precise should the Neutron error `type` vocabulary be across the service's ~244 error sites? → A: Use the real upstream condition name wherever upstream has one; a single defensible generic for Lightstack-internal failures with no upstream counterpart
- Q: How should an external network owned by one project be made visible to others, given the network listing scopes to owner-or-shared? → A: Visible to every authenticated caller as a third disjunct alongside owner and shared, read-only — upstream's rule, and independent of `shared`
- Q: A gatewayless subnet currently loses the metadata route as well as the default route, because both DHCP options are guarded on the gateway. How should FR-023 and FR-004 be reconciled? → A: Separate the two — advertise no default route, but always advertise the metadata route via the subnet's own on-link address
- Q: What should happen when a stored subnet row cannot be read while building a listing? → A: Log a warning naming the row and keep serving the rest — never fail the whole listing, and no non-upstream partial-result field
- Q: Which subnet list filters are in the contract, given upstream has ~15? → A: The network filter plus identifier, name, address range, address family, DHCP setting and project — mirroring what the network listing already supports; anything else is not advertised
- Q: What happens to router gateways and floating IPs configured before the external flag existed, since it defaults to false? → A: Validate new create/update requests only; grandfather stored rows and let reconciliation keep serving them, so an upgrade never silently removes working egress
- Q: What status should refuse a network-create naming a project that does not exist? → A: 400 bad request — the same status as every other rejection of that body attribute, so clients get one rule

---

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Attach a guest to a tenant network (Priority: P1)

An operator creates a network and a subnet, then creates a port on that network so a guest can be
attached to it. Today the port request fails with a server error, so nothing downstream of it works:
the guest boots with no interface, cannot reach the metadata service, and reports no carrier on its
console.

**Why this priority**: it is the only defect in the set that makes a core operation impossible
rather than merely inaccurate, and it is the suspected root of the guest-side metadata failure —
so guest network reachability as a whole rides on it. Everything else in this feature is a
correctness gap on an operation that at least completes.

**Independent Test**: create a network and a subnet, restart the service, then create a port on that
network — the port is created. In real mode, additionally confirm the guest's tap device and its
network's bridge end up in the same network namespace, and that a booted guest retrieves its
metadata.

**Acceptance Scenarios**:

1. **Given** a network created earlier, **When** a port is created on it, **Then** the port is
   created successfully and reports the network, a MAC address and a fixed IP from the subnet.
2. **Given** the network service has been restarted since the network was created, **When** a port
   is created on that network, **Then** it still succeeds — port creation does not depend on state
   held only for the lifetime of a single service process.
3. **Given** real-mode networking, **When** a port is created, **Then** the port's tap device and
   the bridge it is attached to reside in the same network namespace, and the attach succeeds.
4. **Given** real-mode networking and a guest attached to such a port, **When** the guest boots,
   **Then** it obtains its address and retrieves instance metadata rather than reporting no carrier.
5. **Given** a port creation that fails partway, **When** the failure is returned, **Then** no
   orphaned device or half-written port row is left behind.

---

### User Story 2 — Read a Neutron error with a Neutron client (Priority: P1)

A user's SDK, CLI or Terraform provider receives an error from the network service and needs to read
the reason out of it. Today the network service answers in the compute service's error envelope, so
a Neutron-aware client finds none of the fields it looks for and reports an opaque failure instead
of the actual reason.

**Why this priority**: it is a compatibility break on every failing request the network service can
make, not one endpoint; and it is the only defect in the set that is already failing a committed
test gate, so the whole test suite cannot pass while it is open. Fixing it therefore both restores a
gate and removes a client-visible incompatibility.

**Independent Test**: request an absent network, subnet, port, router, floating IP and security
group; each response carries Neutron's error envelope with Neutron's field set. Then run the error
handling suite and the network contract suite — both pass.

**Acceptance Scenarios**:

1. **Given** an identifier that matches no network, **When** it is fetched, **Then** the response
   status is 404 and the body is Neutron's error envelope, not the compute service's.
2. **Given** any failing network-service request — malformed body, forbidden, absent, conflicting,
   or internal — **When** the error is returned, **Then** the body is Neutron's envelope carrying
   the error type, a human-readable message and a detail field, and does **not** carry the numeric
   code field the compute envelope uses.
3. **Given** the error type field on a condition upstream Neutron also has, **When** a client reads
   it, **Then** it carries upstream's own name for that condition, not one generic value shared by
   every error; a purely internal fault may carry the documented generic type instead.
4. **Given** the compute and block-storage services, **When** they return errors, **Then** their
   envelopes are unchanged — this feature changes the network service only.
5. **Given** the full test suite, **When** it is run, **Then** the error-consistency check passes
   and the suite as a whole is no longer red on this defect.

---

### User Story 3 — A network's owning project is honest (Priority: P2)

An administrator creates a network on behalf of another project. Today the requested project is
dropped before the handler ever sees it: the response is `201`, and the network belongs to the
administrator's own project. Nothing in the response says so, because every network response reports
the caller's project rather than the network's actual owner.

**Why this priority**: it silently produces the wrong resource ownership and there is no way for the
client to tell. It already cost real debugging time — fixtures created with an explicit project put
both tenants' networks in the administrator's project, after which a tenant's guest failed port
allocation and *booted anyway* with no addresses, a symptom indistinguishable from a cross-tenant
authorization defect. It is ranked below the two P1s only because the operation does complete for
the ordinary single-project case.

**Independent Test**: as an administrator, create a network naming a second project, then read the
network back and compare its reported owner against the database. The request either takes effect or
is refused; it is never accepted-and-ignored.

**Acceptance Scenarios**:

1. **Given** an administrator naming a project other than their own, **When** the network is created,
   **Then** the network belongs to the named project and the response reports that project — the
   request is honoured, matching upstream, not accepted and redirected.
2. **Given** a non-administrator naming another project, **When** the network is created, **Then**
   the request is refused as a bad request naming the reason, rather than silently redirected into
   their own project.
3. **Given** a request sending both spellings of the project attribute with values that disagree,
   **When** it is submitted, **Then** it is refused as a bad request rather than one value being
   picked arbitrarily.
4. **Given** a request naming a project that does not exist, **When** it is submitted by an
   administrator, **Then** it is refused as a bad request — the same status as the other two ways of
   getting this attribute wrong — rather than creating a network owned by nothing or leaking a
   storage-level error.
5. **Given** any network response — create, list, show, update — **When** the owning project is
   read, **Then** it is the network's actual owner, not the caller.
6. **Given** a shared network owned by another project, **When** a tenant lists or shows it, **Then**
   it is reported with its real owner, so the tenant is not told a foreign resource is theirs.
7. **Given** a refused cross-project request, **When** it is logged, **Then** the log names both the
   caller and the requested project, matching how the equivalent refusal is logged elsewhere.

---

### User Story 4 — Subnet listings answer the question asked (Priority: P2)

A user lists the subnets of one network. Today the filter is ignored and every visible subnet is
returned, so the client must filter locally or shows the wrong rows. Separately, any subnet whose
stored values do not fit the reader's expectations is dropped from the list without a trace — the
user sees a shorter list and no indication that anything is missing.

**Why this priority**: a wrong list is worse than an error, because it is acted on. The silent-drop
half is the more dangerous of the two: a resource that exists, is authorized, and simply does not
appear.

**Independent Test**: create two networks each with a subnet, list subnets filtered by one network's
identifier, and receive only that network's subnet. Separately, create a subnet with no gateway and
confirm it appears in the listing and can be shown and updated.

**Acceptance Scenarios**:

1. **Given** subnets on two networks, **When** the subnet list is filtered by one network, **Then**
   only that network's subnets are returned.
2. **Given** the same listing, **When** it is filtered by name, identifier, address range, address
   family, DHCP setting or owning project, **Then** the filter is applied rather than ignored.
2a. **Given** a query parameter outside that set, **When** the listing is requested, **Then** the
   result is the same as if it had not been sent — an unknown parameter never narrows the answer.
3. **Given** a subnet with no gateway address, **When** subnets are listed, **Then** it appears, and
   its gateway is reported as absent rather than as an invented address.
4. **Given** a subnet with no gateway address, **When** it is shown or updated by identifier,
   **Then** the request succeeds rather than failing with a server error.
4a. **Given** a gatewayless subnet with address assignment enabled, **When** a guest on it boots,
   **Then** it receives an address and can still reach the metadata service, and receives no default
   route.
5. **Given** a request to create a subnet that explicitly asks for no gateway, **When** it is
   created, **Then** no gateway is assigned — the request is honoured rather than overwritten with
   an automatically chosen address.
6. **Given** a stored subnet that genuinely cannot be read, **When** the listing is built, **Then**
   a warning naming that row is recorded, the remaining subnets are still returned, and the request
   succeeds — never a silently shorter list, and never a failed listing for every caller.

---

### User Story 5 — A security-group rule reads back the way it was created (Priority: P3)

A user creates a security-group rule leaving the remote prefix, remote group and port range unset,
then reads the rule back. The create response reports empty strings and zeros; the read reports
absent values. The same rule described two different ways breaks clients that compare, cache or
round-trip the object — including infrastructure-as-code tools that diff the created object against
the desired state and see a change that is not there.

**Why this priority**: it is a self-consistency defect with a narrow blast radius and no data loss —
the stored rule is correct, only one of four descriptions of it is wrong. Three of the four
descriptions were already aligned; this is the one that was missed.

**Independent Test**: create a rule with the optional fields unset, capture the create response,
then fetch the same rule by identifier, from the rule list, and from its group — all four
descriptions are identical.

**Acceptance Scenarios**:

1. **Given** a rule created with no remote prefix and no remote group, **When** the create response
   is read, **Then** both are reported as absent, matching what the subsequent read reports.
2. **Given** a rule created with no port range, **When** the create response is read, **Then** the
   range bounds are reported as absent rather than as zero, which would describe a different rule.
3. **Given** a rule created with every optional field set, **When** all four descriptions are
   compared, **Then** they agree — the alignment must not regress for populated rules either.
4. **Given** the existing check that the other three descriptions agree, **When** it is extended to
   cover the create response, **Then** it fails before the fix and passes after it.

---

### User Story 6 — Configure guest egress from the dashboard (Priority: P3)

An operator opens the dashboard's router *Set Gateway* dialog to give tenant guests a route out. The
network dropdown is always empty, because no network can be marked as external and a query for
external networks always returns nothing. Egress cannot be configured from the user interface at
all.

**Why this priority**: it is the largest piece of work in the set and the only one that adds a
concept rather than correcting one, and there is a working command-line path to the same outcome. It
is genuinely user-visible, but it blocks a convenience rather than a capability.

**Independent Test**: mark a network as external, list external networks and see it, then open the
dashboard's *Set Gateway* dialog and select it. Confirm a router given that gateway provides guest
egress.

**Acceptance Scenarios**:

1. **Given** a network created or updated as external, **When** it is read back, **Then** it is
   reported as external, and the flag survives a service restart.
2. **Given** an external and a non-external network, **When** external networks are listed, **Then**
   only the external one is returned; when non-external networks are listed, only the other.
3. **Given** a non-administrator, **When** they attempt to mark a network external, **Then** the
   request is refused; when they list external networks, they can see them.
3a. **Given** an external network owned by another project and not marked shared, **When** a tenant
   lists or shows networks, **Then** it is visible to them, reported with its real owner — but they
   cannot attach a subnet to it, rename it, delete it, or change its external status.
4. **Given** an external network exists, **When** the dashboard's router gateway dialog is opened,
   **Then** it offers that network for selection.
5. **Given** a router whose gateway is set to a network that is *not* external, **When** the request
   is made, **Then** it is refused — today any network is silently accepted as an external one.
6. **Given** a router with an external gateway, **When** a guest behind it sends outbound traffic,
   **Then** it reaches the outside, and a floating IP allocated from that network reaches the guest.
7. **Given** a deployment upgraded with a router gateway already configured on a network that is not
   marked external, **When** the service restarts and reconciles, **Then** that router's egress still
   works and the condition is logged for the operator — the upgrade takes nothing away.

> **Scope decision (2026-08-11)**: external networks are **modelled**, not documented away — the
> ticket's second option (record the gap as intended) was declined. This story therefore carries the
> feature's only stored-data change, and it is the reason it stays at P3: it can be deferred to a
> follow-up without disturbing the other five stories.

---

### Edge Cases

- A port is requested on a network whose bridge does not exist and cannot be created — the failure
  must name the reason rather than reporting a generic server error, and must leave nothing behind.
- A port is requested on a shared network owned by another project — this remains allowed, and the
  device is placed in the namespace that the network's bridge is in, not the caller's.
- Two tenants have a network with the same name, and one of them creates a port by name — the port
  must not land on the other tenant's network.
- An error occurs before the request has been authenticated or routed to a resource handler — it
  still needs the network service's envelope, not a framework default.
- A subnet listing is paged and filtered at the same time — the filter must apply to the whole
  result set, not just the page, and the page boundary must remain stable.
- A subnet's gateway is explicitly removed by an update — the absence must be persisted and
  reported, address assignment must be reconfigured to stop advertising a default route, and the
  metadata route must survive the change (FR-023a).
- A gatewayless subnet is created with address assignment enabled — guests get an address and reach
  metadata, but have no default route. This is the requested outcome, not a defect.
- A rule's port range is set to `0` deliberately (a valid ICMP type) — reporting it as absent would
  be the mirror image of the current defect and is equally wrong.
- The last external network is deleted while a router still uses it as a gateway — the router's
  existing gateway must not be left pointing at a network that no longer qualifies.
- A network is marked external and then unmarked while routers depend on it — those routers keep
  working under the grandfathering rule (FR-035a), and the condition is logged (FR-035b); what is
  refused is configuring a *new* gateway on it.
- A deployment is upgraded with routers and floating IPs already in place — every one of them keeps
  its egress, even though the new attribute defaults to not-external.
- Two networks are both marked external — nothing may assume there is exactly one.

## Requirements *(mandatory)*

### Functional Requirements

**Port creation (US1)**

- **FR-001**: Creating a port on an existing network MUST succeed whenever the caller is entitled to
  that network, regardless of how long ago the network was created or how many times the service has
  restarted since.
- **FR-002**: Port creation MUST NOT depend on bookkeeping held only in the memory of the current
  service process; the recorded networks are the authority for what exists.
- **FR-003**: In real mode, a port's device and the bridge it attaches to MUST be placed in the same
  network namespace, and that namespace MUST exist before either is created.
- **FR-004**: A guest attached to a created port MUST obtain an address and reach the metadata
  service in real mode. Metadata reachability MUST NOT depend on the subnet having a gateway
  (see FR-023a).
- **FR-005**: A failed port creation MUST leave no device and no port record behind, and its error
  MUST identify which step failed.

**Error envelope (US2)**

- **FR-006**: Every error the network service returns MUST use Neutron's error envelope, carrying an
  error type, a message and a detail field, and MUST NOT carry the numeric code field the compute
  envelope uses.
- **FR-007**: The error type MUST name the specific condition for the resource concerned, using
  upstream Neutron's own name for that condition wherever one exists (the per-resource not-found
  conditions, the in-use conditions, address-allocation failure, and so on), rather than one generic
  value reused for every error.
- **FR-007a**: Failures with no upstream counterpart — the service's own internal faults, such as a
  host networking operation that could not be carried out — MUST use a single documented generic type
  rather than an invented per-site name. The set of type names MUST be enumerated in one place so the
  vocabulary cannot drift as new error sites are added.
- **FR-008**: Every failing path of the network service MUST be converted, not only the not-found
  ones — a partial conversion that keeps the compute body inside a Neutron wrapper is explicitly not
  acceptable.
- **FR-009**: The one endpoint that already emits Neutron's envelope by hand MUST be folded into the
  shared path so there is a single description of the envelope.
- **FR-010**: The compute, identity, block-storage and image services MUST keep the envelopes they
  use today; no other service's error format changes.
- **FR-011**: The committed error-consistency check MUST pass, and the check MUST be shown failing
  before the change and passing after it.

**Network ownership (US3)**

- **FR-012**: A network-creation request that names a project MUST NOT be accepted and ignored. The
  request is either honoured or refused with a status and message naming the reason.
- **FR-013**: An administrator naming a project other than their own MUST have the request honoured:
  the network is owned by the named project. This matches upstream, where the ability to set the
  project on create is exactly an administrator's privilege.
- **FR-014**: A non-administrator naming a project other than their own MUST be refused as a **bad
  request** — not a forbidden and not a not-found — carrying upstream's reason that specifying a
  project other than the authenticated one is not allowed for this user.
- **FR-015**: Both spellings of the project attribute MUST be accepted, and MUST be treated as the
  same attribute: sending one populates the other, and sending both with values that disagree MUST be
  refused as a bad request.
- **FR-016**: A named project that does not exist MUST be refused as a **bad request** rather than
  producing a network with an unusable owner or surfacing a storage-level failure. Every rejection of
  this one body attribute — a non-administrator naming a foreign project, the two spellings
  disagreeing, and a project that cannot be resolved — MUST carry the same status, so a client has one
  rule to follow rather than three. The refusal MUST NOT reveal whether some other project exists.
- **FR-017**: Every network response — create, list, show and update — MUST report the network's
  actual owning project, not the caller's.
- **FR-018**: A refused cross-project network request MUST be logged naming the caller's project and
  the requested project.

**Subnet listing (US4)**

- **FR-019**: The subnet listing MUST apply the network filter, returning only subnets of the named
  network.
- **FR-020**: The subnet listing MUST apply the remaining filters in its contract: identifier, name,
  address range, address family, DHCP setting, and owning project under both of its spellings. This
  set mirrors what the network listing already supports and covers what the command-line client, the
  dashboard and the infrastructure-as-code provider actually send.
- **FR-020a**: Filters outside that set MUST NOT be advertised as supported, and an unrecognised query
  parameter MUST NOT silently narrow the result. Quietly returning fewer rows than asked for is the
  defect class this feature exists to remove, so an unknown filter must leave the result unchanged
  rather than become an accidental predicate.
- **FR-021**: A subnet with no gateway address MUST appear in listings and MUST be readable and
  updatable by identifier without failing.
- **FR-022**: An absent gateway MUST be reported as absent, distinctly from an address of any kind.
- **FR-023**: A request that explicitly asks for a subnet with no gateway MUST be honoured; the
  service MUST NOT substitute an automatically chosen gateway for it. Omitting the attribute
  altogether MUST keep today's behaviour of choosing one, so "not specified" and "explicitly none"
  remain distinguishable requests.
- **FR-023a**: A gatewayless subnet MUST NOT advertise a default route, and MUST still advertise the
  route to the metadata service, reached on-link rather than through a gateway. The two are separate
  concerns and MUST NOT be gated on each other: a user asking for no gateway is declining a default
  route, not declining instance metadata.
- **FR-024**: A stored subnet that cannot be read MUST NOT be omitted from a listing without a trace.
  The service MUST record a warning identifying the affected row and MUST continue building the rest
  of the listing — one unreadable row may not fail the whole request, because that would blank every
  caller's subnet list including the dashboard's.
- **FR-024a**: The response MUST NOT gain a non-upstream field to signal incompleteness; the operator
  log is the channel for it. Applies to every listing this feature touches, not only subnets.
- **FR-025**: Filters MUST compose with paging so that filtering applies to the whole result set and
  page boundaries stay stable.

**Security-group rule consistency (US5)**

- **FR-026**: The rule-creation response MUST describe the created rule identically to the way the
  same rule is subsequently described when fetched by identifier, listed, or read through its group.
- **FR-027**: Unset optional attributes — remote prefix, remote group, port range bounds — MUST be
  reported as absent in every description, never as an empty value or zero.
- **FR-028**: A deliberately zero port value MUST be preserved as zero, not converted to absent.
- **FR-029**: The existing agreement check across the other descriptions MUST be extended to include
  the creation response, and MUST fail before the change.

**External networks (US6)**

- **FR-030**: A network MUST be able to be marked as external, and the mark MUST persist across
  service restarts.
- **FR-031**: Every network response MUST report the network's actual external status rather than a
  fixed value.
- **FR-032**: Listing networks filtered by external status MUST return the matching networks —
  external-only and non-external-only queries MUST be complements, not one of them always empty.
- **FR-033**: Marking a network external MUST be restricted to administrators; reading external
  status and listing external networks MUST be available to any authenticated caller. Upstream gates
  exactly this attribute on create to administrators, so this is fidelity rather than a local choice.
- **FR-033a**: An external network MUST be visible to every authenticated caller regardless of which
  project owns it, as a third independent way to qualify for visibility alongside "owned by the
  caller" and "shared". Being external MUST NOT require also being shared, and MUST NOT depend on it.
- **FR-033b**: That visibility is **read-only**: appearing in a caller's network list or show response
  MUST NOT let a non-owner attach a subnet to the network, rename it, delete it, or change its
  external status. The existing rule that sharing a network never confers authority over its address
  ranges applies unchanged to external networks.
- **FR-034**: Setting a router's gateway MUST require the named network to be external, and MUST
  refuse a network that is not.
- **FR-035**: Allocating a floating IP MUST require its network to be external.
- **FR-035a**: The two checks above apply to **new** create and update requests only. A gateway or
  floating IP configured before the external attribute existed MUST keep working, and periodic
  reconciliation MUST keep asserting its address translation rather than withdrawing it — an upgrade
  may not silently remove working guest egress. Nothing may retroactively mark a network external to
  make old state pass, because that would grant an administrator-only, tenant-visible attribute to
  networks no operator designated.
- **FR-035b**: Grandfathered gateways MUST be discoverable rather than merely tolerated: a router
  whose gateway network is not external MUST be identifiable from the service's logs at startup, so
  an operator can mark the network and converge deliberately.
- **FR-036**: Guest egress through a router whose gateway is an external network MUST work in real
  mode, and a floating IP from that network MUST reach the guest.
- **FR-037**: The dashboard's router gateway selection MUST offer the external networks.

**Cross-cutting**

- **FR-038**: Each of the six defects MUST be covered by a test that is demonstrated failing before
  the change and passing after it.
- **FR-039**: No defect in this feature may be fixed in a way that re-opens one fixed elsewhere —
  in particular the cross-tenant subnet-write rule, the subnet ownership reporting rule, and the
  console lookup behaviour must remain as they are.
- **FR-040**: Documentation that describes the network service's error format, its network
  attributes, or the absence of external networks MUST be updated to match the delivered behaviour,
  including the stale cross-reference that attributes the envelope divergence to an unrelated
  security issue.

### Key Entities

- **Network**: a tenant's layer-2 network. Owns a project (which must be reported truthfully, US3)
  and an external flag that decides whether it can serve as a router gateway or a floating-IP pool. Backed by a bridge whose location determines where guest devices must be placed.
- **Subnet**: an address range on a network. Its gateway is optional; the absence of one is a
  meaningful state that must be storable, reportable and requestable.
- **Port**: a guest's attachment to a network. Carries a device that must live in the same namespace
  as its network's bridge.
- **Security-group rule**: a filter entry described in four places; the descriptions must agree, and
  unset optional attributes must be absent rather than zero-valued.
- **Error response**: the network service's failure description. Distinct from the compute service's
  in both wrapper and field set; carries a condition type, a message and a detail.
- **External network**: a network designated as the boundary to the outside; consumed by router
  gateways and floating-IP allocation, both of which currently accept any network.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A user can create a network, a subnet and a port and attach a guest that reaches the
  network and retrieves its metadata — the end-to-end path succeeds where it previously failed at
  the port step.
- **SC-002**: Port creation succeeds on 100% of networks that exist and are authorized, including
  networks created before the most recent service restart.
- **SC-003**: 100% of the network service's error responses carry the network service's own envelope
  and field set; 0% carry the compute service's.
- **SC-004**: The full test suite passes end to end, with the error-consistency check reporting all
  of its checks passed — it currently fails one and returns a non-zero exit, blocking the suite.
- **SC-005**: No request that names a project other than the caller's is answered with a success that
  did not take effect: an administrator's request is applied and a non-administrator's is refused,
  both verified by comparing the response against the stored owner rather than against the status
  code alone.
- **SC-006**: A filtered subnet listing returns exactly the matching subnets — no extra rows and no
  missing rows — for each of the seven filters in its contract, and returns the unfiltered result for
  a parameter outside it.
- **SC-007**: A subnet with no gateway is visible in listings, readable and updatable, where today
  it is absent from listings and errors on read.
- **SC-008**: The same security-group rule is described identically by all four of its descriptions,
  for both fully populated and fully unset optional attributes.
- **SC-009**: An operator can configure guest egress entirely from the dashboard, with at least one
  external network offered in the gateway selection, where that selection is unconditionally empty
  today.
- **SC-010**: Each of the six defects has a recorded before-and-after result showing its test failing
  on the unchanged code and passing on the changed code.
- **SC-011**: No previously passing check regresses: the network, compute, identity, block-storage
  and image contract suites, the unit tests and the dashboard compatibility script all still pass.

## Assumptions

- **A1**: The bridge stays in the host's default namespace, because the hypervisor needs to reach it
  there; the fix for US1 therefore moves the guest device to the bridge rather than the bridge to the
  device. The alternative — a per-project bridge inside a per-project namespace — would break
  hypervisor access and is not pursued.
- **A2**: In stub mode, port creation is expected to succeed without any host networking. The
  in-memory record of simulated bridges is bookkeeping, not authority; its absence must not fail a
  request whose network is recorded.
- **A3**: Upstream Neutron's error type is the name of the condition it raised (for example the
  distinct not-found conditions for a network, a subnet and a port). Lightstack matches that naming rather
  than inventing its own vocabulary. The detail field may be empty, as it usually is upstream.
- **A4**: Error status codes are already correct throughout the network service; only the body
  changes. Any status change discovered during the work is reported, not silently applied.
- **A5**: A network with no gateway cannot currently be created through the API at all — an explicit
  request for none is overwritten with an invented address — so the vanishing-row half of US4 is
  today only reachable through a direct data write. Fixing the request handling is what makes the
  reading defect reachable, so the two halves must land together or the second is untestable.
- **A6**: Security-group rule port bounds are stored such that an unset bound and a zero bound are
  distinguishable; if they are not, making them so is part of US5.
- **A7**: External networks are visible across projects and administrator-managed — a tenant may use
  one as a gateway or floating-IP source but may not create or modify one. Marking a network external
  is an administrator-only attribute upstream, which is where this lands. The visibility half is
  FR-033a: being external qualifies a network for visibility on its own, without also being shared,
  because otherwise the tenant-facing gateway selection stays empty and F11 is only fixed for
  administrators.
- **A8**: The dashboard is not modified. Where a dialog is empty, the cause is in the data the
  network service supplies to it.
- **A9**: Where this feature and a sibling ticket touch the same behaviour, this feature owns only
  what is listed here; the boundaries in *Out of Scope* hold.
- **A10**: US3's behaviour was settled by reading upstream's own implementation rather than its API
  reference, which does not document it: the project attribute on create is honoured when the caller
  may set it — administrators and service accounts — and otherwise refused as a **bad request**, with
  the two spellings normalised into each other and a disagreement between them refused. Lightstack has no
  service-account concept, so administrator is the whole of the privileged set here.
- **A11**: US3 therefore diverges from the equivalent compute decision on this branch's history,
  which refuses cross-project creation outright — deliberately, because upstream compute has no such
  attribute to honour while upstream network does. Fidelity to each service is the rule, not internal
  symmetry between them.

## Out of Scope

- **The cross-tenant subnet-write defect (B8)** — deliberately excluded by the source ticket and
  owned by the tenant-isolation work, which has already landed. This feature must not re-open it.
- **A guest booting successfully with no addresses (B13)** — the compute-side half of the debugging
  trail described in US3; filed against the Nova ticket, not here.
- **Identity list endpoints that ignore query filters** — the same bug class as US4 but in another
  service; already owned by the tenant-scoping work. US4 covers subnets only.
- **Provider network attributes** (physical network, segmentation identifier, network type as a
  user-settable input) — external status is the only new network attribute in scope.
- **The unguarded `shared` attribute** — creating a shared network is an administrator-only action
  upstream, and Lightstack accepts it from any caller. Found while confirming US3 and US6 against upstream
  policy; it is a different attribute on the same request and is left for its own ticket rather than
  smuggled in here.
- **Multi-node overlay networking** — this feature's real-mode work is single-node.
- **Error envelope changes in any other service** — including the identity service, whose envelope
  divergence is a separate concern.

## Dependencies

- **D1**: Builds on the tenant-isolation and tenant-scoping work already on this branch's history:
  the subnet ownership-reporting rule, the cross-tenant subnet-write refusal, the shared role check
  and the caller-versus-target project resolution are all pre-existing and are reused rather than
  reinvented.
- **D2**: US1's real-mode acceptance needs a Linux host with elevated privileges; the development-mode
  half is verifiable anywhere.
- **D3**: US6's dashboard acceptance needs the dashboard stack from the existing compatibility
  harness.
- **D4**: The full test suite currently exits non-zero because of US2. Until US2 lands, other
  stories' suite runs must be assessed against that known-failing check rather than against a green
  baseline.
