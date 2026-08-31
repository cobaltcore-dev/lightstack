# Feature Specification: Nova Gaps & Client Discovery

**Feature Branch**: `feature/547` (feature dir `025-nova-gaps-client-discovery`)
**Created**: 2026-08-28
**Status**: Draft
**Input**: cloud-storage#547 — "o3k — Nova gaps & client discovery (B3, B4, B7, B12, B13, B14, B15,
B16, F16)", title + body + 1 comment (which adds **B17**). Sub-issue of #536; absorbs **F16** from
the closed #537.

## Overview

Ten defects in two families that meet at the same place: **what a stock client is told about the
compute service**.

The first family is **discovery** — the version documents and self-links that a client reads *before*
it makes a single API call. One version document omits its own link array, which makes a routine
`flavor list` abort inside the client library; and every advertised self-link prefers a
deployment-configured hostname over the host the client actually addressed, which makes a per-command
endpoint override unusable. The same omission exists one service over in identity, where three `show`
commands abort on a client version that ships with the current Debian/Ubuntu release — including the
version this project's own real-mode runbooks are executed with, where the failure degrades a
fixture to empty strings instead of stopping.

The second family is **compute answers that are wrong in a way the status code cannot reveal**. A
server reaches *active* with no network at all, because a failed address allocation is logged and the
boot continues. A stopped server cannot be started again, and afterwards the product keeps serving a
*running* server that the hypervisor does not have — nothing reconciles the two. A floating IP that
was successfully associated never appears in the one field users and the dashboard read to check.
An action addressed by name — a form this product deliberately accepts everywhere else — fails with a
server error. A usage counter filters on a state the product never writes. An operator's
cross-tenant action is attributed in the access log to the tenant acted upon rather than the operator
who acted. And a boot-from-new-volume request is accepted, then quietly downgraded to a plain image
boot with no volume created.

The common thread across both families is the same as the sibling network work: **the answer looks
successful**. The version document returns `200`, the boot returns `202`, the association returns
`202`, the limits view returns `200`. A client cannot detect any of it from the response, which is
why the existing contract suites pass while the behaviour is wrong — and why three of these were
found only by a human running a real host through the dashboard.

## Clarifications

### Session 2026-08-31

Ten decisions: the three the ticket left open, then seven found by an ambiguity scan of the spec
itself. None remain open.

- Q: When a requested network cannot be attached, is the boot refused up front or accepted and landed
  in a failed state? → A: **Both, split by when the fault is detectable.** A network that does not
  exist or that the caller may not use is refused before the request is accepted. A fault that only
  appears once allocation is under way — after the create has already been answered — lands the server
  in a failed state carrying a readable reason. Upstream compute fails the build; this keeps that
  outcome while still refusing the common typo synchronously.
- Q: What happens to a deleted server's action history? → A: **Retained past the server, then
  reclaimed by age.** Deleting a server does not remove its action records, matching upstream, and a
  stated retention window bounds their growth. The window is a deployment setting, not a constant baked
  into the product.
- Q: What should a start do when the server's hypervisor guest is absent? → A: **Re-create it from the
  server's stored definition and start it** — the same self-healing upstream's hard reboot performs.
  Every input to the guest's definition is already derivable from stored state, so nothing from the
  original create request is needed. The self-heal does not excuse the record: a divergence must still
  be surfaced rather than silently repaired, so FR-024 stands alongside it.

- Q: How wide is the self-link work — only where the ticket names, or every emitter in the product? →
  A: **Product-wide.** Every self-link emitter is routed through the one shared rule. Three of the
  out-of-ticket sites carry the *same* discovery defect as the compute version document — a hardcoded
  host, a relative path and a hardcoded scheme — and would break the same client flow for volumes,
  networks and placement. Once the shared rule is correct the remaining sites are one-line changes.
- Q: Which host wins when a deployment configures one and the client addresses another? → A: **The
  request host wins; the configured hostname becomes the fallback** used when a request carries no
  usable host. This is the only answer that fixes the discarded endpoint override, and behind a proxy
  or ingress the request already carries the hostname the deployment wants advertised. An opt-out
  setting for deployments that must pin their advertised host was considered and rejected as
  unnecessary — see *Constraints & Tradeoffs*.
- Q: How is a divergence between the recorded state and the hypervisor detected — at read time, or by
  reconciliation? → A: **Persist the state on every action, plus a periodic sweep for out-of-band
  drift.** The divergence this feature observed is *caused* by the action path recording nothing, so
  fixing that removes the source rather than compensating for it. A sweep then catches what no action
  caused — a crashed guest, an out-of-band removal. Reads stay free of hypervisor calls, which a
  per-read check could not offer on a server listing. The consequence is that truth is bounded by the
  sweep interval rather than instantaneous, so SC-006 states a window instead of claiming zero.
- Q: Can a deleted server's action history be read, given the records now outlive the server? → A:
  **Yes — by the owning project and by administrators**, scoped on the ownership recorded with each
  action rather than on the server, which no longer exists. This changes a `404` to a `200` for a
  request that is answered today, which is accepted deliberately: retaining records the API will never
  serve would make the retention decision meaningless. Non-owners continue to get the same *absent*
  answer they get now.
- Q: What size is a boot volume given when the request omits one? → A: **The flavor's disk size,
  floored at the image's minimum disk.** The volume is the server's root disk, so the flavor is the
  size the user already chose by picking it. The image's minimum cannot be the sole source because it
  defaults to zero; used as a floor it also keeps the too-small refusal coherent for the defaulted
  case.
- Q: Which failure conditions does a lifecycle action distinguish, and how? → A: **Upstream's mapping
  — absent target, wrong state or impossible action, and genuine internal fault are three distinct
  answers.** A status upstream never emits was rejected even though it would describe an unreachable
  hypervisor more precisely: clients that branch on status are written against upstream, and the extra
  precision is already available in the logs.
- Q: How is a self-heal recorded so it is discoverable? → A: **On the existing start action's own
  record, plus a warning-level log entry.** The action record for the healing request is already
  written and already carries a message field, so the repair is visible to the server's owner and to an
  operator without inventing an event upstream does not have or adding a tenant-facing surface.

---

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Use a stock command-line client with no special configuration (Priority: P1)

