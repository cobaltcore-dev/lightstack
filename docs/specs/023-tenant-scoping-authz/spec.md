# Feature Specification: Tenant Scoping & Authorization Model

**Feature Branch**: `023-tenant-scoping-authz`  
**Created**: 2026-08-10  
**Updated**: 2026-08-10 — re-grounded after rebase onto `feature/545` @ `9992ebf`; clarified (7 decisions)  
**Status**: Draft  
**Input**: GitHub issue [cobaltcore-dev/cloud-storage#549](https://github.com/cobaltcore-dev/cloud-storage/issues/549) — "o3k — tenant scoping & authorization model (P2, P3)". Sub-issue of #536, split out of #545 because these two items are capabilities with policy decisions in them rather than surgical fixes. #545's own specification confirms the boundary: its FR-023 names "the broad owner-or-administrator sweep across the compute handlers and domain-scoped tokens" as deliberately out of its scope and split out to this feature.

## Overview

Lightstack answers two questions inconsistently: **what does "scope" mean**, and **who may act across it?**

The administrative override is no longer a missing concept — the rebase onto #545 brings a shared role helper and three separate services that already implement owner-or-administrator. What is missing is its consistent application. Compute's core server operations are the outlier: an operator can open any tenant's console, delete any tenant's image, and add a subnet to any tenant's network, but cannot list, show, or delete that tenant's server. Two compute operations that are gated to administrators *only* — evacuate and reset-state — then restrict themselves to the administrator's own project, which makes them unusable for the situations they exist to handle.

Separately, a caller who asks for authority over a whole domain is told "yes" and handed a token that carries no authority at all. And in the one place the boundary is missing altogether, usage reporting hands any tenant every other tenant's consumption.

Both halves are one vocabulary: project scope, domain scope, and the administrative override are the same question asked three ways. This feature settles that vocabulary and finishes applying it.

## Clarifications

### Session 2026-08-10

- Q: Should a domain-scoped authentication request be honoured, or refused? → A: Refused with a bad request naming the unsupported scope. No domain-scoped token is issued and no domain-level authorization layer is built — role assignments bind to projects only, so there is no domain-level grant to read, and no supported client needs one.
- Q: How far does the administrative override reach into compute? → A: Every server operation **except creation**, matching upstream Nova's `admin_or_owner` default. An administrator may read, delete, stop/start/reboot, resize, rebuild, write metadata and tags, attach and detach volumes and interfaces, migrate, evacuate and reset-state another project's server, but may not create a server into another project.
- Q: Do cross-tenant listings widen implicitly for administrators, or on explicit request? → A: Explicit opt-in only, matching upstream's `all_tenants` administrator option (verified against the Nova API reference). Bare use with no value means true; a non-administrator passing it is silently ignored rather than refused. Upstream's companion `project_id`/`tenant_id` narrowing filter is deferred — it is additive and no client needs it yet.
- Q: How is a cross-tenant lookup by name resolved, given the same name can exist in two projects? → A: It is not. Name matching stays scoped to the caller's own project; addressing another project's server requires its identifier. Upstream accepts only a UUID on these paths (verified against the Nova API reference) and resolves names client-side, so Lightstack's name matching is a local extension that must not be carried into a context where it can select the wrong tenant's server.
- Q: Tenant usage reporting currently exposes every project's usage to any authenticated caller. Fix here, or defer? → A: Fix here. It is the same decision this feature is already making, on an endpoint already broken in the direction the feature is fixing; deferring would ship a rule contradicted by a live endpoint. The aggregate form becomes administrator-only and the per-project form is scoped through the existing shared resolver.
- Q: When an operator acts across projects, whose project is recorded, and is a refused cross-tenant attempt logged? → A: The **owning** project is recorded, with the acting user identifying who did it — so the operation appears in the log the affected tenant can actually read. No new field and no migration. Refused cross-tenant compute attempts are logged as warnings naming the parties, matching the precedent #545 set for network writes. Consequence made explicit: only creation is written to the action record today, so the write path must be extended to the operations the override now covers, or the tenant-visible log this choice exists for stays empty.
- Q: Planning found two more ungated administrative surfaces — the aggregate migration list and host aggregates. In scope? → A: **In scope.** Both are the same rule this feature already applies: an administrative surface requires the administrative role. Host aggregates are the sharper of the two — nine handlers with no role check at all, six of them writes — making it the only privilege-escalation defect in the feature rather than another read leak.
- Q: Is the identity list-filter bug in scope, and is it covered by a sibling ticket? → A: In scope, and no sibling ticket covers it — #546, #547 and #548 were checked. #546's B6 is the same bug class (`ListSubnets` ignores its `network_id` filter) but is owned there, so this feature covers the **identity** collections only. The audit found six that ignore every documented filter; #545 already fixed the seventh (`ListDomains`), which supplies the shape to copy.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An operator administers any tenant's compute resources (Priority: P1)

A cloud operator holds the `admin` role. A tenant raises a support ticket: their virtual machine is stuck and unreachable. The operator authenticates and needs to look at that machine — list it, read its detail and diagnostics, and if the tenant asks, delete it — without first being granted membership of that tenant's project, and without going to the database.

Today the operator can open that machine's console and read its boot log, because the console was given the override explicitly. Every other compute operation answers as though the machine does not exist. The operator's remaining options are to self-grant a role on the tenant's project — which changes the tenant's own role list and is visible to them — or to edit the database directly.

**Why this priority**: It is the defect with the widest surface, it forces operators into workarounds worse than the problem, and it is now an internal inconsistency rather than a missing capability: the same product grants this override in three other places. It also contains a strictly-broken subcase (Acceptance Scenario 7) that no policy decision is needed to fix.

**Independent Test**: Create two projects and two non-administrative users, one per project. Create a virtual machine as project B's member. Authenticate as an operator holding `admin`, scoped to project A. Exercise the compute operations against project B's machine and confirm each is answered rather than reported missing. Repeat every step as project A's non-administrative member and confirm each is still refused, and still refused in a way that does not reveal the machine exists.

**Acceptance Scenarios**:

1. **Given** an operator with the `admin` role scoped to project A and a virtual machine owned by project B, **When** the operator requests that machine's detail, **Then** the machine's detail is returned rather than a "not found" answer.
2. **Given** the same operator, **When** the operator lists virtual machines without asking for a cross-tenant view, **Then** only project A's machines are listed — the default view is unchanged and existing clients see no difference.
3. **Given** the same operator, **When** the operator explicitly asks for a cross-tenant view of the machine list — including in the bare, valueless form clients send — **Then** machines from every project are listed, each identifying its owning project.
4. **Given** a non-administrative member of project A and a virtual machine owned by project B, **When** that member requests the machine's detail, by identifier or by name, **Then** the request is refused exactly as it is today, and the answer does not distinguish "does not exist" from "belongs to someone else".
5. **Given** a non-administrative member of project A, **When** that member explicitly asks for a cross-tenant view of the machine list, **Then** the parameter is silently ignored and only project A's machines are returned — not refused, which would reveal that the parameter exists and is gated.
6. **Given** an operator with the `admin` role, **When** the operator deletes, stops, reboots, resizes, rebuilds, re-tags, or attaches a volume to another project's machine, **Then** the operation succeeds and is recorded in a way that identifies which user performed it.
6a. **Given** an operator with the `admin` role, **When** the operator attempts to create a machine into another project, **Then** the request is refused — creation is the one server operation the override does not cover.
6b. **Given** an operator with the `admin` role and two projects that each own a machine named `web-1`, **When** the operator addresses `web-1` by name, **Then** only their own project's machine is considered — neither the other tenant's machine nor an arbitrary choice between the two. Addressing the other tenant's machine requires its identifier.
7. **Given** an operator with the `admin` role and a machine owned by another project, **When** the operator evacuates it from a failed host or resets its state, **Then** the operation succeeds — today both are refused as "not found" despite being gated to administrators only, which leaves them unusable for the exact situations they exist to handle.

---

### User Story 2 - A domain-scoped authentication request gets an honest answer (Priority: P2)

An integrator points a client at Lightstack and asks to authenticate with authority over a whole domain rather than a single project — the standard way to reach identity operations that are not owned by any one project. The request succeeds with a success status, so the client proceeds. Every subsequent call then fails in a confusing way, because the token it was handed carries no scope and no roles at all: the request for domain authority was silently discarded.

The integrator needs the request answered truthfully — refused, with a message naming what is unsupported — so that the failure surfaces at the point of authentication instead of as a chain of unexplained refusals later. Lightstack has no domain-level authority to grant, so the honest answer is "no", stated once, at the door.

**Why this priority**: Smaller in surface than User Story 1, but the more dangerous failure mode: the caller cannot tell it was ignored. A client believing it holds domain-wide authority, holding a role-less token, is worse than a client that was refused. It ranks below User Story 1 only because it is reached by fewer callers today, and because User Story 1 contains a subcase that is broken outright.

**Independent Test**: Send an authentication request scoped to a domain and inspect the answer. It must be a bad request naming the domain scope as unsupported, identical for every domain named, and distinguishable from a bad-credential refusal. Then send a project-scoped request whose domain appears as a qualifier of the project, and confirm it still succeeds.

**Acceptance Scenarios**:

1. **Given** a valid user credential, **When** the caller requests authentication scoped to a domain, **Then** the response is a bad request naming the domain scope as unsupported — never a success that omits both the scope asked for and any roles.
2. **Given** that refusal, **When** the caller inspects it, **Then** it is distinguishable from a bad-credential refusal, so the caller can tell its request was rejected for what it asked rather than for who it is.
3. **Given** any domain reference at all — one that exists, one that does not, one the caller holds no role in, and one that is not a well-formed identifier — **When** it is used as a domain scope, **Then** the refusal is identical in every case, so the answer discloses nothing about which domains exist.
4. **Given** a project-scoped request that names its domain as a **qualifier of the project** (the existing, supported form), **When** it is sent, **Then** it still succeeds exactly as it does today — the new refusal must not catch the nested domain qualifier.
5. **Given** an existing project-scoped or explicitly unscoped authentication request, **When** it is sent unchanged, **Then** its answer is what it is today — no existing client's authentication changes.
6. **Given** a scope kind Lightstack does not support at all, **When** a caller requests it, **Then** the response is a refusal naming it, not a silently degraded success.
7. **Given** a request naming both a project scope and a domain scope, **When** it is sent, **Then** it is refused as contradictory rather than resolved by precedence.

---

### User Story 3 - A tenant cannot read another tenant's resource usage (Priority: P2)

A tenant queries their own compute usage — instance count, vCPU, memory and disk hours — to check what they are consuming. The same endpoint, asked slightly differently, hands them **every** project's usage broken down by project, and asked with another project's identifier, hands them that project's usage specifically. Neither form checks who is asking. A tenant can therefore enumerate how large every other tenant on the deployment is, and size up any named one.

An operator needs the aggregate view, because seeing usage across all projects is what the endpoint is for. A tenant needs their own. Today both get both.

**Why this priority**: It is the only confidentiality defect in this feature — the other two stories are about capability that is missing or dishonest, this one is about information that leaks. It shares User Story 1's priority band because it is the same rule applied to the same kind of endpoint, and because the fix is small: the aggregate form gains a role check and the per-project form adopts a resolver that already exists.

**Independent Test**: As a non-administrative member of project A, request the aggregate usage view and confirm it is refused. Then request project B's usage by identifier and confirm the answer describes project A, or is refused — never project B. Repeat both as an administrator and confirm both succeed.

**Acceptance Scenarios**:

1. **Given** a non-administrative member of project A, **When** they request the aggregate usage view covering all projects, **Then** the request is refused.
2. **Given** a non-administrative member of project A, **When** they request usage for project B by identifier, **Then** the answer does not describe project B.
3. **Given** an operator holding the administrative role, **When** they request the aggregate usage view, **Then** usage for every project is returned.
4. **Given** an operator holding the administrative role, **When** they request usage for any single project by identifier, **Then** that project's usage is returned.
5. **Given** any caller, **When** they request usage for an identifier that is malformed or names no project, **Then** the answer is a client error naming the problem — not a success reporting zeroes, which is indistinguishable from a real project that has no instances.

---

### User Story 4 - Isolation results that also cover the override (Priority: P3)

A reviewer runs the multi-tenancy test suite before a release. Thanks to #545 the suite now provisions genuinely non-administrative users and refuses to run if either test token carries the administrative role, so its isolation results are trustworthy. What it does not do is exercise the override at all: all six of its checks assert that tenants cannot reach each other, and none asserts that an operator can. Once User Story 1 ships, a regression that quietly re-narrows an operator to their own project would leave every check green.

The reviewer needs the suite to cover both directions — the boundary holding for tenants, and the override working for operators — and needs at least one check that demonstrably fails when project scoping is removed, so a green result is evidence rather than assumption.

**Why this priority**: It changes no product behaviour, so it is last. It is nonetheless what makes the other three stories believable, and it is now much smaller than it was before the rebase: #545 already built the non-administrative fixtures and the fail-loudly setup this story used to have to specify.

**Independent Test**: Inspect the suite for a check that authenticates as an operator and reaches another project's resource. Then remove a project-scoping condition from a resource lookup and confirm the suite turns red.

**Acceptance Scenarios**:

1. **Given** the multi-tenancy suite, **When** it runs, **Then** it contains at least one check that authenticates as an operator holding the administrative role and successfully reaches a resource owned by another project.
2. **Given** the suite run against a build with a project-scoping condition deliberately removed, **When** the suite runs, **Then** it reports failure.
3. **Given** the suite, **When** it runs, **Then** the guarantees #545 established still hold: no identity used to assert isolation holds an administrative role, a role that cannot be resolved by name fails the run loudly, and teardown runs however the suite exits.
4. **Given** the suite, **When** it is run twice in a row against the same deployment, **Then** the second run behaves identically to the first.

---

### User Story 5 - Filtering an identity collection actually filters (Priority: P3)

A client asks for one role by name and receives every role. Because the answer is a well-formed list rather than an error, nothing signals that the filter was discarded — so a caller that takes the first entry silently gets the wrong one. This is half of how the multi-tenancy suite came to grant the administrative role to both of its test users for as long as it did: it asked for the member role by name, got all three, and took the first.

The remaining workaround for that is client-side filtering, which only helps callers who know the filter is broken.

**Why this priority**: No confidentiality or capability consequence, and #545 already removed the sharp edge from the one place it had bitten. It is in this feature because the same class of silent-wrong-answer is what the rest of the feature is about, because no sibling ticket covers the identity collections, and because #545 already established the fix shape on one of them.

**Independent Test**: For each identity collection, request it filtered by name and confirm the result contains only matches. Confirm an unfiltered request is unchanged.

**Acceptance Scenarios**:

1. **Given** more than one role exists, **When** a client requests roles filtered by name, **Then** only roles matching that name are returned.
2. **Given** any identity collection that upstream documents a filter for, **When** a client sends that filter, **Then** it is applied rather than discarded.
3. **Given** a filter naming something that does not exist, **When** it is sent, **Then** an empty collection is returned — not the full collection, and not an error.
4. **Given** a request with no filter at all, **When** it is sent, **Then** the answer is exactly what it is today.

---

### User Story 6 - Operator-only surfaces reject tenants (Priority: P1)

Some compute surfaces are operator-only by nature: they describe or reshape the deployment rather than anything a tenant owns. Host aggregates group hypervisors for scheduling; the migration list reports every movement of every instance across the whole deployment. Neither is scoped to a project, because neither belongs to one.

Lightstack exposes both to any authenticated tenant. The migration list hands over every project's `server_uuid`, source and destination node and flavour changes — its own source comment notes it is "an admin endpoint in real OpenStack". Host aggregates are worse: nine handlers with no role check whatsoever, six of them writes. A tenant can create aggregates, rename them, add and remove hypervisors, rewrite their metadata, and delete them.

An operator needs both. A tenant needs neither, and must not have either.

**Why this priority**: This is the only place in the feature where a tenant can *change* something that is not theirs, rather than read it. Every other defect here is a missing capability, a dishonest answer, or a read that leaks — this one is a tenant performing operator work on the deployment. The practical blast radius is bounded by Lightstack's single-node deployment model, where aggregates influence little; the authorization gap is not bounded by anything.

**Independent Test**: As a non-administrative tenant, attempt each aggregate operation and the aggregate migration listing. Every one must be refused. Repeat as an operator and confirm each succeeds.

**Acceptance Scenarios**:

1. **Given** a non-administrative member of project A, **When** they request the deployment-wide migration list, **Then** the request is refused — today it returns every project's migration records.
2. **Given** a non-administrative member of project A, **When** they create, rename, delete, or modify the metadata of a host aggregate, **Then** each request is refused.
3. **Given** a non-administrative member of project A, **When** they add a hypervisor to an aggregate or remove one from it, **Then** the request is refused.
4. **Given** a non-administrative member of project A, **When** they list or show host aggregates, **Then** the request is refused — aggregate topology describes the deployment, not their project.
5. **Given** an operator holding the administrative role, **When** they perform any of the above, **Then** it succeeds exactly as it does today.
6. **Given** a non-administrative member of project A and a server owned by project B, **When** they list, show, delete, or act on that server's migration records, **Then** the request is refused — today no ownership check exists on any of them.

---

### Edge Cases

- An operator holds `admin` in one project and no role at all in the project owning the resource — the override must key off the role the token carries, not off membership of the target project.
- A resource belongs to no project at all — a host aggregate, a deployment-wide migration listing. The owner-or-administrator rule has no owner to fall back on, so these are administrator-only rather than overridable, and the distinction must be deliberate rather than incidental.
- A resource is addressed by name rather than identifier, and the same name exists in two projects. Name matching never spans projects (FR-005c), so the ambiguity cannot arise — but the boundary must hold on every path that accepts a name, including the delete and action paths where the consequence of picking wrong is destruction.
- A cross-tenant listing is requested by a caller who does not hold `admin`. The parameter is ignored and the caller's own project is returned; the answer must be indistinguishable from the same request without the parameter, so it does not depend on whether any other project has resources.
- A cross-tenant listing is combined with paging. Page boundaries must remain stable and must not skip or repeat resources when the result set spans projects.
- An operator deletes another project's resource whose deletion has side effects the owning project can observe — a released address, a detached volume, a revoked console token. The owning project's remaining records must not be left referring to something that no longer exists.
- An administrator-only operation is invoked by an administrator against another project's resource. The administrative gate and the project scoping must agree; today two operations gate on the role and then scope to the caller, so the gate can never be satisfied usefully.
- A caller asks for a scope naming both a project and a domain in one request. This is contradictory and must be refused rather than resolved by precedence.
- A caller asks for a domain scope naming a domain by an identifier that is not a well-formed identifier. Because the reference is never resolved, this must produce the same bad request as every other domain scope — not a server error, and not a different answer that reveals the reference was malformed.
- A client that already sends its domain as a qualifier inside a project scope keeps working. This is the form Horizon, Terraform and the CLI all send, so catching it would break every existing caller.
- A token issued before this feature ships is presented after it ships. It must continue to be accepted with exactly the authority it had when issued.
- An administrator views another project's server. The addresses on that server are read through a separate lookup that is scoped to the caller's project, so widening only the server lookup would return a server that appears to have no network at all. Every sub-lookup feeding a response must widen with it.

## Requirements *(mandatory)*

### Functional Requirements

#### Administrative override (User Story 1)

- **FR-001**: The override MUST be expressed through the existing shared role-check behaviour rather than a new mechanism, and MUST follow one of the two shapes the product has already established — the owner-or-administrator point lookup used by the console, or the administrator-may-target-any-project resolution used by the quota endpoints — chosen per operation according to whether the project is the caller's scope or a parameter of the request.
- **FR-002**: The system MUST grant the override on the basis of a role the caller's token carries, and MUST NOT require the caller to hold membership of the project owning the target resource.
- **FR-003**: A caller who does not hold that role MUST see behaviour identical to today's for every operation in scope. In particular, a refusal MUST NOT distinguish a resource that does not exist from one that exists in another project, because distinguishing them confirms the resource exists — the rule the console and image deletion already follow.
- **FR-004**: The override MUST apply to every server operation **except creation** — reads (show, detail, diagnostics, metadata and tag reads, console), and state-changing operations (delete, stop/start/reboot, resize, rebuild, metadata and tag writes, volume and interface attach/detach, migrations, evacuate, reset-state). This is upstream Nova's `admin_or_owner` default, so it is a rule rather than a list, and no divergence needs documenting.
- **FR-004a**: An administrator MUST NOT be able to **create** a server into another project. Creation is the one server operation the override does not cover: upstream provides no such override, no supported client asks for it, and admitting it would make an administrator's own project ambiguous at create time.
- **FR-005**: Resource listings MUST return only the caller's own project by default, and MUST widen to every project only when the request explicitly asks for a cross-tenant view **and** the caller holds the administrative role. Widening MUST NOT happen implicitly on the strength of the role alone, so no existing client's default view changes.
- **FR-005a**: The cross-tenant request parameter MUST follow upstream's own conventions, because clients already send them: present with no value means enabled, and the accepted boolean vocabulary matches upstream's rather than being stricter.
- **FR-005b**: A caller who does not hold the administrative role and passes the cross-tenant parameter MUST have it **silently ignored** — the listing returns their own project, as upstream does for administrator-only query parameters. It MUST NOT be refused, because a refusal would tell the caller the parameter exists and is gated, and MUST NOT widen.
- **FR-005c**: Matching a server by **name** MUST remain scoped to the caller's own project, even for a caller holding the administrative role. Addressing another project's server MUST require its identifier. Lightstack accepts a name where upstream accepts only a UUID, so this keeps a local convenience from becoming a way to destroy the wrong tenant's server by typing a name two tenants happen to share.
- **FR-005d**: A name that matches no server in the caller's own project MUST be answered exactly as it is today — "not found" — and MUST NOT fall through to a cross-project search, because falling through would reintroduce the ambiguity FR-005c exists to prevent.
- **FR-006**: When a listing returns resources from more than one project, each entry MUST identify its owning project.
- **FR-007**: When the override is exercised, the operation MUST be recorded against the **owning** project, with the acting user identified. Recording it against the acting operator's project instead would leave the affected tenant's own action history silently empty, so the party with the strongest interest in the record could never find it. A cross-project action is recognisable in that record because the acting user holds no role in the project the action is recorded against — no additional field, and no change to what is stored.
- **FR-007a**: The operations the override newly covers MUST appear in that record, under upstream's action vocabulary (research R6) — every operation upstream records as an instance action. Operations upstream records under no action name — a server update, its metadata, its tags — are not recorded either, so the history reads as it does against Nova. Today only creation is recorded, so an operator deleting or resizing a tenant's server would leave no trace at all — which would satisfy FR-007 only vacuously.
- **FR-007b**: A **refused** cross-tenant compute attempt MUST be logged as a warning naming the acting caller, the target resource and its owner. This matches the behaviour #545 established for refused cross-tenant network writes, and exists for the same reason: a refused cross-tenant access is the event an operator goes looking for afterwards, and it cannot be answered from a log that recorded nothing. A permitted access by the resource's own owner logs nothing.
- **FR-008a**: Operations gated to administrators only MUST NOT then restrict themselves to the administrator's own project. Evacuating an instance from a failed host and resetting an instance's state are administrative operations whose whole purpose is acting on resources the administrator does not own; both currently refuse with "not found" in exactly that case. This requirement carries no policy decision — it makes two existing operations do what their own gate already claims.

#### Administrative surfaces (User Stories 3 and 6)

- **FR-008b**: The aggregate usage view — the form that reports every project's usage broken down by project — MUST require the administrative role.
- **FR-008c**: The per-project usage view MUST resolve which project it reports on through the same shared resolution the quota endpoints use, so an administrator may name any project while every other caller may read only their own: naming any other identifier — another project's, one that does not exist, or a malformed one — is refused with the same `403`, before anything is looked up.
- **FR-008d**: A usage request naming a project that is malformed or does not exist MUST be refused with a client error naming the problem. It MUST NOT report zeroes, which a caller cannot distinguish from a real project that happens to own no instances, and which today masks a malformed identifier as a success.
- **FR-008e**: The deployment-wide migration list MUST require the administrative role. It reports every project's instance movements and is not scoped to any project.
- **FR-008f**: Every host aggregate operation — listing, showing, creating, updating, deleting, adding and removing hypervisors, and setting metadata — MUST require the administrative role. Aggregates describe the deployment's scheduling topology and belong to no project; there is no owner for an owner-or-administrator rule to fall back on.
- **FR-008g**: The per-server migration operations MUST apply the same ownership rule as every other server operation. They currently apply **none** — a caller may read, act on, and delete the migration records of any project's server. Satisfying FR-004 here means adding an ownership check that does not exist, not relaxing one that does.

#### Scope vocabulary (User Story 2)

- **FR-009**: The system MUST refuse a domain-scoped authentication request with a **bad request** naming the domain scope as unsupported. No domain-scoped token is issued, and no domain-level authorization layer is introduced: role assignments bind a user to a role within a project only, so there is no domain-level grant to read, and no supported client requires one. The refusal replaces today's silent degradation, which is the defect.
- **FR-010**: The system MUST NOT answer any authentication request with a success that both omits the scope the caller asked for and carries no roles. Every scope request MUST be either honoured or refused.
- **FR-011**: The system MUST refuse, with a client error naming the scope kind, any scope it recognises in the request but does not support. FR-009 is the one instance of this rule that exists today; FR-011 is the general form, so a future unsupported scope kind inherits the behaviour without a new requirement.
- **FR-012**: The system MUST refuse a request naming more than one scope kind at once.
- **FR-013**: A refusal caused by an unsupported or contradictory scope MUST be distinguishable by the caller from a refusal caused by invalid credentials.
- **FR-014**: The refusal MUST NOT attempt to resolve the domain reference it is rejecting. Rejecting before resolution is what makes the refusal uniform across domains that exist, domains that do not, domains the caller holds no role in, and malformed references — and uniformity is what guarantees the answer discloses nothing about which domains exist.
- **FR-015**: The refusal MUST apply only to a **domain scope**, and MUST NOT catch the domain **qualifier nested inside a project scope**, which is the existing supported form that every current client sends.
- **FR-016**: Existing project-scoped and explicitly unscoped authentication requests MUST be answered exactly as they are today, including the shape of the response and the contents of the issued token. Both authentication paths — password and token — MUST agree, since they carry duplicated scope-handling logic today.
- **FR-017**: Tokens issued before this feature ships MUST continue to be accepted, carrying exactly the authority they carried when issued.

> **FR-018 is intentionally unused.** It previously required project-scoped operations to refuse a domain-scoped token. Clarification settled that no domain-scoped token is ever issued (FR-009), which made it moot. The number is left vacant rather than reused, so references in earlier drafts do not silently resolve to a different requirement.

#### Validation (User Story 4)

- **FR-019**: The multi-tenancy validation suite MUST gain positive coverage of the override — at least one check where an operator reaches a resource owned by another project — alongside its existing negative coverage.
- **FR-019a**: The suite MUST also cover the operator-only surfaces from both directions: a non-administrative caller refused, and an operator admitted. A gate that is added but never exercised is indistinguishable from one that was forgotten.
- **FR-020**: The suite MUST contain at least one assertion that turns red when a project-scoping condition is removed from a resource lookup.
- **FR-021**: The guarantees #545 established MUST continue to hold: no identity used to assert isolation holds an administrative role, an unresolvable role name fails the run loudly, and teardown runs however the suite exits.
- **FR-022**: Each behaviour change MUST be demonstrated failing before and passing after, per defect, so no fix is accepted on assertion alone.

#### Identity collection filtering (User Story 5)

- **FR-024**: The identity collections that currently discard every query filter — roles, users, projects, groups, services and endpoints — MUST honour the filters upstream documents for them, at minimum filtering by name where upstream offers it. The domain collection already does this, so the shape to follow exists in the codebase rather than needing invention.
- **FR-025**: A filter that matches nothing MUST return an empty collection. Returning the unfiltered collection is the specific failure this addresses: it is indistinguishable from a successful match to any caller that reads the first entry.
- **FR-026**: An unfiltered request MUST be answered exactly as it is today.
- **FR-027**: The equivalent defect in the network service's subnet collection is **out of scope** — it is tracked as B6 in the Neutron sub-ticket (#546) and must not be fixed in both places.

#### Documentation

- **FR-023**: Project documentation describing Lightstack's authorization and scoping model MUST be updated to state the rule as it ends up defined, and MUST NOT continue to describe behaviour this feature changes.

### Terminology

Two words are used deliberately and are not synonyms. **Operator** means the person — the human actor in the user stories, who holds the administrative role. **Administrative role** means the credential itself, the thing a token carries and the thing the rule tests. An operator is refused when their token lacks the administrative role; the rule never tests who someone is.

### Key Entities

- **Token scope**: The authority boundary a token carries. Two kinds exist and remain the only two: project-scoped, and explicitly unscoped. This feature settles that domain is *not* a third kind, and makes the system say so instead of pretending otherwise.
- **Administrative override**: The rule permitting a caller to act on a resource owned by a project other than the one their token is scoped to. Already exists in three services and in compute's console; this feature makes its application consistent.
- **Resource ownership**: The relationship binding a compute resource to exactly one project. Unchanged by this feature — the override changes who may act across it, not who owns what.
- **Role assignment**: The binding of a user to a role within a project, which determines the roles a token carries and therefore whether the override applies. Its project-only shape is what makes FR-009 a design question rather than a repair.
- **Usage report**: An aggregate of a project's compute consumption. It has no owner row of its own — it is derived by grouping instances — which is how it came to have no ownership check at all.
- **Deployment-scoped resource**: A resource belonging to the deployment rather than to any project — a host aggregate, the deployment-wide migration listing. Because it has no owning project, the owner-or-administrator rule cannot apply to it; access is administrative or nothing. Its absence of an owner column is precisely why these surfaces were left ungated: there was no `project_id` to filter on, so no filter was written.
- **Validation identity**: A user and project pair created by the test suite for the purpose of demonstrating isolation. Its defining property is that it is *not* privileged.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An operator holding the administrative role can complete a support task against another tenant's virtual machine — locate it, read its state, and act on it to the extent decided in FR-004 — without being granted a role in that tenant's project and without direct database access.
- **SC-002**: 100% of the compute operations in scope apply the same override rule; no operation in scope permits cross-project access the rule denies, and none denies access the rule permits.
- **SC-002a**: A tenant can determine, from records available to them, that an operator acted on their server and which user it was — for every operation the override covers.
- **SC-002b**: A refused cross-tenant compute attempt is answerable from the logs afterwards: the acting caller, the target and its owner are all recoverable.
- **SC-003**: Zero operations remain that are gated to administrators only and then restricted to the administrator's own project.
- **SC-004**: A non-administrative user's ability to reach another project's resources is unchanged: every cross-tenant attempt refused before this feature is still refused after it — across the two services this feature edits, and demonstrably untouched in the three it does not — and no refusal newly reveals that a resource exists.
- **SC-004a**: A non-administrative caller can learn nothing about any project other than their own from the usage endpoints — neither the set of projects that exist, nor any named project's consumption.
- **SC-004b**: Zero compute surfaces that describe or reshape the deployment are reachable without the administrative role.
- **SC-004c**: A non-administrative caller cannot change any state that is not owned by their own project — across every compute surface, not only servers.
- **SC-005**: 100% of authentication requests carrying a scope receive either a token stating that scope or a refusal naming it; zero requests receive a success carrying neither. Every domain-scope refusal is identical regardless of the domain named, so none reveals which domains exist.
- **SC-006**: Every authentication request valid before this feature ships receives an unchanged answer after it ships, and every token issued before it ships remains usable with unchanged authority.
- **SC-007**: The multi-tenancy validation suite turns red when a project-scoping condition is removed from a resource lookup, and green when it is restored — demonstrating the suite tests isolation rather than assuming it.
- **SC-008**: The multi-tenancy validation suite covers both directions: at least one check fails if a tenant gains cross-project access, and at least one fails if an operator loses it.
- **SC-009**: The multi-tenancy validation suite runs twice consecutively against the same deployment with identical results.
- **SC-010**: Every identity collection that accepts a documented filter applies it; zero return the unfiltered collection in response to a filter they do not support.
- **SC-011**: The multi-tenancy suite's client-side filtering workaround can be removed without changing its result, demonstrating the server-side filter is real.
- **SC-012**: A reader of the project documentation can state, from the documentation alone, which callers may act across project boundaries and which scope kinds are accepted, and that statement matches observed behaviour.

## Assumptions

- **A-001 — Base**: Specified against `feature/545` @ `9992ebf`, the tip of the stacked chain (549 ← 545 ← 534 ← 533 ← 532 ← 482 ← 524 ← 481 ← 515 ← 480 ← main). This is the base the ticket's findings were taken on, so its figures are directly comparable. The ticket reports 1 administrative check in compute and 49 project-scoped queries across 40 handlers; the current tip carries 4 checks and 102 project-scoped predicates across 12 files, with 56 handler-level reads of the caller's project. The additional checks are administrator-*only* gates rather than overrides, which is what FR-008a addresses.
- **A-002 — What the rebase supplied**: The reference implementations this feature builds on are now present, and the specification depends on them rather than inventing equivalents. There is a shared role-check helper and an administrative-role constant used at 15 call sites across six packages; an owner-or-administrator point lookup in the console that already settles the disclosure question (both "absent" and "someone else's" answer alike, because distinguishing them confirms existence); an administrator-may-target-any-project resolver behind the quota endpoints that already settles the parameterised-project case, including refusing to resolve a non-administrator's foreign identifier so the endpoint cannot be used to probe which projects exist; and cross-project administrative writes in image deletion and subnet creation. FR-001 and FR-003 are written to extend these rather than restate them.
- **A-003 — What #545 supplied**: The validation fixtures this feature previously had to specify are done. The multi-tenancy suite selects the non-administrative role by name on the client side, refuses to run if that role resolves to the administrative one, asserts neither tenant token carries the administrative role, checks the status of each role assignment, and tears down on any exit. User Story 4 is correspondingly reduced to the override coverage and the deliberately-red check, which #545 did not and could not add.
- **A-004 — Service scope**: The rule is specified for **compute** only — its server operations (FR-004), its usage reporting (FR-008b–FR-008d), and its deployment-scoped surfaces (FR-008e–FR-008g). It already exists in the image and network services and in compute's console; extending it to the remaining block-storage and identity surfaces is out of scope. The framing is therefore "finish applying an established rule", not "introduce a rule".
- **A-005 — Role name**: The override keys off the existing administrative role, which is seeded today and already has a shared constant. No new role is introduced. Roles are not a closed set — operators may define their own — so nothing may assume the seeded three are exhaustive.
- **A-006 — Backwards compatibility is a hard constraint**: No change may alter the answer given to a request that is valid today, except where a Functional Requirement names that change explicitly. FR-008e and FR-008f are such changes: a non-administrative caller that reaches these surfaces today will be refused afterwards. That is the point of them, and no supported client depends on the current behaviour — Horizon reaches both only from its Admin panel, as an administrator.
- **A-007 — Storage**: No schema change and no migration. Refusing domain scope rather than honouring it removes the one thing that could have required storage — a way to express a role held at domain level — so nothing about ownership or roles changes shape.
- **A-008 — Identity filtering is in scope, by elimination**: The sibling sub-tickets of #536 were checked and none covers it — #546 is Neutron, #547 is Nova discovery and compute gaps, #548 is the test harness. #546's B6 is the same bug class on the network service's subnet collection and stays there. Six identity collections are affected; the seventh was fixed by #545, which is why the fix shape is already in the codebase (FR-024).

## Out of Scope

- Extending the administrative override to block-storage and identity surfaces that do not have it, beyond what consistency with compute requires.
- Introducing a system scope, group-based role assignments, inherited (domain-to-project) role assignments, or implied roles.
- Building a domain-level authorization layer, or domain-scoped tokens of any kind. FR-009 settles this as a refusal; the blueprint's existing "no domain-scoped assignments" gap stands, now honestly reported rather than silently degraded.
- Introducing a general-purpose, externally configurable policy engine. This feature settles one rule; it does not make the rule user-editable.
- Changing what any role other than the administrative role may do.
- Allowing an administrator to create a server into another project (FR-004a).
- Upstream's `project_id`/`tenant_id` narrowing filter on listings, and the other administrator-only query parameters upstream gates (`host`, `auto_disk_config`, `config_drive`). All are additive on top of FR-005 and can follow when a client needs them.
- Changing project or domain lifecycle behaviour, which is #545's subject and is already merged into this branch's history.

## Dependencies

- The shared role-check helper, administrative-role constant, and project-reference resolution introduced by #533/#534/#545 and present on this base.
- The multi-tenancy suite's non-administrative fixtures and fail-loudly setup, introduced by #545.
- The existing authentication flow, whose backwards compatibility constrains every change in User Story 2.
- The existing role assignment model, whose project-only shape is the reason FR-009 refuses domain scope rather than honouring it.
