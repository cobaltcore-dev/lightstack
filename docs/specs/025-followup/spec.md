# Feature Specification: SPEC-025 Follow-ups

**Feature Branch**: `feature/547` (feature dir `025-followup`)
**Created**: 2026-09-02
**Status**: Draft
**Input**: `docs/specs/025-nova-gaps-client-discovery/FOLLOWUPS.md` — F1–F8, the findings surfaced while
implementing SPEC-025 and deliberately left out of it.

**No separate ticket.** This is a *pseudo-ticket*: the work is tracked by a comment on
cloud-storage#547 rather than by an issue of its own, and it is implemented on `feature/547`
alongside the feature that found it. Everything else about the workflow is unchanged — spec,
clarify, plan, tasks, implement, host validation, branch review.

## Overview

Seven defects, one shape: **the product answers successfully and does not do what it was asked.**

Two of them discard a client's *intent*. A server listing accepts every filter parameter upstream
documents and applies none of them, so `openstack server list --name web` returns the whole project
and the caller reads the first row as if it were the match. An image PATCH accepts the JSON-Patch
document, applies only its `replace` operations, discards the rest, and answers `200` with the
unchanged image — so `openstack image set --min-disk 10`, which sends `add`, reports success and
changes nothing.

One discards a *limit*. Volume creation consults no quota at all: the `cinder_quotas` table exists,
both quota views report limits and usage from it, and nothing on any create path reads them. A single
project can exhaust the node's storage.

One discards the *client's own address*. The service catalog rewrites every endpoint URL from a
deployment-wide setting, so a client that reached the deployment under one name is told to use
another — and fails at catalog lookup, before it reads a single version document. SPEC-025 fixed
exactly this one level up, for self-links.

One advertises an action that **cannot** succeed: `restore` requires a status the product never
writes, because every delete is a hard delete. Upstream is in the same position at its default reclaim
interval and keeps the action routed, so the product keeps upstream's answers rather than withdrawing it.

One advertises a *negotiation* that does not happen: microversions 2.1–2.90 are offered, the request
header was never read before SPEC-025, and comments in the code describe behaviour as
version-gated when it is unconditional.

And one is a straightforward cost: a listing of N servers issues N address queries.

The thread is the same one that runs through SPEC-025 and its sibling network work, and it is why none
of these was caught by a contract suite: **the status code is correct.** `200` for a listing of the
wrong rows, `200` for a patch that wrote nothing, `202` for a volume that should have been refused,
`200` for a catalog a client cannot use. Five of the seven were found by a human running a real host,
and two of those (F7, F8) were found only because they *disguised themselves as other bugs* — F7 looked
like the address map SPEC-025 had just fixed disagreeing with itself, F8 looked like the project
scoping matching zero rows.

That has a direct consequence for how this feature is tested, stated once here because it applies to
every requirement below: **a test that exercises only the working case passes against the broken
code.** `?name=probe` returns probe today. A patch returns `200` today. Under-quota creation succeeds
today. So every requirement in this spec is paired with a *negative* assertion — a non-matching
filter returns nothing, a patched value is read back, an over-limit request is refused and leaves
nothing behind. Nine of SPEC-025's own verification steps had to be rewritten for exactly this reason.

**F5 is not in scope and needs no work.** The unreachable `ResizeInstance` handler it records was
already removed on this branch; it is in FOLLOWUPS.md for the audit trail, so that its raw
`c.Param("id")` read is not mistaken for a surviving instance of B14 in a diff.

## Clarifications

### Session 2026-09-02

- Q: Is deferred delete implemented, or is the `restore` action withdrawn? → A: **Withdrawn.**
  *(Superseded 2026-09-30, below: deferred delete stays out, but `restore` stays routed.)* The
  handler is removed, so `{"restore": null}` receives the action dispatcher's answer for an action it
  does not know. Upstream's own default is the same behaviour: its api-ref states the instance "is
  hard deleted immediately" unless `reclaim_instance_interval` is set above 0, in which case the soft
  path is enabled. The product's *behaviour* therefore already matches a default upstream deployment —
  the defect is a handler that advertises otherwise. Implementing deferred delete would add a
  persisted state, a retention window, a reclaim sweep and a quota interaction (upstream keeps
  charging quota until reclaim) for a capability upstream ships disabled.

- Q: How far does microversion gating go — every behaviour upstream gates across 2.1–2.90, or only
  those the product implements? → A: **Bounded to what the product implements.** The audit covers the
  response fields and behaviours Lightstack actually produces; those upstream introduced part-way through
  the advertised range get a gate, and anything found gated upstream but left ungated here is
  recorded rather than silently skipped. This bounds the work by the product's own surface — roughly a
  dozen response fields — instead of by upstream's ninety-version history, most of which gates
  behaviour Lightstack does not implement at all. It still fixes the verifiable defects: the comments
  claiming gates that do not exist, and a range the product cannot honour.

- Q: Does the service catalog echo the request host the way self-links now do, or does it remain a
  deployment-wide statement? → A: **Echo the request host, configured host as the fallback** — the
  same precedence SPEC-025 established for self-links. The catalog is the *earlier* of the two
  failures: a client reads it before any version document, so fixing self-links alone left every
  command failing one step sooner. Two consequences were surfaced while deciding and are now
  requirements rather than notes: the catalog's 24-hour cache is keyed on the project alone, and
  since the request host is client-controlled a shared cache would let one caller's host be served
  to another (FR-031a); and nothing in the product reads `X-Forwarded-Host`, so a proxy that
  rewrites `Host` still advertises an internal name (FR-031b). The claim that this story leaves the
  cache untouched was withdrawn from Out of Scope as a result.

- Q: Which block-storage resources are enforced in this feature? → A: **All six the quota views
  report** — `volumes`, `gigabytes`, `per_volume_gigabytes`, `snapshots`, `backups`,
  `backup_gigabytes` — plus volume extension against the gigabytes allowance. Nothing is reported
  without being enforced. This is the larger of the options considered, and deliberately: a reported
  limit that is not enforced is the defect this story exists to fix, so leaving three of six in that
  state would reproduce it. **Scoping this answer surfaced that FR-019's "every path" is wider than
  the spec knew** — restoring a backup into a new volume and importing a managed volume each insert a
  volume, so there are four volume-creating routes rather than two.