A user installs the standard OpenStack client, points it at a Lightstack deployment, and runs everyday
commands. Today one of the first things they try — listing flavors — fails inside the client with a
bare indexing error, and the only way through it is to know that a version must be pinned in the
environment. Separately, a user who overrides a single service's endpoint per command finds the
override silently discarded: the client resolves the deployment's configured hostname instead, and
fails against a host the user never named.

**Why this priority**: it is the first thing every client does and the first thing every new user
hits. The failure surfaces as a client-side crash rather than an API error, so it reads as a broken
product; and both halves are prerequisites for testing anything else in this feature from a stock
client.

**Independent Test**: with no version pinned in the environment, list flavors — the flavors are
listed. Then set an endpoint override for one service and run a command against it — the override is
the host contacted.

**Acceptance Scenarios**:

1. **Given** an environment with no compute API version pinned, **When** the user lists flavors,
   **Then** all flavors are listed and no client-side error occurs.
2. **Given** the compute service's current version document is fetched directly, **When** its body is
   read, **Then** it carries a link array with a self link whose address is absolute and resolvable.
3. **Given** a per-service endpoint override naming a host different from the deployment's configured
   hostname, **When** any command is run against that service, **Then** the override is the host
   contacted and the deployment's configured hostname is not substituted.
4. **Given** a request that arrives through a proxy or ingress under one hostname, **When** any
   response carrying a self-link is read, **Then** the link names that same hostname and a port the
   service is actually reachable on — never an internal address the client cannot resolve.
5. **Given** the deployment configures no hostname at all, **When** self-links are produced, **Then**
   they are still absolute and resolvable, derived from the request.

---

### User Story 2 — Read back a project, user or role with a stock client (Priority: P1)

An operator runs `project show`, `user show` or `role show` — by name, as everyone does. On the client
version shipped by the current Debian/Ubuntu release the command exits non-zero with a one-word
error and prints nothing, because the response omits a field the client removes unconditionally
before rendering. On a newer client the same command works, so whether the product appears functional
depends on which client happens to be first on the path.

**Why this priority**: it breaks basic read-only identity operations on a mainstream client, it is the
same one-field omission as US1 in a second service, and — most consequentially — it silently degrades
this project's own real-mode validation fixtures, which read identifiers from exactly these commands.
A fixture that returns an empty string instead of failing makes every later step in a runbook test
the wrong thing.

**Independent Test**: with the older client, run all three `show` commands by name — each prints the
resource. Independently, fetch each resource directly and confirm the link field is present and
absolute.

**Acceptance Scenarios**:

1. **Given** the older stock client, **When** the operator shows a project, a user and a role by name,
   **Then** each command exits successfully and prints the resource.
2. **Given** any single identity resource is fetched directly, **When** its body is read, **Then** it
   carries a self link whose address is an absolute, resolvable URL — including a scheme.
3. **Given** an identity collection is listed, **When** an entry is read, **Then** that entry carries
   the same self link in the same shape as the single-resource read.
4. **Given** a resource that already reported a link before this change, **When** it is read again,
   **Then** the link is still present and is now absolute rather than missing its scheme.
5. **Given** a runbook that reads an identifier from one of these commands, **When** it runs on the
   older client, **Then** it obtains the real identifier rather than an empty value.

---

### User Story 3 — A server never reports itself healthy without the network it asked for (Priority: P1)

A user creates a server on a named network. The address allocation for that network fails. Today the
failure is logged and the boot continues: the server reaches *active* with an empty address map and no
interface at all. The user is told the server is running; it has no connectivity, cannot reach the
metadata service, and there is nothing in the server's own record to say why.

**Why this priority**: it is a silent fail-open on the product's central operation, and it converts
any single downstream networking fault into a class of failure that is indistinguishable — from the
API — from a working server. During the sibling tenant-scoping work this exact state was read as a
cross-tenant authorization defect and investigated as a product failure until the network's owner was
checked by hand. Fixing the underlying allocation fault in the sibling network work does not fix
this: it removes one cause and leaves the fail-open in place for the next.

**Independent Test**: induce an address-allocation failure for a requested network, then create a
server on it — the server does not report itself as running and healthy, and its own record says what
went wrong.

**Acceptance Scenarios**:

1. **Given** a requested network whose address allocation fails, **When** a server is created on it,
   **Then** the server does not reach a running state, and its record reports a failed state with a
   reason.
2. **Given** a requested network that does not exist or is not usable by the caller, **When** a server
   is created on it, **Then** the request is refused before anything is created, and the refusal names
   the network.
3. **Given** a server whose boot failed for this reason, **When** it is read back, **Then** its
   reported state and its address map agree with each other — it never reports running with no
   addresses.
4. **Given** a server whose boot failed after some resources were already created, **When** the
   failure is handled, **Then** those resources are released or remain attributed to the failed
   server, and none are left ownerless.
5. **Given** a server created with no network requested at all, **When** it boots, **Then** the
   existing behaviour is unchanged — this story constrains *requested* networks only.

---

### User Story 4 — Stop a server and start it again (Priority: P1)

An operator stops a running server, then starts it again. Today the stop is accepted, the start fails
with a server error naming a missing hypervisor guest, and — worse — the product's record still says
the server is running with a live power state and a hypervisor guest identifier, while the hypervisor
has no such guest. Nothing reconciles the two, and reading the server reports *active* either way, so
the divergence is invisible until the next action fails.

**Why this priority**: stop/start is a basic lifecycle operation, and the failure leaves the product
serving a confident answer that is false. A record that disagrees with reality is worse than an error,
because every later decision — the operator's, the dashboard's, a contract suite's — is made against
it. It is also the suspected reason a contract suite hangs against any stack where this has been
triggered.

**Independent Test**: on a real-mode host, stop a booted server and start it again — both succeed, and
after each the reported state matches the hypervisor's. Separately, remove the hypervisor domain out
of band and read the server — the product does not report it as running.

**Acceptance Scenarios**:

1. **Given** a running server, **When** it is stopped, **Then** the operation succeeds and the server
   thereafter reports a stopped state and a stopped power state.
2. **Given** a stopped server, **When** it is started, **Then** the operation succeeds and the server
   thereafter reports a running state.
