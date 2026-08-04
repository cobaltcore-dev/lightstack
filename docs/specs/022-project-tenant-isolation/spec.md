# Feature Specification: Project/Tenant Lifecycle & Isolation Fixes

**Feature Branch**: `feature/545` (feature id `022-project-tenant-isolation`)
**Created**: 2026-08-04
**Status**: Draft
**Input**: GitHub issue [cobaltcore-dev/cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545) — *"o3k — project/tenant lifecycle & isolation fixes (B2, B5, B8)"*, sub-issue of [#536](https://github.com/cobaltcore-dev/cloud-storage/issues/536) (index of pre-existing defects surfaced by the Horizon compatibility campaign #531 → #532/#533/#534).

## Context

Four defects block the most basic tenant story in O3K. A tenant cannot be created with the payload
every OpenStack client sends, a tenant can write a subnet into another tenant's network, one
project-scoped URL form answers "method not allowed", and the test suite that should have caught any
of it never authenticated.

None of these is a regression from the Horizon campaign — all four pre-date it and were surfaced by
it. Every one has its answer already present in the repository: one handler in the same file shows
the broken handler what to do, and the missing route is a single line beside its 43 siblings.

| Ref | Defect | Observed | Reference implementation already in repo |
|---|---|---|---|
| **B5 / P1** | Creating a project with the domain reference every client sends fails | `500` generic server fault | The user-create handler in the same file does a real domain lookup and answers `400` |
| **B8** 🔒 | Adding a subnet never checks who owns the target network — **cross-tenant write** | Succeeds; victim's network then advertises the foreign subnet | The port-create handler validates the target network before using it |
| **B2** | The project-scoped volume-list URL is unroutable | `405` | The same collection is routable on the flat URL form |
| **T1** | Three test suites authenticated as the literal string `"null"` | Vacuous passes | ✅ **already fixed** in the #534 review — verify and build on it |

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An operator can create a tenant (Priority: P1)

An operator creating a new tenant uses the standard client command, which names the default domain
the way every OpenStack client and every piece of documentation names it. The tenant is created.
When the operator names a domain that does not exist, they are told which field was wrong — not
handed a generic server fault. When they reuse a name already taken, they are told it is taken.

**Why this priority**: Nothing else in this feature — or in the parent issue — is demonstrable until
a tenant can be created. The multi-tenancy suite cannot even reach its first assertion, because its
setup creates two tenants and then runs every check against the empty result.

**Independent Test**: Create a tenant naming the default domain the standard way; confirm it appears
in the tenant list and can be scoped to. Repeat with a nonexistent domain and with a duplicate name;
confirm each is refused with a message naming the cause.

**Acceptance Scenarios**:

1. **Given** an authenticated operator, **When** they create a project naming the default domain by
   its well-known alias, **Then** the project is created and its identifier is returned.
2. **Given** an authenticated operator, **When** they create a user naming the default domain by that
   same alias, **Then** the user is created.
3. **Given** an authenticated operator, **When** they create a project naming a domain that does not
   exist, **Then** the request is refused as a client error identifying the domain field, and no
   project is created.
4. **Given** an existing project, **When** an operator creates another project with the same name,
   **Then** the request is refused as a conflict naming the duplicate, not as a server fault.
5. **Given** an authenticated operator, **When** they create a project without naming a domain at
   all, **Then** it is placed in the default domain, exactly as before this change.
6. **Given** an authenticated operator, **When** they retrieve the default domain by its alias, or
   list domains filtered by the default domain's name, **Then** they get that one domain — so the
   client's own resolution of a `--domain` argument succeeds without relying on a fallback.

---

### User Story 2 - A tenant cannot write into another tenant's network (Priority: P1) 🔒

Two tenants share a deployment. One tenant owns a network. The other tenant attempts to attach a
subnet — an address range, a gateway, address assignment — to that network. The attempt is refused.
The owning tenant's network is unchanged and its subnet list stays empty.

**Why this priority**: This is the only security defect in the parent issue. It is a cross-tenant
**write**: the attacker chooses the address range, the gateway address and whether addresses are
handed out on someone else's network, and the victim's own views then present the foreign subnet as
theirs. It is ordered after Story 1 only because it cannot be demonstrated until tenants can be
created.

**Independent Test**: With two tenants, have tenant B attempt to add a subnet to tenant A's network;
confirm refusal, and confirm A's network still lists no subnets. Confirm B can still add a subnet to
B's own network.

**Acceptance Scenarios**:

1. **Given** a network owned by tenant A, **When** tenant B attempts to add a subnet to it, **Then**
   the request is refused and no subnet is created.
2. **Given** the refused attempt above, **When** tenant A views their network and their subnet list,
   **Then** neither shows any trace of tenant B's attempt.
3. **Given** a network owned by tenant B, **When** tenant B adds a subnet to it, **Then** it succeeds
   exactly as before this change.
4. **Given** a network identifier that names no network at all, **When** any tenant attempts to add a
   subnet to it, **Then** the request is refused as "not found" rather than surfacing as a server
   fault.
5. **Given** an administrator, **When** they add a subnet to any tenant's network, **Then** it
   succeeds (administrators are not confined to one tenant).
6. **Given** a network tenant A owns and has marked **shared**, **When** tenant B attempts to add a
   subnet to it, **Then** the request is refused as **forbidden** — sharing lets other tenants use a
   network, not define its address ranges — while a network B cannot see at all is refused as **not
   found**.

---

### User Story 3 - The project-scoped volume URL answers (Priority: P2)

A client that builds the project-scoped form of the block-storage URL — the canonical upstream form,
which carries the project in the path — lists its volumes and gets its volumes.

**Why this priority**: Independent of the other stories and safe to land at any time, but it is a
compatibility gap rather than a blocker or a security hole: the same collection is already reachable
on the flat URL form that the service catalog advertises, so most clients never notice.

**Independent Test**: List volumes on the project-scoped URL form; confirm the same collection the
flat form returns.

**Acceptance Scenarios**:

1. **Given** a tenant with volumes, **When** they list volumes on the project-scoped URL form,
   **Then** they receive their volumes — the same set the flat form returns.
2. **Given** a tenant with no volumes, **When** they list volumes on the project-scoped URL form,
   **Then** they receive an empty collection, not an error.
3. **Given** any tenant, **When** they list volumes on the project-scoped URL form naming a project
   that is not their own, **Then** they still receive only their own volumes — the path segment never
   widens what a caller can see.
4. **Given** the project-scoped URL form, **When** a client requests the detailed view of volumes or
   snapshots, or the default volume type, **Then** it receives that view — not the result of looking up
   a resource literally named "detail" or "default".

---

### User Story 4 - The isolation suite reports on something real (Priority: P1)

An engineer runs the multi-tenancy suite. It either exercises two genuinely distinct tenants and
reports on real isolation checks, or it stops with a clear failure. It never reports success while
having created nothing.

**Why this priority**: This story is what makes Stories 1–3 believable. The three suites that
authenticated as the literal string `"null"` are already fixed (#534 review), but the multi-tenancy
suite still has no guard on the *tenants and users* its setup creates — only on the token — so the
same class of vacuous pass remains one broken handler away. Fixing the harness **before** the
isolation fix is what makes Story 2 provable rather than asserted.

**Independent Test**: Run the suite against a stack where tenant creation is deliberately broken;
confirm it stops with a named failure instead of reporting passes. Then run it against a healthy
stack; confirm it exercises both tenants and passes.

**Acceptance Scenarios**:

1. **Given** a stack where the suite's setup cannot create a tenant or a user, **When** the suite
   runs, **Then** it stops with a message naming what could not be created and exits non-zero.
2. **Given** a healthy stack, **When** the suite runs, **Then** every check runs against two distinct
   real tenants and the suite passes.
3. **Given** the isolation and routing fixes have landed, **When** the suite runs, **Then** it
   includes a check that fails if a tenant can write a subnet into another tenant's network, and a
   check that fails if the project-scoped volume list is unroutable. These two checks arrive **with**
   the fixes they guard — a check asserting a refusal cannot be written before the refusal exists — so
   this scenario is satisfied by the end of the other two stories, not within this one.
4. **Given** the three suites repaired in the #534 review, **When** they are re-run, **Then** none of
   them authenticates as an empty or literal-`null` token.
5. **Given** a run that aborts part-way — at setup, or on a failed check — **When** the suite is run
   again immediately, **Then** it starts cleanly and reaches the same result, because the aborted run
   removed its own fixtures rather than leaving a name conflict behind.

---

### Edge Cases

- **Domain named by its well-known alias vs. by its identifier vs. omitted** — all three must work,
  and all three must land the resource in the same default domain.
- **Domain named but disabled** — refused as a client error, consistent with how user creation
  already treats it.
- **Domain list asked to filter** — the list ignores every query filter today and returns all domains,
  so a client's name lookup silently gets the wrong answer rather than an error. The `name` filter must
  actually filter (FR-001a).
- **Domain name in the wrong case** — `default` is the alias and resolves; `Default` is the stored
  name and resolves; a name filter for `default` does **not** match the domain named `Default`, because
  names stay case-sensitive.
- **Target network is shared** — a network its owner has marked shared grants no right to add a
  subnet (FR-010): the refusal is "forbidden", distinct from the "not found" a caller gets for a
  network they cannot see at all. Note this deliberately differs from port creation, which does admit
  shared networks.
- **Target network identifier is well-formed but names nothing** — must be "not found", not a
  server fault. Today it reaches a foreign-key violation and surfaces as `500`.
- **Target network identifier is malformed** (not an identifier at all) — a **bad request** naming the
  network field, not a server fault from a rejected type cast, and deliberately a different answer from
  the "not found" a well-formed unknown identifier gets (FR-011a).
- **Refused subnet attempt leaves no residue** — no partially created subnet, and no address
  assignment started for the victim's network.
- **Project-scoped URL naming another project** — must never widen visibility; the path segment is
  decoration, the caller's own scope decides.
- **Sub-resource names that look like identifiers** — on the project-scoped URL form, a collection
  and a by-identifier lookup are siblings, so a literal sub-resource name (e.g. a "detailed view"
  suffix) can be mistaken for a resource with that name. This is the exact failure that made
  Horizon's Create Volume button look inert (#533 F8). It bites only where a by-identifier sibling is
  registered beside the literal name — three forms here; a sub-resource with no such sibling answers an
  honest "not found" instead (FR-016, FR-016a).
- **Concurrent creation of two projects with the same name** — one wins, the loser gets a conflict,
  neither gets a server fault.

## Requirements *(mandatory)*

### Functional Requirements

#### Tenant & identity creation (B5 / P1)

- **FR-001**: The system MUST accept the well-known alias for the default domain wherever a domain
  reference is accepted in a creation request, and resolve it to the same domain that an omitted
  domain reference resolves to. The system already emits this alias in its own responses, so
  refusing it on input is self-inconsistent.
- **FR-001a**: Retrieving a domain **by** the alias MUST also work, and listing domains MUST honour a
  name filter (exact match) — it ignores every query filter today and returns all domains regardless.
  Standard clients resolve a `--domain` argument client-side *before* sending any creation request:
  they retrieve the domain by the given reference, then fall back to a name lookup. Accepting the alias
  only on creation leaves the standard command working by accident of that fallback rather than by
  design. Domain names remain **case-sensitive**, as upstream treats them.
- **FR-002**: Creating a project MUST verify that the named domain exists and is enabled before
  persisting anything, and MUST refuse an unknown or disabled domain as a **client error whose
  message names the domain field** — never as a generic server fault.
- **FR-003**: Creating a project with **no** domain reference MUST continue to place it in the
  default domain, resolved by the domain's name rather than by a hard-coded identifier, so a
  deployment whose default domain carries a different identifier still works.
- **FR-004**: Creating a project whose name is already taken MUST be refused as a **conflict** naming
  the duplicate, consistent with how user creation already reports the same collision.
- **FR-005**: Creating a **user** MUST accept the same default-domain alias as FR-001 and otherwise
  keep its existing behaviour unchanged, including its existing administrator-only gate.
- **FR-006**: Creating a **group** MUST resolve and validate its domain reference the same way — it
  today shares the project handler's unvalidated hard-coded default and therefore its identical
  failure mode. (See Assumption A3.)
- **FR-007**: Responses MUST keep reporting the domain identifier in the form they report it today;
  accepting the alias on input MUST NOT change what is returned. (See Assumption A2.)
- **FR-008**: The domain-reference resolution MUST exist as a **single shared behaviour** used by
  every path that accepts one — the three creation paths and the retrieval path of FR-001a — so they
  cannot disagree about what a valid domain is, mirroring how project references in URL paths are
  already resolved once and shared.

#### Cross-tenant subnet write (B8) 🔒

- **FR-009**: Adding a subnet MUST decide the caller's entitlement to the **target network before any
  side effect** — before the subnet exists, and before any address-range or address-assignment work
  begins. This is the ordering requirement; FR-010 states the rule being applied.
- **FR-010**: Only the network's **owner** or an **administrator** is entitled to add a subnet to it.
  A network its owner has marked *shared* grants no such entitlement — sharing lets other tenants
  *use* a network, not define its address ranges. (Decision Q1; this deliberately differs from the
  port-create rule, which does admit shared networks — see Assumption A5.)
- **FR-010a**: A refusal MUST distinguish what the caller can see from what they may do: a network the
  caller cannot see at all is reported as **not found**, and a shared network they can legitimately
  see but do not own is reported as **forbidden**. (Decision Q1.)
- **FR-011**: A target network identifier that is **well-formed but names no network** MUST be refused
  as **not found**, not as a server fault from a foreign-key violation.
- **FR-011a**: A **malformed** target network identifier MUST be refused as a **bad request naming the
  network field**, not as a server fault from a rejected type cast. It is a body attribute, not a path
  segment, so the "not found" rule #515 established for malformed path ids does not apply here; this
  matches both upstream and how subnet creation already rejects a malformed address range.
- **FR-011b**: A refused cross-tenant subnet write MUST be **recorded as a warning** identifying the
  calling project, the target network and the network's owner, so an operator can find the attempt
  afterwards and tell a misconfigured client from a probe. A legitimate subnet creation MUST NOT add
  any such record.
- **FR-012**: An administrator MUST remain able to add a subnet to any tenant's network.
- **FR-013**: A tenant adding a subnet to their **own** network MUST see no change in behaviour —
  same result, same response shape, same address-assignment side effects.
- **FR-013a**: The owner of a network MUST be able to **retrieve, rename and delete** a subnet attached
  to that network even when the subnet belongs to another project. FR-012 makes that state reachable —
  an administrator may add a subnet to any network, and the row is attributed to the *caller's*
  project (upstream `populate_project_id` copies `context.project_id`, so the subnet and its network
  can legitimately have different owners). Without this the three subnet paths disagree: the listing
  shows the subnet (it already matches on the network's project), retrieval by id answers not found,
  and deletion refuses — an address range on the owner's own network that they can see, cannot
  inspect, and cannot remove. Upstream grants the network owner all three (`get_subnet` includes
  `ADMIN_OR_NET_OWNER_READER`; `update_subnet` and `delete_subnet` include `ADMIN_OR_NET_OWNER_MEMBER`;
  `network_owner` is `project_id:%(network:project_id)s`), so O3K was **stricter than upstream** on
  exactly these three paths. Rename is included because it is strictly weaker than removal — a scope
  that permits the delete and refuses the rename would be an arbitrary line. A **shared** network
  grants read only — upstream puts `rule:shared` in `get_subnet` and deliberately not in the two
  mutating rules — and a caller who owns neither the subnet nor its network still sees nothing.
  Retrieval, listing and rename MUST report the subnet's **own** project in `tenant_id`, not the
  caller's — on every path that can return a subnet the caller does not own. (See Assumption A5a.)

#### Project-scoped volume list (B2)

- **FR-014**: Listing volumes on the project-scoped URL form MUST return the caller's volumes — the
  same collection the flat URL form returns for the same caller.
- **FR-015**: The project segment in that URL MUST NOT widen what a caller can see; the caller's own
  scope decides, exactly as on the flat form.
- **FR-016**: The project-scoped URL form MUST NOT leave a collection reachable-but-wrong: a literal
  sub-resource name must not be mistaken for a resource of that name. In scope are the ticketed volume
  list **plus the four sub-resources a sibling by-identifier lookup actually swallows** — the detailed
  views of volumes and snapshots, and the default volume type. Each has a by-identifier sibling
  registered beside it, so the literal name matches that sibling and the request answers with the
  *wrong* thing rather than an honest "not found" — the exact failure that made Horizon's Create Volume
  button look inert (#533 F8). (Decision Q2.)
- **FR-016a**: The remaining 14 absent routes on the project-scoped URL form are **out of scope**:
  they are merely missing, not wrong, and closing them would add routes with no coverage beyond a
  parity guard. This includes the detailed view of **backups**, which has no by-identifier sibling in
  that group — no backup route is registered there at all — so it already answers an honest "not found"
  and is absent rather than shadowed. They MUST be recorded as a known gap so a later ticket can close
  them deliberately. (Decision Q2.)

#### Verification harness (T1 and its siblings)

- **FR-017**: The multi-tenancy suite's setup MUST **fail loudly** when it cannot create a tenant, a
  user, a role assignment or a scoped token — no check may run, and no result may be reported, against
  a tenant that was never created.
- **FR-017a**: The multi-tenancy suite MUST be **re-runnable**: its teardown MUST run however the suite
  exits — pass, failed check, or aborted setup — so a run that dies part-way leaves no fixtures behind.
  Without this, FR-004 and FR-017 combine into a new failure mode: the suite creates its tenants under
  fixed names, so one crashed run would make every later run abort at setup with a name conflict.
- **FR-018**: The multi-tenancy suite MUST gain a check that **fails** if one tenant can add a subnet
  to another tenant's network, and MUST confirm the victim's network and subnet views stay clean.
- **FR-019**: The multi-tenancy suite MUST gain a check that exercises the project-scoped volume
  **list** URL form.
- **FR-020**: The three suites repaired in the #534 review MUST be re-verified as extracting a real
  token, so this feature builds on a harness that authenticates.
- **FR-021**: Each fix MUST be demonstrated **failing before and passing after** — a static or live
  check per defect, so no fix is accepted on assertion alone.
- **FR-022**: Automated tests MUST cover the routing shape (the project-scoped collection is
  reachable and its sibling sub-resources are not shadowed) and the entitlement decision for the
  subnet write, so neither can silently regress.

#### Non-goals

- **FR-023**: This feature MUST NOT change the tenant-scoping or authorization model beyond the two
  defects above and the subnet-read/delete parity FR-013a requires to make the first of them
  coherent. FR-013a is in scope precisely because it is not a policy decision: it restores upstream's
  own rule on two paths where O3K is stricter, and closes a state FR-012 makes reachable (see
  Assumption A5a). The broad owner-or-administrator sweep across the compute handlers and
  domain-scoped tokens were deliberately split out to #549 and are out of scope. The
  administrator-only gate that project creation lacks (user creation has one) is noted here as a
  finding and left to that ticket. (See Assumption A6.)

### Key Entities

- **Domain**: The top-level identity container. Holds a name and an identifier, and can be enabled or
  disabled. Exactly one is the deployment default. It is referenced by a well-known alias, by its
  identifier, or by omission — and all three must name the same thing.
- **Project (tenant)**: The unit of resource ownership and isolation. Has a unique name and belongs
  to a domain. Every resource in the system is scoped to one.
- **Network**: Owned by exactly one project, and may be marked shared. Owning it is what should
  entitle a caller to attach subnets to it.
- **Subnet**: An address range, gateway and address-assignment policy attached to a network. It is
  created with the *caller's* project as owner but attached to a network the caller may not own —
  which is precisely the defect.
- **Volume**: Block storage owned by one project, reachable on two URL forms that must agree.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An operator can create a tenant with the standard client command naming the default
  domain the standard way — the command that fails today — on the first attempt, with no
  workaround and no knowledge of internal identifiers.
- **SC-001a**: The client's own resolution of a `--domain` argument succeeds on its **first** attempt —
  retrieving the default domain by its alias returns that domain, so the standard command no longer
  depends on a fallback after a failed lookup.
- **SC-002**: Naming a domain that does not exist produces a message that identifies the domain as
  the problem. Across an enumerated set of malformed and rejected creation payloads — unknown domain,
  disabled domain, non-identifier domain reference, duplicate name, missing required field — the
  tenant-creation path returns a **client** error every time and a generic server fault **zero** times.
- **SC-003**: A tenant attempting to attach a subnet to another tenant's network fails **100%** of
  the time, and the target tenant's own views of that network show **zero** trace of the attempt.
- **SC-004**: A tenant attaching a subnet to their own network, and an administrator attaching one to
  any network, both still succeed 100% of the time — the fix costs no legitimate capability.
- **SC-004a**: A tenant is refused a subnet on a **shared** network they do not own, and the refusal
  reads as "forbidden" rather than "not found" — the caller learns they may not, not that it is absent.
- **SC-004b**: Every refused cross-tenant subnet write leaves exactly **one** warning record naming the
  caller, the target network and its owner; a legitimate creation leaves **none**.
- **SC-005**: Both URL forms for listing a tenant's volumes return the **same** collection for the
  same caller, and neither rejects the request as an unsupported operation.
- **SC-005a**: On the project-scoped URL form, all four shadowed sub-resource views (detailed volumes,
  detailed snapshots, default volume type) return that view — **zero** of them return the result of a
  lookup for a resource named after the sub-resource.
- **SC-006**: The multi-tenancy suite runs end to end against two genuinely distinct tenants and
  passes; when its setup is deliberately broken it exits non-zero with a message naming what failed,
  and reports **zero** passes.
- **SC-006a**: The multi-tenancy suite gives the same result on two consecutive runs, and on a run
  immediately following an aborted one — **zero** manual cleanup between runs.
- **SC-007**: Every defect in scope has a recorded check that demonstrably **failed before** the fix
  and **passes after** — four for four, no exceptions.
- **SC-008**: No previously passing test or suite regresses. Specifically green before the work is
  considered done: the unit suite, the linter, the integration suite, the multi-tenancy suite and the
  identity contract suite. `test/horizon_compat_test.sh` currently **fails** two checks on the
  project-scoped volume paths this feature repairs, so it must go from failing to passing rather than
  merely staying as it was.

## Assumptions

- **A1**: The default domain is referenced by the alias `default` because upstream Keystone's Default
  domain literally carries that as its identifier — which is why every client and every document
  sends it, and why O3K's own token and signature-validation responses already emit it. The alias
  cannot be made a real stored identifier: the identity identifier columns are typed as UUIDs, so
  resolving the alias is the only available fix, not one design among several.
- **A2**: Responses continue to report the stored domain identifier rather than the alias. The
  contract suite asserts the stored identifier in five places; switching the response form would be a
  separate, larger compatibility decision. The alias is something requests may **use** — on creation
  (FR-001) and on retrieval (FR-001a) — never something responses **return**.
- **A3**: Group creation is included (FR-006) because it carries a byte-for-byte copy of the project
  handler's unvalidated hard-coded default and therefore its identical `500`. Once the resolution is a
  single shared behaviour (FR-008), including it costs one line; excluding it would leave the shared
  behaviour deliberately unused at one of its three call sites. Role creation is **not** included: it
  parses a domain reference and discards it, so it has no failure mode to fix.
- **A4**: Refusing a cross-tenant subnet write is a **behaviour change** for any caller relying on it
  today. It is treated as a security fix, not a compatibility break: no legitimate client depends on
  writing into another tenant's network.
- **A5**: The subnet-write entitlement check follows the *shape* already used by port creation —
  resolve the target network once, refuse if it does not resolve — so the two paths stay recognisably
  the same. The *rule* deliberately differs: port creation admits shared networks, subnet creation does
  not (FR-010, decision Q1). Because two neighbouring handlers will read the same but decide
  differently, the difference must be stated in the code, not left to be rediscovered.
- **A5a**: Widening subnet retrieval and deletion to the network's owner (FR-013a) is **upstream
  parity, not a new policy**, which is why it belongs with B8 rather than with #549's authorization
  model. The rule was read from `neutron/conf/policies/subnet.py` and `base.py`, not inferred: O3K's
  own `ListSubnets` already applies it (SPEC-020 added `n.project_id = $1` for a Horizon reason), so
  the change makes four O3K paths agree with each other *and* with upstream. Mutation is the genuine
  widening — a caller may now rename or remove a row belonging to another project — so it is bounded
  exactly as upstream bounds it: network owners only, no `shared` disjunct on the mutating paths, no
  administrator bypass (that bypass is #549's sweep, and adding it here would widen further than the
  defect). Both mutating handlers had the same latent trap: their entitlement lookup and their
  `UPDATE`/`DELETE` each carried `project_id`, so widening only the lookup would have answered
  `200`/`204` while changing nothing. The write now targets the id the entitlement query resolved.
- **A6**: Project creation is not administrator-gated today, while user creation is. That is a real
  divergence from upstream, but it is a policy decision of the kind the parent issue explicitly moved
  to #549, so it is recorded (FR-023) and not changed here.
- **A7**: The three suites named in T1 are already repaired (#534 review, commit `1a9e4bd`); this
  feature verifies rather than re-fixes them, and adds the missing setup guards that T1 did not cover.
- **A8**: Stub mode is sufficient to demonstrate all four defects and all four fixes. Real mode is
  not required and not exercised.
- **A9**: No schema change and no migration are needed. Every fix is behavioural: a lookup, a
  predicate, a route and test coverage.
- **A10**: The four sub-resources a sibling by-identifier lookup swallows are **pre-existing** defects,
  independent of this feature's route addition — adding the volume list neither causes nor worsens them.
  They are in scope by decision Q2 because they answer wrongly rather than absently, which is the
  failure mode that has already cost this project a debugging cycle (#533 F8). Which sub-resources
  qualify was verified against the route group rather than inferred from the flat/project-scoped
  difference: a name is only swallowed where a by-identifier sibling is registered beside it.

- **A11**: FR-001a requires a **narrow exception** to the rule #515 established — that a non-identifier
  path id is answered "not found" before the handler runs. The exception is the domain-retrieval route
  only, and only for the one well-known alias: every other non-identifier value on that route still
  gets #515's answer. The rule was introduced to stop malformed ids reaching an identifier-typed column
  as a server fault, and resolving a known alias to a real identifier serves that same purpose rather
  than undermining it.

## Dependencies

- **Blocks**: #549 (*tenant scoping & authorization model*) depends on this feature's harness work —
  until the multi-tenancy suite genuinely authenticates and genuinely creates tenants, neither
  ticket's isolation claims can be proven.
- **Blocked by**: nothing. This is the first sub-issue of #536 and deliberately self-contained.
- **Internal ordering** (a real chain, per the ticket): tenant creation (Story 1) → harness
  (Story 4) → cross-tenant write (Story 2). The volume route (Story 3) is independent of all three.
- **Environment**: the stub stack described in #536's shared setup, plus the standard OpenStack
  command-line client.

## Out of Scope

- The owner-or-administrator sweep across the compute handlers, and domain-scoped tokens — both moved
  to #549.
- Adding an administrator-only gate to project creation (recorded in FR-023 / A6, decided in #549).
- The remaining sub-issues of #536: #546 (network defects), #547 (service discovery and compute
  gaps), #548 (test tooling).
- Returning the domain alias in responses instead of the stored identifier (A2).
- Query filters on the domain list other than `name` (FR-001a adds only the one clients use to resolve
  a `--domain` argument).
- Case-insensitive domain names — upstream treats them as case-sensitive and so does this feature
  (FR-001a).
- Any real-mode behaviour, schema change or migration.

## Clarifications

### Session 2026-08-04

- Q: Should the `default` domain alias also resolve on the read path, or POST paths only? → A: POST paths **plus** `GET /v3/domains/default` and `?name=` filtering on the domain list (exact match) — the alias must not depend on a client-side fallback surviving
- Q: FR-004 makes a duplicate project name a conflict and FR-017 exits setup early, so a crashed run's leftover fixtures would block every later run — how does the suite stay re-runnable? → A: Cleanup on exit via a trap registered before the first resource is created, so teardown runs however the suite exits
- Q: FR-011 said a malformed `network_id` on subnet create must be "a client error" — which one? → A: Bad request naming the network field (it is a body attribute, so #515's not-found rule for malformed path ids does not apply); well-formed-but-unknown stays not found
- Q: Should a refused cross-tenant subnet write be logged as a security event? → A: Yes — at warn level, carrying the caller's project, the target network and its owner

### Prior decisions (specification phase, 2026-08-04)

Both decisions open at the end of specification were settled the same day. Their answers are folded
into FR-010/FR-010a and FR-016/FR-016a above; the questions and rejected options are kept here as the
record of why.

#### Q1 — Shared networks and the refusal code (FR-010) → **resolved: owner or administrator only; 404 for unseen, 403 for shared**

**Context**: The ticket says a cross-tenant subnet write should return "`404`/`403`" — it does not
choose. Separately, the in-repo reference implementation (port creation) admits shared networks
(*the caller's own network OR any shared network*), while upstream policy for subnet creation is
owner-or-administrator only, which excludes shared.

**What we need to know**: On a network another tenant owns and has marked **shared** — may the caller
add a subnet? And how is a refusal reported?

| Option | Answer | Implications | Verdict |
|--------|--------|--------------|---------|
| A      | **Owner or administrator only.** Shared grants nothing. Refuse a network the caller cannot see with "not found"; refuse a shared network they can see but do not own with "forbidden" | Matches upstream policy exactly; strictest and most defensible for a security fix. Diverges from the sibling port-create rule, so the two paths differ by intent and need a comment saying why | ✅ **chosen** |
| B      | **Owner, administrator, or any shared network.** Refuse with "not found" | One rule, copied verbatim from port creation — simplest and most consistent internally. But it leaves a narrower version of the same cross-tenant write open: any tenant can define the address range on any shared network | rejected — reopens the defect in narrower form |
| C      | **Owner or administrator only, refusing everything with "not found"** | Never discloses that a network exists, which is the safer information posture. Slightly less informative than upstream for the shared case | rejected — a shared network is already visible to the caller, so "not found" would be a lie they can detect |

#### Q2 — Scope of the project-scoped URL repair (FR-016) → **resolved: the ticketed route plus the shadowed sub-resources**

**Context**: The ticket scopes B2 to one missing route. But the project-scoped route group is missing
**19** of the flat form's routes, and some of those names are swallowed by a sibling by-identifier
lookup rather than answering "not found" — the exact failure that made
Horizon's Create Volume button look inert (#533 F8). The code comment beside that group argues for
consistency in so many words: *"Leaving the new resources out of the group would reproduce exactly
that class of bug."*

**What we need to know**: How wide is this fix?

| Option | Answer | Implications | Verdict |
|--------|--------|--------------|---------|
| A      | **The ticketed route only** — the volume list | Smallest possible diff, exactly what the ticket asks. Leaves 18 known gaps of the same class for a future ticket, and leaves the shadowed sub-resources answering wrongly | rejected — leaves three routes returning the wrong thing |
| B      | **The ticketed route plus the shadowed sub-resources** ("detailed view" and "default" forms) | Closes the routes that are actively *wrong* rather than merely absent — the ones that answer with the wrong thing instead of an honest "not found". Moderate diff. Planning established that `backups/detail` has no by-identifier sibling in that group, so it is absent rather than shadowed; the o3k#82 review then found a **fourth** that does have one, `os-volume-transfer/detail`, bringing the in-scope set to four | ✅ **chosen** |
| C      | **Full parity, enforced by a test** — every route on the flat form has a project-scoped twin, guarded by a test that fails when the two sets diverge | Closes all 19 and makes the class of bug impossible to reintroduce, which is what the existing code comment intends. Largest diff, and the added routes are untested beyond the parity guard | rejected for now — the 14 merely-absent routes become a recorded gap (FR-016a) |