- Q: Does snapshot size count toward the `gigabytes` allowance? → A: **Yes, matching upstream** —
  whose configuration reference describes the allowance as "total gigabytes for volumes and
  snapshots". The existing counter reads volume size only, so the counter is corrected rather than
  the enforcement weakened to fit it: a snapshot occupies backend storage, and excluding it would
  leave a project able to exceed its storage allowance without exceeding its quota. The cost is a
  **visible number changing** — reported usage rises for any project holding snapshots, and some may
  read as over their limit immediately, which is recorded as a release-note item rather than
  smoothed over.
- Q: Is accepting a transferred volume enforced, given it provisions nothing? → A: **Yes** — FR-019
  is widened from "provisions a volume" to "adds a volume to a project". From the receiving side an
  accept is indistinguishable from a create: same count, same gigabytes, same storage. Leaving it out
  would keep a route by which a project at its limit acquires more, and a transfer is trivially
  initiated by any project willing to hand one over. A refused accept leaves the transfer pending and
  the volume with its sender (FR-019a), so "leaves nothing behind" had to be restated: no *new* state
  for a create, no *lost* state for a transfer.
- Q: What happens to a server-list filter naming an attribute the product does not record? → A:
  **Refuse with a client error naming the filter.** Better to fail than to answer. This is a
  **deliberate divergence from upstream**, which supports `host`, `availability_zone` and tags and
  would return matching servers — accepted knowingly, because the alternative answers are an empty
  list that reads as a fact about the deployment or an ignored filter, and the ignored filter is the
  defect this story exists to remove. The requirement it replaces was unfollowable: it said to answer
  "the way upstream answers it", and upstream *honours* these filters, which the product cannot.
  **Bounded** by FR-004a to the enumerated refused filters — a parameter that is neither honoured
  nor enumerated stays ignored, so no client is newly refused for sending a benign extra.
- Q: What does a `remove` operation mean for the image properties a patch may write? → A: **Refused
  for all of them**, with a client error naming the path (FR-011). All four are base properties
  carrying a schema-defined default, so none has an absent state to restore; resetting one to its
  default would make `remove` a synonym for a write the client did not request — a visibility reset
  dressed as a deletion. Refusing is also what upstream does for a base property, and it preserves
  what the story is for: the client is told, rather than receiving success and no change.
- Q: Does honouring microversions start refusing requests that succeed today? → A: **Only for a
  version outside the advertised range.** A client asking for more than the deployment offers is
  refused, as upstream refuses it, because telling it to negotiate down is the mechanism working. A
  header that merely fails to **parse** keeps being treated as no negotiation (FR-041a) — SPEC-025
  decided that deliberately and recorded it as A-006, and FR-040 requires existing callers not to
  change. The requirement as originally written would have reversed a decision the code documents,
  which is why the two cases are now separated: upstream itself answers them differently (406 versus
  400), so "answer the way upstream answers it" was never one instruction.
- Q: Are the refusals this feature introduces recorded anywhere? → A: **Quota denials are; the
  client-error refusals are not** (FR-029a). A denial names the project, resource, limit and
  requested amount, because it is actionable by whoever configured the limit and is invisible to them
  otherwise. A refused filter, a rejected patch operation and an out-of-range microversion are
  already in the caller's hands as errors, and recording each would let a looping client fill the log
  to restate what it was told. The question was raised by F8's own history: the discarded patch
  operations left no trace at any level, which is why nobody found them until a value was read back.
- Q: What does an administrator's project filter do without the cross-tenant opt-in? → A: **It
  narrows within their own scope, so the answer is an empty collection** (FR-006a). No special case:
  scope intersected with filter, and an administrator wanting another project's servers passes the
  opt-in as well, which is what upstream requires. Treating the filter as implying the opt-in was
  rejected because it would make a filter widen scope — the one thing FR-006 forbids — and would make
  this parameter behave unlike every other filter. The residual risk is a dashboard administrative
  view that filters by project without the opt-in and would show an empty page; that is a
  host-validation check, not a reason to special-case the rule.

### Session 2026-09-30

- Q: Does `restore` receive the unknown-action answer (400), or upstream's (404/409)? → A:
  **Upstream's.** This supersedes the withdrawal above. Upstream routes `restore` at every
  configuration (`nova/api/openstack/compute/deferred_delete.py`, expected errors 403/404/409): at
  the default reclaim interval, a deleted server is 404 and a live one 409 (`InstanceInvalidState`).
  Its 400 "There is no such action" is for names it does not route. So the earlier premise held for
  deletion semantics but not for the answer a client receives. The gophercloud contract test, which
  expects 404 after a delete, caught the difference. Deferred delete stays out; only the handler's
  unreachable success branch goes.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Narrow a server listing and get the servers asked for (Priority: P1)

An operator or a script runs `openstack server list --name web`, or types into the dashboard's search
box, and gets the servers whose name matches. A name that matches nothing returns nothing.

**Why this priority**: this is the highest-severity item in the set, and higher than it reads. A
listing that ignores its filter does not fail — it returns a well-formed page of *other* servers, and
every client that trusts `--name` to narrow the result then reads another server's state. The same
shape has now been found three times in this product: `os-simple-tenant-usage` leaked another
tenant's usage (SPEC-023), six identity collections returned the whole collection whatever was asked
(SPEC-023 US5, and it is what let the multi-tenancy suite grant the administrative role to both of
its test users for as long as it did), and now compute. It is also the most-used filter in the
product: it is what both the CLI and the dashboard send.

**Independent Test**: list servers with a name that exists and a name that does not; the second must
return an empty collection. Fully independent of every other story.

**Acceptance Scenarios**:

1. **Given** a project with servers `web`, `db` and `cache`, **When** the caller lists servers with
   `?name=web`, **Then** only `web` is returned.
2. **Given** the same project, **When** the caller lists servers with `?name=doesnotexist`, **Then**
   an empty collection is returned — not the project's servers.