3. **Given** a server whose guest was removed by something other than the product, **When**
   reconciliation has run, **Then** the server is no longer reported as running — and until then the
   staleness is bounded by the stated interval rather than permanent.
3a. **Given** a server the product itself stopped or started, **When** it is read immediately
   afterwards, **Then** it reports the new state without waiting for any reconciliation.
4. **Given** a server whose hypervisor guest is absent, **When** it is started, **Then** the guest is
   re-created from the server's stored definition and started, the operation succeeds, and the
   re-created guest matches the server as recorded.
5. **Given** a self-heal has occurred, **When** an operator looks for it, **Then** it is visible — a
   guest that keeps vanishing is discoverable rather than masked by a start that keeps succeeding.
6. **Given** a server whose guest is absent and cannot be re-created, **When** it is started, **Then**
   the failure names why, and the reported state ends up consistent with reality — in no case an
   unexplained server error over a record that still claims the server runs.
7. **Given** any lifecycle action that the hypervisor refuses, **When** the response is returned,
   **Then** its status distinguishes an absent target, an action impossible in the current state, and an
   internal fault — using the statuses upstream uses for each.
8. **Given** a server stopped and started repeatedly, **When** each action completes, **Then** the
   reported state and the hypervisor agree after every one of them.

---

### User Story 5 — Address a server action by name, as everywhere else (Priority: P2)

This product deliberately accepts a server *name* wherever the upstream API accepts only an
identifier, and almost all of the compute surface honours it — including deletion. The action
endpoint does not: it authorizes the request correctly, then hands the sub-handler the raw name,
which reaches a query expecting an identifier and fails with a server error. Deleting a server by name
works; suspending the same server by name does not.

**Why this priority**: it is an internal inconsistency in a local extension rather than a missing
feature, and it is invisible to the stock client, which resolves names before the request leaves the
machine. But it is a server error on a documented-by-behaviour form, it affects every action on the
endpoint, and it is a small, well-bounded fix.

**Independent Test**: create a server, then request each action against it **by name** directly,
bypassing the client's own name resolution — each either succeeds or reports the resource as absent;
none returns a server error.

**Acceptance Scenarios**:

1. **Given** a server that exists, **When** an action is requested for it by name, **Then** the action
   is performed, exactly as it would be if the identifier had been used.
2. **Given** a name that matches no server, **When** an action is requested for it, **Then** the
   response reports the server as absent — never a server error.
3. **Given** a name that exists in another project, **When** a non-administrator requests an action on
   it, **Then** the response is the same *absent* answer the rest of the compute surface gives — a
   shared name never reaches another tenant's server.
4. **Given** an administrator acting on another project's server by identifier, **When** the action is
   requested, **Then** the existing cross-project override still applies unchanged.
5. **Given** every action the endpoint dispatches, **When** each is exercised by name, **Then** all of
   them behave per the above — no action is left resolving the reference its own way.

---

### User Story 6 — See a floating IP on the server it was associated with (Priority: P2)

A user associates a floating IP with a server and then looks at the server. The address map shows only
the fixed address; the floating address is absent. The association did work — listing floating IPs
reports it bound and active — but the field that users, the dashboard's instance list and the
Terraform provider all read is missing it, so the user is effectively told the association had no
effect.

**Why this priority**: it is the visible result of an operation the product performs correctly, in the
one view people check immediately afterwards. It is a read-path omission with no data loss and a
well-understood upstream shape, which is why it sits below the stories that break an operation
outright.

**Independent Test**: associate a floating IP with a server, then read the server — the address map
carries both addresses for that network, each labelled with its kind.

**Acceptance Scenarios**:

1. **Given** a server with a floating IP associated to its port, **When** the server is read, **Then**
   its address map lists both the fixed and the floating address under the same network.
2. **Given** that same server, **When** each address entry is read, **Then** each is labelled with its
   kind — fixed or floating — and the labels are correct.
3. **Given** a list of servers rather than a single one, **When** it is read, **Then** it reports the
   same addresses as the single-server read.
4. **Given** a server with no floating IP, **When** it is read, **Then** its address map is unchanged
   from today's shape.
5. **Given** an administrator reading another project's server, **When** that server has a floating
   IP, **Then** it appears there too.
6. **Given** a floating IP that is allocated but not associated to any port, **When** any server is
   read, **Then** it appears on none of them.

---

### User Story 7 — Trust the usage numbers and the action history (Priority: P2)

An operator reads a project's limits to see how much of its quota is used. The instance count is
filtered on a state the product never assigns to anything, so the filter is dead code that only
obscures the query's intent — and it is the only usage query carrying it. Separately, deleting a
server leaves its action history behind permanently: that history is only ever appended to, has no retention
of any kind, and the sibling tenant-scoping work multiplies the rate at which it grows by recording
every server action rather than creation alone.

**Why this priority**: the dead filter is a correctness hazard rather than a current miscount — it will
start silently under-reporting the moment anything begins writing that state — and unbounded growth in
a store that is only ever appended to is a real operational problem, just not an urgent one. Grouped because
they are the same question about what deletion means.

**Independent Test**: read a project's limits and compare the reported instance, core and memory usage
against the project's actual servers — they agree. Independently, create and delete a server, then
confirm its action history survives the deletion — and that records past the retention age are gone.

**Acceptance Scenarios**:

1. **Given** a project with servers in a mix of states, **When** its limits are read, **Then** the
   reported usage counts every server the project has, and matches the same figures reported by the
   quota views.
2. **Given** the usage figures, **When** how they are derived is inspected, **Then** they exclude no
   server on the basis of a state the product never assigns.
3. **Given** a server that is deleted, **When** its action history is inspected afterwards, **Then**
   the records are still there — deletion of a server does not delete its history.
3a. **Given** action records older than the configured retention age, **When** reclamation has run,
   **Then** those records are gone while newer ones remain, and the history of a server that still
   exists is untouched regardless of age.
4. **Given** a deleted server's action history, **When** the owning project or an administrator
   requests it, **Then** the records are returned rather than reported absent.
4a. **Given** that same history, **When** any other project requests it, **Then** the answer is the
   same *absent* answer given today, revealing nothing about whether the server ever existed.
5. **Given** a deployment that has run many create-and-delete cycles, **When** the action history is
   measured, **Then** its size is bounded by the settled rule rather than growing without limit.

