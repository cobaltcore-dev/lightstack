# Follow-ups from SPEC-025 (cloud-storage#547)

Findings surfaced while implementing this feature that were deliberately left out of it.
Each is written so it can be filed as-is.

**Not filed as issues yet** — new tickets land in someone's backlog and the routing is the
team's call, so they are recorded here rather than created unilaterally. Say the word and
they go up as issues on `cobaltcore-dev/cloud-storage`.

---

## F1 — Volume creation enforces no quota at all

**Where**: `internal/cinder/` — `CheckQuota` has **zero** occurrences in the whole package.

**What**: a project can create unlimited volumes and consume unlimited gigabytes. The
`cinder_quotas` table exists (migration `039`) and the quota *views* report limits and
usage, but nothing on the create path consults them. `volumes` and `gigabytes` quotas are
reported and never enforced.

**Why it is not in SPEC-025**: raised during that feature's clarify pass, when deciding
whether boot-from-new-volume should count against a quota. It cannot bypass a check that
does not exist, and adding the product's first volume-quota enforcement inside a compute
ticket would be inconsistent and unreviewable in that scope. Recorded there as an
explicit Out of Scope entry.

**Severity**: a tenant can exhaust node-local storage or a Ceph pool. Worth a look before
any deployment that is not single-tenant.

---

## F2 — `RestoreInstance` gates on a status nothing writes

**Where**: `internal/nova/advanced_actions.go` — `RestoreInstance` refuses unless
`status == "SOFT_DELETED"`.

**What**: nothing in the product ever sets `SOFT_DELETED`. Every delete path is a hard
`DELETE FROM instances`. So `POST /servers/{id}/action {"restore": null}` can never
succeed: the instance either exists with some other status (refused) or is gone (404).
The action is advertised and unreachable.

**Why it is not in SPEC-025**: same family as the dead `!= 'DELETED'` filter that feature
removed from the usage query (B7), and noted alongside it. Removing this one is not
symmetrical, though — the fix is either to implement deferred delete (upstream's
`reclaim_instance_interval`) or to stop advertising the action. That is a product
decision, not a cleanup.

**Severity**: low. Misleading rather than harmful.

---

## F3 — No microversion negotiation

**Where**: `internal/nova/` — `handlers.go` emits `OpenStack-API-Version` and, until
SPEC-025, never read the request header.

**What**: Lightstack advertises microversions 2.1–2.90 and applies behaviour unconditionally.
Comments describe behaviour as version-gated when it is not — for example
`advanced_actions.go`'s note that "microversion 2.45+ returns image_id in response body",
which happens on every request regardless of what the client negotiated.

SPEC-025 introduced **one** local gate, for a deleted server's action history (upstream
gates it at 2.21), specifically to avoid granting more than a client negotiated while
fixing an older gap. That is now the only gate in the product, and it is documented as
such in `internal/nova/microversion.go`.

**Why it is not in SPEC-025**: Constitution Article I requires microversion support to
follow the Flamingo ranges, so this is a real fidelity gap — but a product-wide one.
Generalising it would have dwarfed the ten defects that feature was given.

**Suggested scope**: an audit of which advertised behaviours are actually version-gated
upstream, then a shared negotiation helper. `requestedMicroversion` in
`internal/nova/microversion.go` is a starting point.

---

## F4 — The service catalog ignores the request host

**Where**: `internal/keystone/auth.go` — `substituteURLTemplates`.

**What**: catalog endpoint URLs are rewritten from `LIGHTSTACK_ENDPOINT_HOST` (defaulting to
`localhost`), never from the host the client addressed. SPEC-025 reversed that precedence
for **self-links** (FR-002) but deliberately left the catalog alone: it is deployment-wide
rather than per-request, and it is SPEC-007's surface.

**Consequence worth knowing**: if `LIGHTSTACK_ENDPOINT_HOST` names something a client cannot
resolve, every command fails at catalog lookup, before any version document is read. That
was observed while testing SPEC-025's US1 and is now documented in `docs/CONFIGURATION.md`
and in `test/nova_gaps_test.sh`.

**Question for the ticket**: should the catalog echo the request host the way self-links
now do? Arguments both ways — a catalog is a deployment-wide statement, but a client that
reached the deployment under one name and is then told to use another is exactly the
problem SPEC-025 fixed one level up.

---

## F5 — `ResizeInstance` was unreachable (already removed)

**Where**: `internal/nova/advanced_actions.go` — removed on `feature/547`.

**What**: ~20 lines of handler that nothing routed to and that was absent from the
`serverActions` dispatch table; `resize` dispatches to `ResizeInstanceAction`. Found while
auditing the by-name defect (B14) and deleted at the user's request.

**Recorded for the audit trail only** — no action needed. Noted because its raw
`c.Param("id")` read would otherwise look like a surviving instance of B14 in any diff
that predates the removal.

---

## F6 — Per-instance address query in server listings (N+1)