3. **Given** servers in `ACTIVE` and `ERROR` states, **When** the caller filters by `?status=ERROR`,
   **Then** only the failed server is returned.
4. **Given** a filter parameter combined with paging, **When** the caller pages through the filtered
   result with a marker, **Then** each page contains only matching servers and no server appears
   twice or is skipped.
5. **Given** a non-administrator, **When** they pass a filter naming another project, **Then** they
   see only their own servers — a filter narrows a listing and may never widen it.
6. **Given** a request with no filter parameters at all, **When** the caller lists servers, **Then**
   the result is identical to what the listing returned before this feature.
7. **Given** an administrator filtering by an attribute the product does not record — a hypervisor
   host, say — **When** they list servers, **Then** they are refused with an error naming that
   filter, rather than handed a list.
8. **Given** a client that sends a query parameter which is neither a supported filter nor a named
   unsupported one, **When** it lists servers, **Then** the parameter is ignored and the request
   succeeds, exactly as it does today.

---

### User Story 2 — Set an image property and have it stick (Priority: P1)

An operator runs `openstack image set --min-disk 10 cirros` and the image's minimum disk becomes 10.
If the request cannot be honoured, they are told so.

**Why this priority**: `image set` is the documented way to set an image property, and for a property
that has no value yet the client sends `add` — which is discarded. The failure is silent in both
directions: the client is told `200`, and nothing is logged, because the only log line in the handler
fires when a `replace` UPDATE *errors*. An operator's only way to notice is to read the value back
and compare, which is how this was found. It also blocks a real workflow: SPEC-025's own size-floor
verification needed `min_disk` raised and could not raise it.

**Independent Test**: patch a property with `add`, then read the image back and compare. Independent
of every other story.

**Acceptance Scenarios**:

1. **Given** an image with `min_disk` unset, **When** a patch document with `{"op":"add",
   "path":"/min_disk","value":7}` is applied, **Then** the response reports `min_disk` as 7 and a
   subsequent read agrees.