---

### User Story 8 — See who acted on a tenant's server (Priority: P3)

An operator with administrative rights acts on another project's server. The access log line for that
request names the *target* project as though it were the caller's, because the calling project is
overwritten during the request and the log is written afterwards. The acting user is still named
correctly, so the record is not lost — but the project field, which is what an operator greps when
asking who touched a tenant's resource, points the wrong way.

**Why this priority**: cosmetic in isolation and no user-facing behaviour changes, but it is an audit
field, and it is trivially easy to "fix" by removing the overwrite — which would break the
cross-project override that the overwrite exists to provide. Worth doing carefully and worth doing
last.

**Independent Test**: as an administrator, act on another project's server, then read the service log
— the line identifies the calling project as the operator's, and the target project remains
identifiable.

**Acceptance Scenarios**:

1. **Given** an administrator acting on another project's server, **When** the request completes,
   **Then** the access log identifies the caller's own project as the caller.
2. **Given** that same request, **When** the log is read, **Then** the project acted upon is still
   identifiable — either on the same line or in the operation's own record.
3. **Given** any ordinary same-project request, **When** it completes, **Then** its log line is
   unchanged from today.
4. **Given** the cross-project override, **When** an administrator acts on another project's server,
   **Then** the action still succeeds and is still attributed to the owning tenant in the operation's
   own record — the log fix must not alter that.

---

### User Story 9 — Boot from a new volume, and get a volume (Priority: P2)

A user launches a server from the dashboard's wizard with its default settings, which request a new
boot volume created from an image. The request is accepted and the server boots — from the image
directly. No volume is created, the requested volume size is ignored, and the delete-on-termination
choice is ignored. The user asked for persistent storage, was told it worked, and does not have it.

**Why this priority**: it is the dashboard's *default* launch path, so it is what most first-time
launches actually request; and the gap is a request attribute accepted and discarded, which is the
same silent-success shape as the rest of this feature. It sits below the stories that break an
operation because the server does at least boot.

**Independent Test**: launch a server requesting a new volume from an image with an explicit size —
the server boots, a bootable volume of that size exists, is attached as the server's root disk, and
the delete-on-termination choice is honoured when the server is deleted.

**Acceptance Scenarios**:

1. **Given** a launch request whose root disk is a new volume created from an image, **When** the
   server is created, **Then** a bootable volume is created from that image and the server boots from
   it.
2. **Given** that request specifies a volume size, **When** the volume is created, **Then** it has
   that size.
3. **Given** that request specifies no volume size, **When** the volume is created, **Then** it has the
   flavor's disk size — or the image's minimum disk where that is larger — and never a zero size.
4. **Given** that request asks for the volume to be deleted with the server, **When** the server is
   deleted, **Then** the volume is deleted; and when it asks for the volume to be kept, **Then** the
   volume survives and becomes available.
5. **Given** the volume is created, **When** the volume service is listed, **Then** the volume appears
   there, owned by the requesting project, marked bootable and in use, attached as the server's root
   disk.
6. **Given** the volume cannot be created, **When** the request is handled, **Then** the boot fails
   visibly per US3 rather than falling back to an image boot.
7. **Given** a launch request naming an image directly, or one naming an existing volume, **When** the
   server is created, **Then** both existing paths behave exactly as they do today.

---

### Edge Cases

- A request arrives with no host information at all (a malformed or synthetic request): self-links must
  still be absolute and must not be empty or scheme-less.
- A deployment is reached over an encrypted connection terminated at a proxy: the advertised scheme
  must match what the client used, not what the service received.
- A deployment configures a hostname *and* the client sends a different one: exactly one of them wins,
  the same way, for every service — no service may resolve this differently.
- The port a service is reachable on differs from the port it listens on (a proxy or a chart-published
  port): the advertised link must name the reachable one. The console link derives its own origin today
  precisely because of this; that behaviour must survive the shared fix.
- An identity resource whose optional attributes are absent: adding a link must not change how any
  other field is rendered, including nullable ones.
- A server requests several networks and only one allocation fails: the outcome must be the same as if
  all had failed — a partly-networked server is not a success.
- A server's boot fails after its record exists but before it is running: the record must not be left
  in a state no client can interpret, and it must remain deletable.
- The hypervisor is unreachable when a lifecycle action is attempted, as distinct from the domain being
  absent: these must not report the same thing.
- A stopped server is started while another action is in flight: the reported state must converge, not
  alternate.
- An action is addressed by a name that matches more than one server across projects: it must resolve
  within the caller's own project only.
- An action is addressed by a name that is also a valid identifier shape: it must resolve
  deterministically and not depend on which form is tried first.
- A floating IP is associated to a port that is not attached to any server: it appears on no server.
- Two floating IPs are associated to the same port: both appear.
- A server has ports on several networks and a floating IP on one: only that network's list gains the
  floating entry.
- A project's servers are deleted while its limits are being read: the usage count must not report a
  negative or impossible figure.
- A deleted server's action history is requested by an administrator: the answer must follow the
  retention rule above, and MUST NOT differ by caller.
- A boot-from-new-volume request specifies a size smaller than the image needs — smaller than its stated
  minimum, or smaller than its actual size, or both: it must be refused clearly rather than producing an
  unbootable volume. The same floor applies when the size was defaulted rather than requested.
- An image states no minimum disk at all: the defaulted volume size must still be usable, not zero.
- A boot-from-new-volume request is made in development mode, where no storage is provisioned: the
  behaviour must be defined and must not leave a half-created volume.

## Requirements *(mandatory)*

### Functional Requirements

> **Reading note**: where a requirement has lettered children (FR-024 and FR-024a–d, FR-042 and
> FR-042a–c, FR-016 and FR-016a), the unlettered parent states the **goal** and the children state the
> **contract** that satisfies it. Verify the children; the parent is not separately testable.

#### Client discovery (US1)

- **FR-001**: The compute service's current-version document MUST carry a link collection containing a
  self link, in the same shape the version *list* already uses.
- **FR-002**: Every advertised self-link MUST name the host the client actually addressed, in
  preference to any hostname configured for the deployment.
- **FR-003**: The deployment-configured hostname MUST remain usable as a fallback for requests that
  carry no usable host, so a deployment that relies on it today keeps working.