**Where**: `internal/nova/handlers.go` — `getInstanceAddresses`, called once per instance
from the detail listing.

**What**: a listing of N servers runs N address queries. SPEC-025 added a floating-IP join
to that query (B17) but did not change the number of round trips — it aggregates in SQL
specifically so the join does not make it worse.

**Why it is not in SPEC-025**: pre-existing, and out of scope there. Flagged in that
feature's spec as a deferred item.

**Suggested scope**: build the whole listing's address map in one query keyed by
`device_id`. Only worth doing if listings become slow in practice.

---

## F7 — The compute server listing ignores every query filter

**Where**: `internal/nova/server_filter.go` — `newServerFilter` builds the project-scope
predicate and the `all_tenants` widening, and nothing else.

**What**: `GET /v2.1/servers` and `/servers/detail` accept upstream's filter parameters and
apply none of them. Verified against the real-mode host deployment:

```
GET /v2.1/servers?name=probe          -> drift, diag, probe
GET /v2.1/servers?name=doesnotexist   -> drift, diag, probe
GET /v2.1/servers?status=ERROR        -> drift, diag, probe
```

Upstream supports `name` (a regex over the display name), `status`, `flavor`, `image`, `host`
and more. `name` is the one that matters most in practice: it is what
`openstack server list --name X` sends, and what Horizon's search box sends. Neither filters
client-side, so both silently show the wrong rows rather than an error.

**How it surfaced**: the SPEC-025 host pass, R8. The step read
`openstack server list --name probe -f json | jq -c '.[0].Networks'` and got `10.244.10.154`
where the detail view said `10.244.10.128` — which looked like the detail and list paths
disagreeing about the address map, the very field B17 had just fixed. They do not disagree:
`.154` belongs to `drift`, and `[0]` was simply the wrong server. **A filter that is ignored
does not fail loudly; it hands back a plausible answer about something else.**

**Why it is not in SPEC-025**: not one of the ten defects, and the same family as the six
identity collections SPEC-023 fixed (`#549` User Story 5) — which fixed identity and left
compute untouched. Doing compute here would be scope creep into a sibling feature's subject.

**Severity**: moderate, and higher than it looks. A wrong-but-plausible listing is worse than
an error, and any client or script that trusts `--name` to narrow a result is reading another
server's state. Note `os-simple-tenant-usage` and the identity collections were both found to
have exactly this shape before.

**Suggested scope**: extend `serverFilter` with upstream's documented parameters, `name` first,
using the existing `common.SQLFilter`. The accumulator is already the right shape — it takes
`Eq` and `Cond` — so this is adding predicates, not restructuring. Pin each one with a test
that asserts a NON-matching value returns nothing, since an ignored filter passes any test
that only checks the matching case.

---

## F8 — The image PATCH silently ignores every operation except `replace`

**Where**: `internal/glance/images.go` — `UpdateImage`, the loop over the JSON-Patch document. The
body reads `if op == "replace" { ... }` and `continue`s past anything else, then falls through to
`GetImage(c)` and returns **200 with the unchanged image**. Its own comment says so —
"simplified - only handles replace operations" — but nothing tells the client.

**What**: `add` and `remove` are accepted and discarded. Verified against the real-mode host:

```
PATCH [{"op":"replace","path":"/min_disk","value":10}]  -> 200, min_disk becomes 10
PATCH [{"op":"add","path":"/min_disk","value":7}]       -> 200, min_disk unchanged
```

Upstream Glance's Images v2 patch media type
(`application/openstack-images-v2.1-json-patch`) specifies `add`, `replace` and `remove`.

**Why it matters more than it reads**: `openstack image set --min-disk N` sends `add`, so the CLI
reports success and nothing changes. That is not a corner case — it is the documented way to set the
property, and it is silent in both directions: no error to the client, no warning in the log. The
existing `log.Warn` fires only when a `replace` UPDATE errors, so an ignored op leaves no trace at all.

**How it surfaced**: SPEC-025 host pass, R9's size floor (FR-052). The step needed `min_disk` raised
to have a floor to hit; `openstack image set --min-disk 10 cirros` returned success, `image show`
reported `0`, and the DB agreed. Two red herrings were eliminated first — the image's `project_id`
matches the caller's, so the `AND project_id = $4` scoping was not silently matching zero rows, and no
patch-failure line was logged.

**Severity**: moderate. Any property a client sets for the first time is silently dropped, and the
`RowsAffected()` of the `replace` UPDATE is not checked either, so a genuine zero-row update is equally
quiet.

**Suggested scope**: handle `add` as an upsert of the same allowlisted column (for these scalar
properties `add` and `replace` are the same write), implement or explicitly refuse `remove`, and answer
**400** for an operation that is not supported rather than 200-with-no-change. Check `RowsAffected()`
on the update and refuse if it is zero. Pin each op with a test that reads the value back — a test that
only asserts 200 passes against the current behaviour.