2. **Given** an image with `min_disk` 7, **When** `{"op":"replace","path":"/min_disk","value":10}` is
   applied, **Then** it becomes 10 (today's behaviour, preserved).
3. **Given** an image, **When** a patch names an operation the service does not support, **Then** the
   request is refused with a message naming the operation — not answered `200` with the image
   unchanged.
4. **Given** an image, **When** a patch asks to `remove` any writable property, **Then** the request
   is refused naming the path, and the property keeps its value — it is not reset to a default.
5. **Given** an image, **When** a patch names a path the service cannot write, **Then** the request is
   refused naming the path.
6. **Given** a patch document with several operations of which one cannot be applied, **Then** none of
   them is applied and the image is unchanged.
7. **Given** an image id that does not exist or belongs to another project, **When** a patch is
   applied, **Then** the request is refused as not found — not answered `200`.

---

### User Story 3 — A project cannot exceed its block-storage quota (Priority: P1)

An operator sets a project's volume quota. Requests that would exceed it are refused, and the
project's consumption stays inside the limit the quota views report.

**Why this priority**: this is the only item with a resource-exhaustion consequence — a tenant can
fill the node's storage or a Ceph pool, and no deployment that is not single-tenant should run
without it. It is also the only one that requires new enforcement rather than the honouring of an
existing intent, which is why FOLLOWUPS.md argued it could not be reviewed inside a compute ticket.

**Note on where the check belongs**: the volume API's create handler is not the only way a volume
enters a project. The compute service's boot-from-new-volume path added by SPEC-025 (FR-049 there)
reaches the same provisioning routine directly; restoring a backup and importing a managed volume
each insert a volume of their own; and accepting a transfer moves one in without creating anything at
all. A check placed in the API handler alone is bypassed by all four. The compute path also carries no
HTTP request context, so the check cannot take the shape the compute service's own quota check takes.

The question the story turns on is therefore not "where is the create handler" but **"what makes a
project's usage go up"** — and the answers are spread across four files.

**Independent Test**: set a project's volume limit to its current usage, then attempt one more volume
through each creation path; both must be refused and neither may leave a volume or a backing store
behind. Independent of every other story.

**Acceptance Scenarios**:

1. **Given** a project at its `volumes` limit, **When** a volume is created through the volume API,
   **Then** the request is refused as over limit, naming the resource.
2. **Given** a project at its `volumes` limit, **When** a server is booted from a new volume, **Then**
   the boot is refused and no volume exists afterwards.
3. **Given** a project whose remaining `gigabytes` allowance is 5, **When** a 10 GB volume is
   requested, **Then** it is refused even though the `volumes` count allows another volume.
4. **Given** a refused request, **When** the project's volumes are listed and the backing store
   inspected, **Then** no partial volume and no orphaned backing object exists.
5. **Given** a project one volume below its limit, **When** two creations are issued concurrently,
   **Then** exactly one succeeds.
6. **Given** an administrator who raises the project's limit, **When** the next creation is attempted,
   **Then** it succeeds without restarting the service.
7. **Given** a project under its limits, **When** volumes are created, **Then** behaviour is unchanged
   from today.

---

### User Story 4 — Reach the deployment under the name it was addressed by (Priority: P2)

A client that resolves and reaches the deployment under one hostname is given a catalog whose
endpoints are under that same hostname, so the commands that follow the token request work.

**Why this priority**: the failure is total but its trigger is a misconfiguration rather than a code
path — when the configured hostname names something the client cannot resolve, *every* command fails
at catalog lookup, before any version document is read. It is currently documented as a deployment
constraint in `docs/CONFIGURATION.md` and in `test/nova_gaps_test.sh`, which is a workaround, not a
fix. It sits behind the three P1 stories because a correctly configured deployment does not hit it.

**This is the earlier of two failures, and only the later one is fixed.** A client reads the catalog
before it reads any version document, so a deployment whose configured host is unreachable fails one
step *before* the self-links SPEC-025 corrected are ever fetched. Fixing self-links and leaving the
catalog means the fix is unreachable in exactly the deployments that needed it.

**Independent Test**: request a token addressing the service under a hostname other than the
configured one, and inspect the returned catalog. Independent of every other story.

**Acceptance Scenarios**:

1. **Given** a deployment whose configured endpoint hostname differs from the address a client used,
   **When** the client requests a token, **Then** the catalog it receives is resolvable by that client.
2. **Given** a request carrying no usable hostname, **When** a token is issued, **Then** the catalog
   still names an absolute, parseable endpoint for every service.
3. **Given** the external object-storage endpoint, **When** a catalog is served, **Then** it is
   unchanged by this story — it is configured out of band and is not the deployment's own address.
4. **Given** a deployment behind a proxy or ingress that preserves the addressed host, **When** a
   token is issued, **Then** the catalog names the address clients use, not the pod's.
5. **Given** two clients in one project that addressed the deployment under different hostnames,
   **When** each requests a token, **Then** each receives a catalog naming its own hostname and
   neither is served the other's.

---

### User Story 5 — `restore` answers as upstream does (Priority: P3)

A client that invokes `restore` gets the answer an upstream deployment gives at its default reclaim
interval: the server is gone (404), or it exists and is not soft-deleted (409). The action stays
routed, as upstream keeps it at every configuration; deferred delete is not introduced.

**Why this priority**: misleading rather than harmful — the action cannot succeed, so nothing can
break through it. It is last-but-one because the change is a cleanup rather than a repair: the
deletion semantics stay as they are, which is what a default upstream deployment does.

**Independent Test**: invoke the restore action on a live server and on a deleted one, and confirm
upstream's answers — 409 and 404 — never the unknown-action 400.

**Acceptance Scenarios**:

1. **Given** a server in any state the product actually writes, **When** the restore action is
   invoked, **Then** the client receives 409, upstream's answer for a server that is not
   soft-deleted — not the answer for an unknown action.
2. **Given** a deleted server, **When** the restore action is invoked, **Then** the client receives
   404, as from an upstream deployment where deletion is immediate.
3. **Given** a deleted server, **When** it is addressed at all, **Then** it is gone — deletion
   semantics are unchanged by this story.

---

### User Story 6 — Negotiate a microversion and get what was negotiated (Priority: P3)

A client that negotiates a compute microversion receives the behaviour that version defines: not more,
not less. A client that negotiates nothing keeps receiving what it receives today.

**Why this priority**: a real Article I fidelity gap — the product advertises a range and honours one
point in it — but nothing currently *breaks* because of it: the product's unconditional behaviour is
the newest behaviour, so clients get more than they asked for rather than less. It is last because it
is the only item whose cost is dominated by an audit rather than a change, and it stays separable
from the other six.

**Bounded by the product's surface, not upstream's history** (FR-042): the audit covers what Lightstack
actually produces, so it is roughly a dozen response fields rather than ninety versions of upstream
changelog. The advertised range is not narrowed to match what the audit finds.

**Independent Test**: request a listing or a server action at an older negotiated version and at the
current one, and confirm the responses differ exactly where upstream says they differ.

**Acceptance Scenarios**:

1. **Given** a client that negotiates the product's minimum version, **When** it reads a response
   whose shape upstream changed later in the range, **Then** it receives the older shape.
2. **Given** a client that negotiates nothing, **When** it makes any request, **Then** it receives
   today's behaviour unchanged.
3. **Given** a client that negotiates a version above the advertised maximum, **When** it makes a
   request, **Then** it is refused, so it can negotiate down.
4. **Given** a client whose version header is malformed rather than out of range, **When** it makes a
   request, **Then** the request succeeds and is treated as no negotiation — unchanged from today.
5. **Given** any response from the compute service, **When** the client inspects the version header,
   **Then** it names the version actually applied.

---

### User Story 7 — A server listing does not slow down in proportion to the servers in it (Priority: P3)

Listing servers costs the same number of round trips whether the project has three servers or three
hundred.

**Why this priority**: correctness is unaffected and no deployment has reported slowness, so this is
the one item in the set that is purely a cost. It is included because the query it rewrites is the
same one US1 extends — doing it separately means editing that path twice, and the address map it
builds carries SPEC-025's floating-IP join, which is easy to get subtly wrong in a rewrite and easy
to verify against the per-server form while both exist.

**Independent Test**: list a project with several servers and assert the address map is identical to
the per-server form while the number of address queries is independent of the server count.

**Acceptance Scenarios**:

1. **Given** a project with several servers, **When** the detailed listing is read, **Then** each
   server's addresses are exactly what the per-server view reports, floating IPs included.
2. **Given** projects of different sizes, **When** their detailed listings are read, **Then** the
   number of address queries does not grow with the number of servers.
3. **Given** a server with no ports, **When** the listing is read, **Then** its address map is empty
   rather than absent or null.

---

### Edge Cases

- A filter parameter present but empty (`?name=`) — treated as absent, so the listing is not narrowed
  to nothing by a client that sent an unset variable.
- A filter value that is syntactically invalid for its column (`?flavor=not-a-uuid`) — must not
  produce a database error surfaced as a server fault.
- A filter naming another project, sent by an administrator *without* the cross-tenant opt-in — the
  scope stays the caller's project; a filter cannot substitute for the opt-in.
- A patch document that is valid JSON but not an array of operations.
- A patch operation with a `path` inside the allowlist but a `value` of the wrong type
  (`{"op":"replace","path":"/min_disk","value":"ten"}`).
- A `remove` operation on a property that cannot be absent — the image's name is required.
- A volume request whose size alone exceeds the project's entire allowance.
- A volume request that is exactly at the limit rather than over it (off-by-one in both directions).
- Quota enforcement when the project has no quota row at all — the code defaults must apply, and must
  be the same defaults the quota views report.
- A quota refusal on the boot path — the server must not be left in a half-created state.
- A project already past its `gigabytes` allowance once snapshot size is counted — existing resources
  stay, further creation is refused, and nothing is deleted to bring the project back under.
- A snapshot of a volume whose size alone fills the remaining gigabytes allowance.
- Deleting a volume that has snapshots — the released gigabytes must be the volume's, not the
  snapshots', which survive it.
- A transfer accepted by a project that has room for the count but not the gigabytes, and the reverse.
- A transfer refused for quota, then retried after the limit is raised — the transfer must still be
  acceptable, not consumed by the failed attempt.
- A catalog served for a request whose Host header carries a port, an IPv6 literal, or nothing at all.
- A microversion header naming a service other than compute, or naming `latest`, or unparseable.
- A detailed listing whose servers span several projects (an administrator's cross-tenant view) —
  addresses must stay attributed to the right server.

## Requirements *(mandatory)*

### Functional Requirements

#### Server listing filters (F7 — User Story 1)

- **FR-001**: Server listings MUST apply every filter parameter they accept to the rows returned, so
  that a parameter matching nothing yields an empty collection rather than an unfiltered page.
- **FR-002**: Both the brief and the detailed server listing MUST honour the same filters — a client
  must not get different rows for the same query depending on which representation it asked for.
- **FR-003**: The honoured set MUST cover the filters stock clients actually send, restricted to
  attributes the product records. **Pinned against upstream during planning** (research R1), which
  splits the set by role: `name`, `status`, `image`, `flavor` and `changes-since` MUST be honoured for
  every caller, and `user_id`, `project_id`/`tenant_id`, `key_name`, `locked`, `task_state` and
  `power_state` for administrators.
- **FR-003a**: `name` MUST match as a **regular expression** over the display name, which is
  upstream's semantics ("exact and regex matching"). A substring match is not acceptable: it would
  return rows a regex would not, which is the same class of wrong-but-plausible answer this story
  exists to remove. An invalid pattern MUST be answered as a client error rather than reaching the
  database as a fault (FR-008).
- **FR-003b**: `ip` is in upstream's general-caller set and the product **does** record the data, in
  the ports' fixed addresses — but not in a column, so honouring it costs a subquery. It MUST be
  honoured once the batched address lookup of FR-045 exists, and MUST be refused under FR-004 until
  then. It MUST NOT be ignored in either state.
- **FR-004**: A filter naming an attribute upstream supports but the product does not record MUST be
  **refused with a client error naming that filter**. The enumerated set is exactly:
  **`host`, `hostname`, `availability_zone`, `tags`**, and **`ip`** until FR-003b's condition is met.
  The product records no host or hypervisor assignment, no availability zone and no tags for a
  server, so those filters cannot be honoured — and an answer that looks like a result is worse than
  a refusal, because the caller cannot tell an empty match from an ignored filter.

  This is a **deliberate divergence from upstream**, and research R1 established that it is
  two-sided: upstream *honours* `host` for administrators and **silently ignores** it for every other
  caller ("any requests from non-admin users will be ignored"), so the product refuses both where
  upstream would answer one and ignore the other. `hostname` is in the set for the same reason and is
  non-admin-accessible upstream from microversion 2.90 — the exact version this product advertises as
  its maximum. The divergence is accepted so that the failure is visible rather than plausible.
- **FR-004a**: The refusal MUST be bounded to that enumerated set. A query parameter that is neither
  honoured nor in it MUST continue to be ignored, as it is today. Refusing everything unrecognised
  would be a new rejection for every client that sends a benign extra parameter — paging, sorting and
  deleted-filter parameters among them — and would contradict FR-007's requirement that an existing
  caller's request behave as it does now.
- **FR-005**: Filters MUST compose with each other, with project scoping, and with paging: the marker
  lookup MUST run under the same predicates as the page query, so a filtered listing cannot silently
  restart at page one.
- **FR-006**: A filter MUST only ever narrow a listing. No filter parameter may widen scope beyond
  what the caller is already entitled to see; the administrator-only cross-tenant opt-in remains the
  only widening mechanism.
- **FR-006a**: This holds for administrators too. A project filter naming another project, sent
  without the cross-tenant opt-in, MUST be applied on top of the caller's own scope — yielding an
  empty collection, which is the correct answer to a request for another project's servers made
  without asking to see other projects. The filter MUST NOT imply the opt-in: that would make a
  filter widen scope, and would make this one parameter behave unlike every other filter.
- **FR-007**: A request carrying no filter parameters MUST produce the same result, in the same order,
  as before this feature.
- **FR-008**: An invalid filter value MUST be refused or ignored deliberately, and MUST NOT surface as
  a server-side fault.
- **FR-009**: Each honoured filter MUST be pinned by a test that asserts a **non-matching** value
  returns an empty collection. A test that asserts only the matching case is not acceptable evidence,
  because it passes against the unfiltered behaviour this requirement replaces. Each named
  refused filter MUST be pinned by a test asserting the refusal, and the ignored-extra case
  (FR-004a) by a test asserting the request still succeeds.

#### Image patch operations (F8 — User Story 2)

- **FR-010**: An `add` operation on a writable image property MUST write the value.
- **FR-011**: A `remove` operation MUST be refused with a client error naming the path, for every
  path the product allows a patch to write. All of them are base properties carrying a schema-defined
  default, so none has an absent state to be restored to, and resetting one to its default would make
  `remove` a synonym for a write the client did not ask for — a visibility reset dressed as a
  deletion. Refusing is also the answer upstream gives for a base property. What matters for this
  story is preserved either way: the client is told, instead of receiving success and no change.
- **FR-012**: An operation the service does not implement MUST be refused with a client error naming
  the operation, never answered as success with the resource unchanged.
- **FR-013**: A path the service cannot write MUST be refused with a client error naming the path.
  This includes the paths the current handler silently discards alongside the unsupported
  operations — arbitrary custom properties among them.
- **FR-014**: A patch document MUST be applied atomically: if any operation in it cannot be applied,
  none is applied.
- **FR-015**: A patch whose target image does not exist, or does not belong to the caller, MUST be
  refused as not found. A write matching zero rows MUST NOT be answered as success.
- **FR-016**: The patch response MUST reflect the applied state of the image.
- **FR-017**: Each supported operation MUST be pinned by a test that reads the property back. A test
  asserting only the response status is not acceptable evidence, because it passes against the
  discard behaviour this requirement replaces.

#### Block-storage quota enforcement (F1 — User Story 3)

- **FR-018**: Volume creation MUST be refused when it would take the project beyond its volume-count
  or total-gigabytes allowance.
- **FR-019**: Enforcement MUST apply to every path that **adds a volume to a project** — not only
  those that provision one — so that no route bypasses the check. There are **more than the two the
  volume API exposes**: the volume-create handler, the compute service's boot-from-new-volume path,
  restoring a backup into a new volume, importing an existing backing store as a managed volume, and
  accepting a transferred volume. The last provisions nothing, yet the accepting project's count and
  gigabytes rise by the volume's size, which is indistinguishable from a create on the receiving
  side. Each MUST be covered, and planning MUST re-derive the list from the code rather than trusting
  this enumeration — a route added later and left unchecked reproduces the defect exactly.
- **FR-019a**: A refusal on a path that did not create the volume MUST leave the prior owner's state
  intact: a transfer refused for quota stays pending, the volume stays with the sender, and nothing
  is deleted. "Leaves nothing behind" means no *new* state for a create, and no *lost* state for a
  transfer.
- **FR-020**: A refusal MUST use the over-limit answer stock clients already understand, naming the
  resource that was exhausted.
- **FR-021**: A refused request MUST leave no volume record and no backing store behind.
- **FR-022**: A refused boot MUST leave no server in a state that implies a volume exists.
- **FR-023**: The check MUST be safe under concurrent requests: two simultaneous creations against a
  single remaining allowance MUST NOT both succeed. This MUST hold for a project with **no**
  configured quota row as well as one with — a project running on the code defaults has no row to
  lock, so a lock-the-row implementation would give the guarantee only to projects an administrator
  has already touched.
- **FR-024**: A single request whose size exceeds the per-volume ceiling MUST be refused on that
  ground, independently of the project's remaining total.
- **FR-025**: Enforced limits MUST resolve from the same source the quota views report, so
  enforcement and reporting cannot disagree — including the code defaults used when a project has no
  configured quota.
- **FR-026**: Counted usage MUST agree with the usage the quota views report for the same project,
  and MUST describe actual consumption. The `gigabytes` allowance therefore counts **volume and
  snapshot size together**, as upstream does — the existing counter reads volume size only, so it is
  the counter that changes, not the enforcement that bends to match it. A snapshot occupies backend
  storage, so excluding it would leave an unbounded path past a limit that claims to bound storage.
- **FR-026a**: The changed count MUST be reflected everywhere usage is reported — the quota-set
  views and the absolute-limits view read one resolution, and MUST continue to.
- **FR-027**: A limit changed by an administrator MUST take effect for the next request, with no
  restart.
- **FR-028**: **All six** reported resources MUST be enforced — `volumes`, `gigabytes`,
  `per_volume_gigabytes`, `snapshots`, `backups` and `backup_gigabytes` — so that nothing the quota
  views report is reported without being enforced. Volume extension MUST be enforced against the
  gigabytes allowance for the size it adds, not the size it results in.
- **FR-028a**: Snapshot creation MUST be refused against the snapshot count allowance, and backup
  creation against the backup count and backup-gigabytes allowances, each on the same terms as volume
  creation: the refusal leaves nothing behind, and the check is race-free.
- **FR-029**: Enforcement MUST be pinned by tests that assert refusal at the limit and success below
  it **for each enforced resource**, and refusal through the compute boot path — the last being the
  one a handler-level test cannot see.
- **FR-029a**: A quota denial MUST be recorded as an operator-visible event naming the project, the
  resource, the configured limit and the amount requested. A project hitting its ceiling is
  actionable by whoever set the ceiling, and it is the one refusal in this feature that no one sees
  unless it is recorded — the client sees its own error, the operator sees nothing. The refusals
  introduced elsewhere in this feature — a refused filter, a rejected patch operation, an
  out-of-range microversion — are NOT recorded: each is already in the caller's hands as a client
  error, and logging them would let any looping client fill the log while restating what it was
  already told.

#### Service catalog addressing (F4 — User Story 4)

- **FR-030**: The endpoints in a served catalog MUST be resolvable by the client that received it.
- **FR-031**: A served catalog MUST name the host the client addressed. The configured endpoint host
  becomes the fallback, used when a request carries no usable host — the same precedence, and the
  same fallback chain, that self-links already follow.
- **FR-031a**: A catalog derived from one client's request host MUST NOT be served to a client that
  addressed a different host. The request host is client-controlled, so a catalog cached under the
  project alone would let one caller determine the endpoints another caller is handed — and therefore
  where that caller sends its token. Either the cache key distinguishes the host, or a host-derived
  catalog is not cached. Caching is off by default, which bounds the exposure but does not remove the
  requirement.
- **FR-031b**: Whether a forwarded-host header is trusted MUST be decided explicitly rather than by
  omission. Nothing reads one today, so a proxy that replaces `Host` with a backend name will have
  its backend name advertised; trusting a forwarded header instead raises the same
  client-controlled-value question as FR-031a and MUST be answered on the same terms.
- **FR-032**: Every catalog endpoint MUST remain absolute and parseable even for a request that
  carries no usable host.
- **FR-033**: The externally configured object-storage endpoint MUST continue to be served as
  configured, unaffected by this story.
- **FR-034**: Catalog addressing MUST be pinned by tests that request a catalog under a host other
  than the configured one and assert what the client is told, and that request catalogs under two
  different hosts in succession and assert neither is served the other's.

#### `restore` answers (F2 — User Story 5)

- **FR-035**: The `restore` action MUST stay routed and MUST answer as upstream does at its default
  reclaim interval: 404 for a server that does not exist, 409 for any server that does. It MUST NOT
  receive the unknown-action answer — upstream routes it at every configuration, and reserves its 400
  "There is no such action" for names it does not route.
- **FR-036**: Deferred delete is NOT introduced — deletion stays immediate, which is what an upstream
  deployment does at the default reclaim interval. The handler's success branch, which gated on a
  soft-deleted status nothing writes, is removed. The change MUST NOT change the answer for any other
  action, and MUST NOT change deletion semantics.
- **FR-037**: The outcome MUST be pinned by a test asserting 409 for a live server and 404 for a
  deleted one, and by the gophercloud contract test's restore-after-delete case.

#### Microversion negotiation (F3 — User Story 6)

- **FR-038**: The compute service MUST read the negotiated microversion from the request in one place,
  and MUST report the version actually applied in its response.
- **FR-039**: A behaviour that upstream introduced part-way through the advertised range MUST NOT be
  served to a client that negotiated a version below it.
- **FR-040**: A client that negotiates nothing MUST keep receiving today's behaviour, so that no
  existing caller changes when negotiation begins to be honoured.
- **FR-040a**: Concretely, a request carrying **no version header** MUST continue to resolve to the
  advertised **maximum** version. This is a **third deliberate divergence from upstream**, which
  assumes the *minimum* supported microversion when no header is sent — found in research (R4) rather
  than known when this spec was written, and **pre-existing** rather than introduced here. Adopting
  upstream's default would remove behaviour from every current caller in a single release, which
  FR-040 forbids.

  It MUST be recorded rather than left implicit, because it **bounds what this story can deliver**:
  with no-header resolving to the maximum, every gate FR-039 introduces affects only a client that
  negotiates *explicitly*. A stock client that sends no header keeps receiving the newest shape of
  every response. FR-044's tests MUST therefore negotiate explicitly — a test relying on the default
  header exercises the maximum and proves nothing about a gate.
- **FR-041**: A negotiated version **outside the advertised range** MUST be refused, as upstream
  refuses it, so that a client asking for behaviour the deployment does not offer is told to
  negotiate down rather than silently handed something else.
- **FR-041a**: A version header that does not **parse** MUST continue to be treated as no
  negotiation, not as a refusal. SPEC-025 took that decision deliberately (recorded as A-006 in
  `internal/nova/microversion.go`): the gate exists to avoid granting more than a client negotiated,
  and that property holds without inventing a rejection on a surface that never had one. Nothing in
  this feature is a reason to reverse it, and FR-040 requires existing callers not to change.
  Upstream answers 400 here; the divergence is narrow, deliberate and already documented in the code.
- **FR-042**: The audit MUST be bounded to behaviours the product implements: the response fields and
  action outcomes Lightstack actually produces, checked against the version at which upstream introduced
  them. A behaviour upstream gates but the product does not implement MUST NOT acquire a gate, and
  the advertised range is not narrowed to match the audit's findings.
- **FR-043**: The audit's outcome MUST be recorded — including the behaviours found to be gated
  upstream and deliberately left ungated here — so that the next reader does not re-derive it, and so
  that comments claiming a gate that does not exist are corrected rather than reproduced.
- **FR-044**: Each gate introduced MUST be pinned by a test at both sides of its boundary.

#### Listing address cost (F6 — User Story 7)

- **FR-045**: The number of address lookups a detailed server listing performs MUST be independent of
  the number of servers listed.
- **FR-046**: The addresses reported for each server MUST be identical to those the per-server view
  reports, floating IPs included.
- **FR-047**: A server with no ports MUST report an empty address map.
- **FR-048**: Equivalence with the per-server form MUST be pinned by a test, and the round-trip count
  MUST be asserted rather than assumed.

### Key Entities

No new entities. The feature reads and writes what already exists:

- **Block-storage quota** — the per-project limits already recorded and already reported by both quota
  views, gaining a reader on the creation paths.
- **Image property** — the small set of image attributes a patch may write, gaining the operations
  the patch media type defines.
- **Negotiated microversion** — the version a client asks for, currently read at exactly one decision
  point in the product.

**No new persisted state.** Deferred delete was the one candidate and is not being introduced
(FR-036), so neither remaining open decision can add an entity: both are about what an existing
response says.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A filtered server listing returns only matching servers; a filter matching nothing
  returns nothing. Verified for every honoured filter, in both the brief and the detailed listing.
- **SC-002**: `openstack server list --name X` and the dashboard's server search return the same rows
  a human would pick out of the unfiltered list.
- **SC-003**: `openstack image set` changes the property it names, confirmed by reading the image
  back, for every property the product allows to be written.
- **SC-004**: No image patch answers success without writing what it accepted; an unsupported
  operation or path is refused with a message naming it.
- **SC-005**: A project cannot create a volume, by any route including a server boot, that takes it
  past its configured allowance.
- **SC-006**: A refused volume request leaves no volume, no backing store, and no server implying one.
- **SC-007**: Concurrent creations against a single remaining allowance yield exactly one volume.
- **SC-008**: Enforced limits and reported limits agree for every project, configured or default.
- **SC-009**: A client reaching the deployment under any address it can resolve completes a full
  command sequence — token, catalog, version document, API call — without per-command overrides, and
  is never handed endpoints derived from another client's request.
- **SC-010**: Every advertised server action is reachable in some state the product produces.
- **SC-011**: A client negotiating the minimum advertised version receives no behaviour introduced
  later in the range; a client negotiating nothing sees no change from today.
- **SC-012**: The address lookups for a detailed listing do not grow with the number of servers, and
  the addresses reported are unchanged.
- **SC-013**: Every requirement above is covered by a test that fails against the current code —
  demonstrated, not asserted, by running the new tests before the change.
- **SC-014**: The existing suites stay green: unit tests, the compute contract suite, and the
  integration scripts.

## Assumptions

- **The filters are pinned against upstream, not invented.** The parameter set, `name`'s matching
  semantics, and the treatment of a parameter the product cannot honour are all upstream behaviours;
  planning resolves them from upstream source or documentation, the way SPEC-024's Neutron decisions
  were resolved. This spec deliberately does not guess them.
- **`add` and `replace` are the same write for these properties.** The image properties a patch may
  write are scalars on a single row, so setting one that has no value and replacing one that does are
  the same operation. This is why FR-010 is small even though the patch media type is not.
- **The image property allowlist stays as it is.** This feature makes the handler honest about what it
  will not write; it does not widen what can be written. Arbitrary custom image properties have
  nowhere to be stored today, and adding somewhere is a separate subject.
- **Quota enforcement reuses the existing limit and usage resolution, and corrects one part of it.**
  The limits, the defaults and the usage counting already exist and are already shared by both quota
  views; this feature adds a reader on the creation paths rather than a second source of truth. The
  one correction is the `gigabytes` count, which reads volume size only and must include snapshot
  size (FR-026) — so the shared resolution stays single, but its answer changes. The concurrency-safe
  shape the compute service already uses for its own quota check is the model.
- **Quota enforcement needs no schema change.** The quota table and its defaults are in place.
- **A boot refused on quota is refused before anything is created.** SPEC-025 established
  pre-flight refusal for a boot that cannot succeed; a quota refusal is the same shape and reuses it.
- **The catalog's object-storage entry is out of scope by construction.** It names an external
  service configured out of band, and is already special-cased for exactly that reason.
- **Honouring negotiation cannot remove behaviour from existing callers.** A client that sends no
  version header is treated as asking for the current version, which is what the product's single
  existing gate already does.
- **No new external dependency, and no new service.** Every change is behavioural, inside packages
  that already exist.

## Constraints & Tradeoffs

- **A negative test is the only evidence that counts here.** Stated in the Overview and repeated in
  FR-009, FR-017 and FR-029 because it is the single most likely way this feature is delivered wrong:
  every one of these defects passes a test that exercises only the working case. Nine of SPEC-025's
  verification steps had this fault and had to be rewritten.
- **Enforcing a quota that was never enforced is a behaviour change for existing deployments.** A
  project already over its configured allowance will start being refused. The defaults are generous
  (100 volumes, 10 TB), so this is unlikely to bite, but it is a change in kind rather than degree and
  belongs in the release notes.
- **The `gigabytes` usage number changes for anyone holding snapshots** (FR-026), and it is a number
  users see: `quota-show`, the absolute-limits view and the dashboard's usage bars all read it. A
  project with many snapshots can read as over its limit the moment this ships, without having
  created anything. That is the correct number — it is what the storage actually holds — but it is a
  visible change with no user action behind it, so it belongs in the release notes next to the
  enforcement note, and the two should be read together.
- **US6's cost is an audit, and the audit is bounded by the product rather than by upstream**
  (FR-042). Bounding it the other way — gating everything upstream gates across 2.1–2.90 — would have
  made the feature's size unpredictable for gates guarding behaviour Lightstack does not implement. The
  tradeoff accepted in exchange is that the advertised range stays partly aspirational: a client may
  still negotiate a version whose behaviour the product never implemented in either form. That is the
  status quo, and FR-043 requires it be written down rather than left to be rediscovered.
- **US7 is the only item with no correctness consequence.** It is included solely because it edits the
  query US1 rewrites. If US1 lands without it, it should be dropped rather than carried.
- **One deliberate divergence from upstream, taken with eyes open** (FR-004). Refusing a filter
  upstream supports means a script that works against real OpenStack gets an error here, and Article I
  otherwise treats upstream behaviour as the target. It is accepted because the two compatible answers
  are both silent: an empty list reads as a fact about the deployment, and an ignored filter is the
  defect being fixed. The divergence is narrow — it applies to the enumerated attributes the product
  does not record, and only when a caller explicitly filters on one. Worth watching in validation:
  the dashboard's administrative server view has a filter control, so a refusal there is user-visible
  in a surface a previous feature validated as working.
- **The catalog gains a per-request dependency, and a cache that assumed the opposite.** FR-031's
  answer contradicts the cache's own comment that the catalog is immutable. The tradeoff accepted is
  that a catalog now varies by requester, which is what makes FR-031a a requirement rather than an
  optimisation: the same client-controlled value that fixes the addressing failure becomes a way to
  choose another caller's endpoints the moment it is cached under a shared key.
- **All three scope-shaped decisions came out on the smaller side.** Deferred delete stayed out,
  with `restore` keeping upstream's answers, the microversion audit was bounded by the product's own surface, and the catalog change
  is a precedence flip in one function. The feature therefore adds no persisted state, no migration
  and no new setting — the cost sits in tests and in the audit, not in new machinery.

## Out of Scope

- **F5 — the removed `ResizeInstance` handler.** Already deleted on this branch; recorded in
  FOLLOWUPS.md for the audit trail only.
- **Widening what an image patch may write.** Custom image properties, `protected` and the tag
  collection are not added to the allowlist here; the patch is made honest about refusing them.
  Tags already have their own endpoints.
- **A second quota surface.** Network quotas remain code defaults with live-counted usage, as
  SPEC-020 left them. This feature enforces block storage, where the limits are already persisted.
- **Quota enforcement on paths outside block storage.** The compute quota check already exists and is
  unchanged.
- **The catalog's structure and fallback.** The hardcoded fallback catalog and the partial-read guard
  are untouched. **The cache is not out of scope** — FR-031a brings its key into this feature, because
  a per-request host and a key that ignores the host cannot both be correct. Nothing else about the
  cache changes: not its backend, its TTL, or its default of being disabled.
- **Microversion negotiation in services other than compute.** Identity, network, block storage and
  image services advertise their own versions; if the audit finds the same gap there, it is filed, not
  fixed here.
- **Performance work beyond FR-045.** No other listing is re-shaped, and no index is added, unless the
  filter work in US1 shows one is needed.

## Dependencies

- **SPEC-025 (`feature/547`), same branch.** This feature continues it and depends on its work being
  present: the shared self-link rule that FR-031 would extend, the floating-IP join in the address map
  FR-046 must preserve, the pre-flight refusal shape FR-022 reuses, the boot-from-new-volume path
  FR-019 must cover, and the single microversion gate FR-038 generalises. It is implemented on the
  same branch for that reason, and tracked by a comment on cloud-storage#547 rather than an issue of
  its own.
- **SPEC-023's filter shape.** The identity collections' filter accumulation is the pattern US1
  follows; the shared accumulator it uses is already the compute listing's base type.
- **The existing block-storage quota views (SPEC-020).** FR-025 and FR-026 depend on their limit and
  usage resolution remaining the single source it is today — which is also what makes FR-026's
  correction to the `gigabytes` count land in both views at once rather than in one of them.
- **Upstream OpenStack behaviour**, for the filter set and semantics (FR-003, FR-004), the over-limit
  answer (FR-020), the patch media type's operations (FR-010 – FR-013), the out-of-range version
  answer (FR-041) and the microversion history (FR-042). Resolved during planning from upstream
  source or documentation.