- **FR-003a**: The change of precedence MUST be documented as a behavioural change for existing
  deployments: a deployment that configures a hostname today will begin advertising the hostname its
  clients actually use. No setting is provided to restore the old precedence.
- **FR-004**: An advertised self-link MUST name a port the service is reachable on, and MUST do so
  regardless of which host source was used — the port MUST NOT be applied in one case and dropped in
  the other.
- **FR-005**: The advertised scheme MUST reflect how the client reached the deployment, including when
  encryption is terminated ahead of the service.
- **FR-006**: A per-service endpoint override supplied by a client MUST be honoured end to end: the
  host the client named MUST be the host it continues to talk to.
- **FR-007**: Every self-link produced by **any** service MUST be an absolute, resolvable URL. No
  self-link may omit its scheme.
- **FR-007a**: Every self-link emitter in the product MUST derive its address from the one shared rule
  of FR-002 to FR-005. All four existing failure forms MUST be eliminated: a relative path, a hardcoded
  host, a hardcoded scheme, and an address assembled without a scheme. No emitter may keep its own
  local rule.
- **FR-007b**: This applies to the version documents of every service — not only compute's — because a
  client reads whichever service it is about to use, and a wrong address there fails that service's
  first call.
- **FR-008**: A stock client MUST be able to complete a basic compute listing with no API version
  pinned in its environment.
- **FR-009**: The console link's origin MUST remain correct after the shared link behaviour is fixed —
  either because the shared behaviour now yields it, or because its local derivation is preserved. It
  MUST NOT regress to advertising the API's own port.

#### Identity read-back (US2)

- **FR-010**: A single-resource read of a project, a user and a role MUST each carry a self link.
- **FR-011**: Entries in the corresponding collection reads MUST carry the same self link in the same
  shape, so a name-based lookup that goes through a collection is served the same fields as a direct
  read.
- **FR-012**: Identity self-links MUST be absolute URLs derived by the same rule as FR-002 to FR-007 —
  one rule for the whole product, not a second implementation.
- **FR-013**: Adding these links MUST NOT change any other field of any identity response, including
  the rendering of absent optional attributes.
- **FR-014**: The three affected read commands MUST succeed on the older stock client that removes the
  link field unconditionally, and MUST continue to succeed on newer clients.

#### Boot without a network (US3)

- **FR-015**: A server whose requested network could not be attached MUST NOT be reported as running.
- **FR-016**: Such a server MUST report a failed state, and the reason MUST be readable from the
  server's own record rather than only from service logs.
- **FR-016a**: A fault detectable **before** the create is answered MUST be refused rather than
  accepted — see FR-017. A fault that only appears **after** the create has been answered MUST land the
  server in the failed state of FR-016. The two paths MUST agree on the outcome a user sees: in neither
  case is a server without its requested network reported as healthy.
- **FR-017**: A requested network that does not exist, or that the caller may not use, MUST be refused
  before the request is accepted, and the refusal MUST name the offending network.
- **FR-018**: A server's reported state and its reported addresses MUST be consistent with one another
  — running with no addresses on a requested network MUST NOT be a reachable state.
- **FR-019**: Where a boot fails part-way, resources already created for it MUST be released or remain
  attributed to that server; none may be left unowned. The failed server MUST remain deletable by its
  owner.
- **FR-020**: A partial failure across several requested networks MUST be treated as a failure, not a
  success with fewer interfaces.
- **FR-021**: A server created with no network requested MUST behave exactly as it does today.

#### Lifecycle state truth (US4)

- **FR-022**: A successful stop MUST be persisted: the server MUST thereafter report a stopped state
  and a stopped power state.
- **FR-022a**: A stop is successful only once the guest is *actually off*. Accepting the hypervisor's
  acknowledgement of a shutdown REQUEST as the outcome is not sufficient — a graceful shutdown is
  negotiated with the guest OS, takes time, and a guest may decline it entirely. A stop MUST therefore
  wait for the guest to stop, and MUST force it off once a bounded grace period expires, so that the
  state it persists is one it observed rather than one it intended.
- **FR-022b**: The grace period of FR-022a MUST be a deployment setting with a documented default,
  and the documentation MUST state that a stop is synchronous and so this period also bounds how long
  a stop request can take. Because it is a request duration, it MUST be bounded above by what the
  request can still be answered within: a stop that runs past the server's response deadline loses its
  response entirely, which reproduces the defect of FR-022a one layer down — the stop succeeds and
  reports nothing. A configured value beyond that bound MUST be clamped and the clamping stated, and
  the default MUST leave room to answer. The relationship between the two durations MUST be enforced
  where they are defined rather than left as an arithmetic coincidence.
- **FR-023**: A successful start MUST be persisted: the server MUST thereafter report a running state
  and a running power state.
- **FR-024**: A server MUST NOT be reported as running when the hypervisor has no corresponding guest.
  A divergence MUST be corrected rather than served indefinitely; FR-024a and FR-024b state how soon,
  which depends on whether the product caused it.
- **FR-024a**: Every action that changes a guest's run state MUST persist the resulting state, so the
  record is correct by construction and no reconciliation is needed for a change the product itself
  made.
- **FR-024b**: A divergence arising from a cause the product did not initiate — a guest that crashed,
  or one removed out of band — MUST be corrected by a periodic reconciliation, within a bounded and
  stated interval.
- **FR-024c**: Reading a server MUST NOT require contacting the hypervisor. A server listing MUST NOT
  become slower in proportion to the number of servers it returns.
- **FR-024d**: The reconciliation interval MUST be a deployment setting with a documented default.
- **FR-024e**: Reconciliation MUST correct a divergence in EITHER direction — a server recorded as
  stopped whose guest is running, as well as one recorded as running whose guest is not — and MUST NOT
  require the guest to be absent in order to notice. A guest that exists but disagrees with the record
  is the same defect as one that is missing. Statuses the hypervisor has no opinion on MUST be left
  alone rather than rewritten from a domain state.
- **FR-025**: A start requested against a server whose guest is absent MUST re-create that guest from
  the server's stored definition and start it, so the operation succeeds. It MUST NOT return an
  unexplained server error, and MUST NOT leave the record claiming the server runs when it does not.
- **FR-025a**: The re-created guest MUST match the server as recorded — its processors, memory, disk,
  network attachments and boot configuration MUST be those the server is defined to have, not defaults.
- **FR-025b**: A self-heal MUST be recorded on the action record of the request that triggered it, and
  MUST also be logged at a level that stands out from ordinary traffic, so a repeatedly vanishing guest
  is discoverable rather than silently masked by a succeeding start.
- **FR-025d**: Recording a self-heal MUST NOT introduce a new action name or a new API surface — the
  existing action record for the request carries it.
- **FR-025c**: If the guest cannot be re-created, the start MUST fail with a stated condition naming
  why, and the server's reported state MUST end up consistent with reality rather than left claiming to
  run.
- **FR-025e**: A self-heal MUST complete within the time budget the product already allows a guest
  creation, and MUST NOT retry. Exceeding the budget MUST be reported as a failure under FR-025c rather
  than by holding the request open.
- **FR-026**: A lifecycle action MUST answer with three distinguishable outcomes: the target is absent,
  the action cannot be performed in the server's current state, or an internal fault occurred. A client
  MUST be able to tell a state problem from an outage without parsing a message.
- **FR-026a**: The statuses used MUST be the ones upstream compute uses for those conditions. No status
  upstream does not emit for a lifecycle action may be introduced, even where it would describe the
  condition more precisely.
- **FR-026b**: A self-heal that cannot complete (FR-025c) is an *action that cannot be performed*, not
  an internal fault, and MUST be reported as such.
- **FR-026c**: An action whose end state the guest is ALREADY in — a stop of a stopped guest, a start
  of a running one — is an action that cannot be performed, not an internal fault. The record MUST be
  corrected to the observed state *before* answering, so that a refusal issued over a stale record
  cannot lock the server out of the action that would have cleared it. A hypervisor that gives no
  usable answer MUST NOT produce a refusal: "could not tell" is not "already done".
- **FR-027**: This applies to every action that reaches the hypervisor directly, not only to start and
  stop.
- **FR-028**: Development mode MUST retain its current behaviour: state transitions are recorded and no
  hypervisor is contacted.

#### Consistent server addressing (US5)

- **FR-029**: Every action dispatched by the server-action endpoint MUST accept the same set of server
  reference forms that the rest of the compute surface accepts.
- **FR-030**: An action naming a server that cannot be reached MUST report it as absent. A reference
  that is not an identifier MUST NOT produce a server error.
- **FR-031**: Server references MUST be resolved through one shared path, so no action can diverge from
  the rule.
- **FR-032**: A name MUST resolve only within the caller's own project, and the existing administrative
  override for identifier-addressed servers MUST be unchanged.
- **FR-033**: Every action currently reachable through the endpoint MUST be covered — none may be left
  reading the raw reference.

#### Floating IPs in the server view (US6)

- **FR-034**: A server's address map MUST include floating addresses associated with its ports,
  alongside the fixed addresses, under the same network.
- **FR-035**: Each address entry MUST be labelled with its kind, and the label MUST be derived from the
  address rather than fixed to one value.
- **FR-036**: The single-server read and the server listing MUST report identical addresses for the
  same server.
- **FR-037**: A server with no associated floating address MUST report the same address map shape as
  today.
- **FR-038**: A floating address that is not associated with a port MUST NOT appear on any server.
- **FR-039**: An administrator reading another project's server MUST see that server's floating
  addresses too — the widened read introduced by the tenant-scoping work MUST NOT stop at this field.

#### Usage counts and action history (US7)

- **FR-040**: The project usage figures reported with limits MUST count every server the project owns,
  and MUST agree with the figures the quota views report for the same resources.
- **FR-041**: No usage figure may exclude a server on the basis of a state the product never assigns.
- **FR-042**: Deleting a server MUST NOT remove its action history. The records survive the server they
  describe, as they do upstream, and this MUST hold at every path that deletes a server — not only the
  ordinary one.
- **FR-042a**: Retained action records MUST be reclaimed once they exceed a stated age, so their total
  volume is bounded rather than growing without limit.
- **FR-042b**: The retention age MUST be a deployment setting with a documented default, not a value
  fixed in the product.
- **FR-042c**: Reclamation MUST NOT interfere with serving requests, and MUST NOT remove the history of
  a server that still exists regardless of the age of its records.
- **FR-043**: A deleted server's action history MUST be readable by the project that owned the server
  and by an administrator. Any other caller MUST receive the same *absent* answer it receives today.
- **FR-043a**: After deletion the server row is gone, so authorization for FR-043 MUST be derived from
  the ownership recorded with each action record. That recorded ownership MUST be the owning project,
  not the project of whoever performed the action.
- **FR-043b**: A caller who is neither the owner nor an administrator MUST NOT be able to distinguish a
  deleted server from one that never existed.
- **FR-043c**: The answer MUST be consistent for a given caller regardless of whether the server still
  exists — the same rule decides both cases.
- **FR-044**: The growth of stored action history MUST be bounded by a stated rule rather than
  unbounded.

#### Access-log attribution (US8)

- **FR-045**: The access log line for a completed request MUST identify the project of the caller who
  made it.
- **FR-046**: For a cross-project action, the project acted upon MUST be identifiable **from the access
  log line itself**, alongside the caller's project. Requiring a stored record to be consulted does not
  satisfy this — SC-012 exists to rule that out.
- **FR-046a**: That target field MUST appear **only when it differs from the caller's project**, so an
  ordinary same-project line is byte-identical to today's (FR-047). It MUST NOT replace or rename the
  existing caller field, which is the one operators already grep.
- **FR-047**: The acting user MUST continue to be identified correctly, and same-project requests MUST
  log exactly as they do today.
- **FR-048**: The cross-project override MUST keep working, and the operation's own record MUST keep
  being attributed to the owning tenant. The fix MUST NOT be achieved by removing the project rebinding
  the override depends on.

#### Boot from a new volume (US9)

- **FR-049**: A launch request whose root disk is a new volume created from an image MUST result in a
  volume being created from that image, and the server booting from it.
- **FR-050**: A requested volume size MUST be honoured.
- **FR-051**: Where no size is requested, the volume MUST be given the server's flavor disk size, and
  never less than the image's stated minimum disk. The attribute MUST NOT be silently discarded.
- **FR-051a**: An image that states no minimum MUST NOT cause a zero-size volume — the flavor's disk
  size governs in that case.
- **FR-052**: A requested size that cannot hold the image MUST be refused clearly, before a volume is
  created. "Cannot hold the image" means **smaller than either** the image's stated minimum disk **or**
  the image's actual size — whichever is larger. The minimum alone is not sufficient, because an image
  may state no minimum at all while still needing several gigabytes.
- **FR-052a**: The same floor MUST be applied when the size was defaulted rather than requested, so
  FR-051 can never produce a volume the image does not fit in.
- **FR-053**: The delete-on-termination choice MUST be honoured on deletion of the server, in both
  directions.
- **FR-054**: The created volume MUST be visible in the volume service, owned by the requesting
  project, marked bootable and in use, and recorded as the server's root attachment.
- **FR-055**: If the volume cannot be created, the boot MUST fail visibly per FR-015 and FR-016 rather
  than falling back to booting the image directly.
- **FR-056**: A request naming an image directly, and a request naming an existing volume, MUST both
  behave exactly as they do today.

### Key Entities

- **Version document**: what a client reads before its first API call to learn the service's supported
  versions and where to reach them. Carries an identifier, a status, a version range, and the link
  collection whose absence breaks the client.
- **Self-link**: the absolute address a response gives for the resource it describes. Produced from a
  scheme, a host, a port and a path; every service produces them, and today by more than one rule.
- **Server address map**: the per-network list of addresses reported for a server, each labelled with
  its kind. The one field users, the dashboard and the infrastructure-as-code provider read to learn a
  server's connectivity.
- **Server reference**: how a client names a server in a request — an identifier or, as a local
  extension, a name. Must resolve identically wherever it is accepted.
- **Instance action record**: the never-amended history of what was done to a server, by whom, under
  which request. Outlives the server it describes; its retention is settled here.
- **Root block device request**: the part of a launch request that describes the server's boot disk —
  its source, its destination, its size and whether it is deleted with the server.
- **Access log entry**: the per-request record naming the caller, the operation and the outcome. Its
  project field is the one an operator greps for cross-tenant activity.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A user who installs a stock client and configures nothing but credentials and the
  authentication address can list flavors, servers, images, volumes and networks — zero commands
  require pinning an API version, and zero fail inside the client library.
- **SC-002**: Setting a per-service endpoint override changes which host the client contacts, in 100%
  of cases, for every service that advertises self-links.
- **SC-003**: Every self-link the product emits is an absolute URL — 0 responses carry a scheme-less or
  empty link — and each names a host and port a client can actually reach. Verified by an exhaustive
  sweep of the emitters rather than by sampling, so "every" is a measured claim.
- **SC-004**: The three affected identity read commands succeed on both the older and the newer stock
  client, and this project's own real-mode runbooks obtain non-empty identifiers on both.
- **SC-005**: Across repeated attempts to create a server whose network cannot be attached, 0 servers
  reach a running state, and 100% report a reason a user can read from the server itself.
- **SC-006**: A server can be stopped and started repeatedly with the reported state matching the
  hypervisor immediately after every transition — 0 divergences across at least ten cycles, with no
  reconciliation needed for any of them. A guest removed out of band is reflected within one
  reconciliation interval, and the server starts successfully on the next attempt with the repair
  visible to an operator without reading service logs line by line.
- **SC-007**: 0 requests in the whole feature's exercise return an unexplained server error: every
  failure is a stated condition with a status that matches it.
- **SC-008**: Every action on the server-action endpoint can be addressed by name — 0 server errors and
  0 actions behaving differently from the rest of the compute surface.
- **SC-009**: A floating IP associated with a server is visible in the server view within one read, and
  the server view, the server listing and the dashboard's instance list all report the same addresses.
- **SC-010**: Reported project usage matches the project's actual resources exactly, and matches the
  quota views for the same resources — 0 disagreements.
- **SC-011**: A deleted server's action history survives its deletion in 100% of cases, and stored
  history older than the configured retention age is absent — both verifiable by measurement after
  repeated create-and-delete cycles, with the retention age set short enough to observe.
- **SC-012**: For a cross-project administrative action, the access log names the operator's project as
  the caller and the target project remains identifiable — verifiable from the log alone, with no
  consulting stored records.
- **SC-013**: The dashboard's default launch path produces a bootable volume of the requested size,
  attached as the server's root disk, in 100% of successful launches — and the delete-on-termination
  choice is honoured in both directions.
- **SC-014**: All existing contract and unit suites pass, including the compute suite that currently
  hangs against a stack where the lifecycle divergence has been triggered.
- **SC-015**: No behaviour outside this feature's stated scope changes — verified by running the other
  services' suites before and after.

## Assumptions

- **A1**: The stock clients are the reference for correctness on US1 and US2. Where a client removes a
  field unconditionally, the field is part of the contract even though no Lightstack code needs it. The fix is
  on the server side; client versions are not pinned to work around it.
- **A2**: The host a client addressed takes precedence over a deployment-configured hostname because a
  deployment behind a proxy or ingress already presents that hostname in the request, so the configured
  value is only needed where the request carries none. This reverses today's precedence and is the
  behavioural change US1 turns on.
- **A3**: The advertised port and the listening port are not assumed equal. The console link's existing
  local derivation exists for this reason and is treated as evidence about the shared rule, not as a
  wart to remove.
- **A4**: Self-links are produced by one shared rule for every service. Fixing them per service is
  explicitly not the approach — the product already has one shared producer and one local workaround,
  and this feature does not add a third.
- **A5**: This feature does not presume a root cause for the missing hypervisor guest in US4. It
  requires that the operation work, that a change the product itself made be recorded immediately, and
  that any other divergence be corrected within a stated interval; identifying why the guest disappears
  is work for the planning phase. Notably, a successful stop is not recorded at all today in real mode,
  which is one confirmed half of the divergence and is independent of whatever removes the guest.
- **A6**: US4's divergence is not an authorization defect. This was established during the sibling
  tenant-scoping work: the owner fails identically on their own server, nothing in that feature touches
  the stop or start paths, and the service was not restarted between the boot and the failure.
- **A7**: US3 is the compute-side half of a network-side defect owned by the sibling network work.
  Fixing that one removes the current trigger; US3 removes the fail-open, and is worth doing on its own
  because the fail-open outlives any single cause.
- **A8**: US5 changes which identifier a sub-handler receives, and nothing about authorization: the
  endpoint already resolves and authorizes the reference before dispatching. Any authorization change
  discovered during the work is reported, not silently applied.
- **A9**: US8's project rebinding is deliberate and load-bearing — the cross-project override depends on
  it. The log is fixed by reading the caller's project before the request is handled, not by removing
  the rebinding.
- **A10**: US6's data is already correct and complete in the network service; this is purely a
  compute-side read that does not consult it. Upstream reports both address kinds in the same
  per-network list, which is the shape adopted.
- **A11**: US9 composes two capabilities the product already has — creating a bootable volume from an
  image, and booting a server from a root volume it attaches. It is an integration, not a new storage
  feature.
- **A12**: US7's dead state filter is removed rather than made live. Nothing writes that state, and
  introducing a soft-deleted server state is a much larger change this feature is not given. The same
  applies to the restore operation that already checks for a soft-deleted state nothing sets — its
  behaviour is unchanged here.
- **A13**: The dashboard is not modified anywhere in this feature. Where a dashboard view is wrong, the
  cause is in the data a service supplies to it.
- **A14**: Where this feature and a sibling ticket touch the same behaviour, the boundaries in *Out of
  Scope* hold, and this feature owns only what is listed here.
- **A15**: US3's answer is deliberately two paths, not one. A network that cannot be used is knowable
  before the create is answered and is refused there; an allocation that fails later cannot be, and
  lands the server in a failed state. The cost is two code paths; the benefit is that the ordinary
  mistake — a wrong network identifier — gets an immediate, readable refusal instead of a background
  failure the user has to poll for.
- **A16**: US4's self-heal needs nothing from the original create request. Every input to a guest's
  definition — its processors, memory and disk from the server's flavor, its network attachments from
  the server's recorded ports, its disk image and boot configuration from the server's own storage — is
  already derivable from stored state. What the product does not have today is a way to assemble that
  definition outside the creation path; making it reusable is the work, not finding the data.
- **A17**: US4's self-heal does not replace the divergence requirement. A start that quietly re-creates
  a guest would otherwise turn a fault into a fixed cost nobody notices, so FR-024 and FR-025b stand
  together: the record must stop being wrong, and the repair must be visible.
- **A18**: US7's retention age defaults to 90 days, an ordinary audit-retention period, and is a
  deployment setting rather than a constant. The default is chosen rather than asked about because any
  bounded value satisfies the requirement and the operator who cares can change it.
- **A19**: US7's reclamation is bounded by record age, not by count, so a deployment's history volume
  is a function of its activity rate rather than of an arbitrary cap that would discard a busy
  deployment's recent history.

## Constraints & Tradeoffs

- **Rejected: an opt-out that restores configured-wins precedence.** It would preserve the defect it
  exists to work around — a client that names its own host would still be redirected — and every
  deployment that genuinely needs a pinned hostname already gets one, because a proxy or ingress puts
  that hostname in the request. Rejecting it keeps one rule rather than two.
- **Rejected: honouring the request host only when a forwarded-host header is present.** Narrower, but
  a client talking directly to the service — which is exactly the endpoint-override case — carries no
  such header, so the defect would survive for the caller who reported it.
- **Accepted cost: an upgrade changes what existing deployments advertise.** FR-003a makes this
  explicit rather than silent. Judged acceptable because the new value is derived from the request the
  client itself made, so it cannot be less reachable than the configured one.
- **Accepted cost: two code paths for a failed network attachment** (FR-016a). The alternative — one
  path — either loses the synchronous refusal for the common mistake or invents a refusal for a fault
  that is not yet knowable.

## Out of Scope

- **The network-side cause of a failed address allocation** — owned by the sibling network work. US3
  makes the failure visible; it does not fix the allocation.
- **Introducing a soft-deleted server state** or upstream's deferred-delete behaviour. US7 removes a
  filter on a state nothing sets; it does not add the state.
- **A general audit-retention framework.** US7 settles retention for one kind of record, by one rule.
- **Structured audit logging** beyond the one misattributed field. US8 is a field fix, not a logging
  redesign.
- **New storage backends or volume features.** US9 uses what exists.
- **Live migration, resize and evacuate correctness** beyond US5's reference handling and US4's
  distinction between an absent target and a fault.
- **Multi-node behaviour.** The real-mode work here is single-node.
- **Client-side changes of any kind**, including pinning or patching a client version, and including
  the dashboard.
- **The cross-tenant authorization model** — settled by the sibling tenant-scoping work already on this
  branch's history and reused, not revisited.
- **Quota enforcement on the volume US9 creates.** Raised during the ambiguity scan and deliberately
  excluded: the volume service enforces no quota on volume creation at all today, so an implicitly
  created volume bypasses no check that exists. Adding the product's first volume-quota enforcement
  inside a compute ticket would be inconsistent and unreviewable in this scope. Worth its own ticket —
  the gap is that volume creation is unmetered, not that this feature makes it worse.
- **Version documents of services other than compute and identity**, except insofar as they consume the
  shared self-link rule, which they all do.

## Dependencies

- **D1**: Builds on the tenant-isolation, tenant-scoping and network-fidelity work already on this
  branch's history. The shared server-reference resolution helper, the administrative override, the
  role checks and the network service's floating-IP allocation are all pre-existing and are reused
  rather than reinvented.
- **D2**: US4 and US9 require a real-mode host with a working hypervisor and elevated privileges. US3
  requires real mode to observe the boot path end to end, though its refusal behaviour is verifiable in
  development mode.
- **D3**: US6 requires a real-mode deployment with a router, an external network and an associated
  floating IP — the fixture the sibling network work established.
- **D4**: US2's acceptance requires the older stock client to be available alongside a newer one, since
  the defect is only observable on the older.
- **D5**: US1's endpoint-override acceptance requires a host reachable under two different names, so
  that an override can be shown to be honoured rather than coincidentally correct.
- **D6**: US7's retention acceptance is measured against a database that has already accumulated
  history from earlier runs; a clean database cannot demonstrate the bound.
