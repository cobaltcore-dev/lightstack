# Phase 0 Research: SPEC-025 Follow-ups

**Feature dir**: `025-followup` | **Branch**: `feature/547` | **Date**: 2026-09-02

Every `NEEDS CLARIFICATION` in the Technical Context is resolved below. The spec deliberately
deferred two upstream lookups to this phase (FR-003, FR-010 – FR-013); both are answered, and the
lookups **turned up three things the clarify session could not have known**, each flagged in place.

Upstream was read through Context7 against `/openstack/nova`, `/openstack/glance`,
`/websites/openstack` and `/websites/openstack_api-ref`. Context7 was unavailable during SPEC-024
and is available now, so this feature's upstream answers are sourced from the api-ref and api-guide
directly rather than by fetching opendev.

---

## R1 — Server-list filters: the set, and what `name` means (FR-003, FR-004)

**Decision**: honour `name`, `status`, `image`, `flavor`, `changes-since` for every caller, and
`user_id`, `project_id`/`tenant_id`, `key_name`, `locked`, `task_state`, `power_state` for
administrators. Refuse `host`, `hostname`, `availability_zone` and `tags` per FR-004. Treat `ip` as
in-scope-if-cheap (see below). `name` matches as a **regular expression**, case-sensitively, over
the display name.

**Rationale**: upstream's api-guide splits the parameter set by role — *"General users have a limited
set of query options, including `changes-since`, `flavor`, `image`, `ip`, `name`, `status`, and
`tags`. Administrators have access to a broader range of query options, including most server
attributes."* The api-guide's `name` entry says it *"Supports exact and regex matching"*, and the
api-ref documents `flavor` and `image` as UUIDs (with `image_ref` a deprecated alias for `image`).
Cross-checking each against `instances`: `name`, `status`, `image_id`, `flavor_id`, `user_id`,
`project_id`, `key_name` (migration 063), `locked` (015), `task_state` (005) and `power_state` are
columns; `changes-since` reads `updated_at`.

**Three findings that change requirements as written**:

1. **`host` is role-split upstream, and the spec's divergence is therefore wider than described.**
   The api-ref: *"The `host` parameter allows administrators to filter the server list by the compute
   node host name. This parameter is restricted to administrative users, and any requests from
   non-admin users **will be ignored**."* So upstream *honours* it for an administrator and
   *silently ignores* it for everyone else. FR-004 refuses it for both. The clarify answer stands —
   it was taken knowingly ("better to fail") — but the divergence is now precisely two-sided:
   Lightstack refuses where upstream would answer, **and** refuses where upstream would ignore. Recorded in
   the plan's Complexity Tracking as an Article I deviation.
2. **`hostname` is a second unsupportable filter**, non-admin-accessible from microversion 2.90 —
   which is the exact version Lightstack advertises as its maximum. It belongs in FR-004's enumerated set.
3. **`ip` is expressible but not cheaply.** It is in upstream's *general user* set, and Lightstack stores
   addresses in `ports.fixed_ips` as JSONB. So it is neither honourable-by-column nor genuinely
   unsupportable. Proposal: honour it via a `ports` subquery on `device_id`, which US7's address-map
   rewrite already needs to build; if that lands, `ip` is nearly free, and if it does not, `ip` joins
   FR-004's refused set rather than being silently dropped. Either way it must not be ignored.

**Alternatives considered**: mirroring upstream's role split for `host` (rejected — the product has
no host column, so "honour for admin" is impossible and the split would only decide *which* silent
answer non-admins get); treating `name` as a substring match (rejected — upstream is a regex, and a
substring match would return rows a regex would not, which is the same class of wrong-but-plausible
answer the story exists to remove).

---

## R2 — Image patch operations, and the status code for a refusal (FR-010 – FR-013)

**Decision**: the media type is `application/openstack-images-v2.1-json-patch` and conforms to
**RFC 6902**, so `add`, `replace` and `remove` are all defined. Implement `add` and `replace` as the
same write for the four allowlisted properties. Answer **403** for `remove` of one of them, and for
a path outside the allowlist that names a service-managed property. Answer **400** for a malformed
document or an operation RFC 6902 does not define.

**Rationale**: the api-ref states the body *"must conform to the JSON Patch format as defined by
RFC6902"*, which is where the three operations come from. On refusals it is explicit that the code is
403, not 400: *"Modifying certain image properties can lead to a 403 (Forbidden) response. This
includes base image properties managed by the Image Service, properties for which you lack
permission (since v2.2), deleting the only image location..., or setting properties with reserved
names like those prefixed with `os_glance`."* All four allowlisted paths (`/name`, `/visibility`,
`/min_disk`, `/min_ram`) are **base** properties — the api-ref notes base properties are *"always
included, with a value of `null` if not set"*, which is also why they have no absent state to be
removed to, the reasoning the clarify session reached independently.

**Finding**: the spec says "client error" throughout FR-011 – FR-013 without fixing a code, and the
natural reading was 400. Upstream's answer is **403** for the property cases. `common` already has
`NewForbiddenError`; no new helper is needed for this one.

**Alternatives considered**: 400 for everything (rejected — upstream distinguishes a malformed
document from a forbidden property, and a client library may branch on it); implementing `remove` as
a reset to the column default (rejected in clarify — makes `remove` a synonym for an unrequested
write).

---

## R3 — The over-limit answer (FR-020)

**Decision**: reuse `common.NewQuotaExceededError`, which answers **413** with code `overLimit` and
names the resource.

**Rationale**: 413 is the OpenStack convention for a quota refusal — it appears in the standard
status-code list for the identity API and is what object storage returns when a container quota is
exceeded. More decisively for this product, it is already what compute's own quota refusals answer
through this exact helper, and FR-025's requirement that enforcement and reporting cannot disagree
extends naturally to enforcement and *compute's* refusals not disagreeing either.

**Caveat, recorded rather than hidden**: the Cinder-specific fault body (upstream raises
`VolumeLimitExceeded` / `VolumeSizeExceedsAvailableQuota`) was not confirmable from the
documentation available through Context7 — the queries returned performance-test fixtures and the
generic status-code lists. The existing helper is the consistency anchor; if a stock client is found
to branch on a Cinder-specific key during host validation, that is a finding to act on, not an
assumption to bake in now.

---

## R4 — Microversion refusal, and a divergence nobody had noticed (FR-041, FR-041a)

**Decision**: refuse an out-of-range version with **406 Not Acceptable**. Keep treating an
unparseable header as no negotiation (FR-041a). **A new error helper is required** —
`internal/common/errors.go` has no 406 constructor.

**Rationale**: the api-guide is unambiguous: *"If a specified microversion is outside the supported
range, a 406 Not Acceptable error is returned."* It also confirms two things Lightstack already gets right:
`OpenStack-API-Version` takes precedence over `X-OpenStack-Nova-API-Version` when both are present
(which is the order `requestedMicroversion` reads them in), and `latest` requests the maximum.

**Finding — a third Article I deviation, and the largest of them.** The same paragraph states: *"If
no version headers are provided, the **minimum** supported microversion is assumed."* Lightstack's
`requestedMicroversion` returns the **current** (maximum) version for a request with no header, and
does so deliberately — its comment reasons that a non-negotiating client should keep receiving the
product's full behaviour. FR-040 requires exactly that ("no existing caller changes"). So upstream
fidelity and FR-040 are in direct conflict here, and FR-040 wins by the spec's own decision.

The consequence is worth stating plainly, because it bounds what US6 can deliver: with no-header
resolving to the maximum, **the gates FR-039 introduces only ever bite a client that explicitly
negotiates an older version.** A stock client that sends no header keeps receiving the newest shape
of every response, where upstream would hand it 2.1 shapes. Adopting upstream's default would be a
breaking change for every existing Lightstack caller, which is why it is not proposed here — but it means
Lightstack's advertised `min_version` remains partly aspirational in a second, distinct way from the one
FR-042 already covers, and FR-043's audit record must say so.

**Alternatives considered**: adopting upstream's minimum-version default (rejected — breaks every
existing caller, contradicts FR-040); making the default configurable (rejected — a new setting for a
compatibility question nobody has asked for, and it would double the test matrix of every gate).

---

## R5 — Where block-storage enforcement goes (FR-018, FR-019, FR-019a, FR-028)

**Decision**: one `ctx`-shaped helper in `internal/cinder`, called from **six** sites:

| Site | File | Resources to check |
|---|---|---|
| Volume create (API) | `volumes.go` `CreateVolume` | `volumes`, `gigabytes`, `per_volume_gigabytes` |
| Boot from new volume (compute) | `volumes.go` `CreateBootVolumeFromImage` | same |
| Backup restore into a new volume | `backups.go` (`INSERT INTO volumes`) | same |
| Managed-volume import | `manage.go` `ManageVolume` | same |
| Transfer accept | `transfers.go` `AcceptVolumeTransfer` | `volumes`, `gigabytes` |
| Volume extend | `volumes.go` `os-extend` action | `gigabytes` for the **delta** |
| Snapshot create | `volumes.go` `CreateSnapshot` | `snapshots`, `gigabytes` |
| Backup create | `backups.go` `CreateBackup` | `backups`, `backup_gigabytes` |

**Rationale**: the spec's FR-019 asked planning to re-derive the list from the code rather than trust
its enumeration, and doing so found all four volume-inserting routes — three separate
`INSERT INTO volumes` statements plus the shared provisioning routine — where the spec named two.
Eight call sites in total once snapshots, backups and extend are included, across four files.

**Shape**: `nova.CheckQuota` is the model and is *almost* directly reusable — it already wraps
`database.WithTx(ctx, ...)` with a `SELECT ... FOR UPDATE` on the project's quota row, which is the
TOCTOU-safe form FR-023 needs. Its only compute-specific detail is its `*gin.Context` parameter,
used solely to read `project_id` and `c.Request.Context()`. The Cinder helper takes
`(ctx, projectID, resource, amount)` instead, which is what lets the compute boot path call it at
all. `database.WithTx` is package-level, so it is reachable from `internal/cinder` unchanged.

**Rollback obligation (FR-021, FR-019a)**: the check must run *before* any backing store is created,
not after. `provisionVolume` already creates the backing store and then inserts the row, rolling the
backing store back if the insert fails — so placing the check at the top of `provisionVolume` covers
the two paths through it and needs no rollback at all. The three paths that insert directly need the
check before their `INSERT`. Transfer accept is the exception FR-019a describes: there is nothing to
roll back, and the refusal must leave the transfer *pending*.

**Alternatives considered**: putting the check inside `provisionVolume` only (rejected — three
routes insert volumes without going through it); a database constraint or trigger (rejected — cannot
produce a 413 with a resource name, and Article VIII favours using the framework directly);
enforcing at the HTTP middleware layer (rejected — the compute boot path carries no HTTP request).

---

## R6 — The bounded microversion audit (FR-042, FR-043)

**Decision**: audit the compute response fields Lightstack actually emits, comparing each against the
microversion at which upstream introduced it. Candidate list to work through, drawn from the
product's own response builders and the comments that claim gates:

- `createImage`'s `image_id` in the action response body — the comment in `advanced_actions.go`
  claims 2.45 gating that does not exist. Upstream introduced it at 2.45.
- `key_name` in server detail (SPEC-024/#524's addition) — upstream has carried it since 2.1.
- `locked` — upstream 2.9.
- `description` / `tags` — not emitted, so out of the audit by FR-042.
- `OS-EXT-SRV-ATTR:*` fields — emitted; upstream widened non-admin visibility at 2.3 and 2.75.
- `fault` (SPEC-025 migration 067) — upstream 2.1, but its `code` semantics shifted.
- The deleted-server action history gate at 2.21 — already implemented, the product's only gate.
- `hostname` in server detail — 2.90, the advertised maximum.

**Rationale**: FR-042 bounds the audit by what the product emits, so the work is enumerating
emitters rather than reading ninety versions of changelog. The list above is the enumeration to
verify, not the answer — each entry needs its introduction version confirmed against the api-ref's
`rest_api_version_history` before a gate is added or an entry is recorded as deliberately ungated.

**Note**: R4's finding caps the value of every gate this produces (no header → maximum, so only
explicit negotiators are affected). That does not change what the audit should cover, but it is why
FR-044's tests must negotiate explicitly rather than rely on a default.

---

## R7 — Catalog host substitution: the mechanism, and a two-port trap (FR-031, FR-031a, FR-031b)

**Decision**: keep `substituteURLTemplates`' prefix-replacement approach and feed it the request
host with **any port stripped**. Change `BuildServiceCatalog`'s signature to carry the host from its
two call sites, both of which already hold the request context. Apply the same change to
`buildHardcodedCatalog`.

**Rationale**: the current function does not parse URLs — it string-replaces `http://lightstack:` and
`http://localhost:` with `http://<configured>:`, which is how each endpoint keeps *its own* port
(the trailing colon is part of the match). That property is worth preserving: the per-service ports
live in the seeded endpoint rows, and `common.BaseURL`'s port-substituting form is the wrong tool
here because there is no single port for a catalog.

**The trap**: a request host frequently carries a port (`lightstack.example.com:35357`). Substituting it
into a pattern that supplies its own trailing colon produces `http://lightstack.example.com:35357:8774` —
two ports, resolvable by nothing. This is the identical failure `common.hostWithPort` documents
having produced before ("authorities like `lightstack.example.com:1234:9696`"). The host must therefore be
split before use, and `common`'s existing bracket-aware logic is the thing to reuse rather than
re-derive — it currently lives inside the unexported `hostWithPort`, so a host-only helper needs
exporting or extracting.

**Cache (FR-031a)**: the key is `"service_catalog:" + projectID` with a 24-hour TTL and a comment
asserting the catalog is immutable. Once the host varies per request that comment is false.
Recommended resolution: include the resolved host in the key. Not caching host-derived catalogs at
all is the simpler alternative and equally correct, since caching is off by default — but it
silently removes a cache an operator turned on, so the key change is preferred.

**Forwarded headers (FR-031b)**: nothing in the product reads `X-Forwarded-Host` or
`X-Forwarded-Proto`. Recommendation: **do not** start trusting them in this feature. `c.Request.Host`
behind a correctly configured ingress already carries the public name; a forwarded header is
client-supplied on a direct connection, and FR-031a exists precisely because a client-supplied host
reaching a shared cache is dangerous. Document the `proxy_set_header Host $host` requirement instead.

---

## R8 — One address query for a listing (FR-045 – FR-048)

**Decision**: replace the per-instance call with one query taking the page's instance ids as an
array, returning `device_id` alongside each row so results can be bucketed by server. Keep the
existing SQL shape — the `floating_ips` LEFT JOIN with `ARRAY_AGG ... FILTER`, and the
`GROUP BY p.id` that makes it one row per port — since that is B17's fix and FR-046 requires the
output to be byte-identical.

**Rationale**: the current query filters `p.device_id = $1 AND p.project_id = $2`, where the second
argument is the *instance's own* project, not the caller's. In a batched form there is no single
project to pass, so the pairing must be preserved another way: joining `instances i ON i.id =
p.device_id AND i.project_id = p.project_id` states the same constraint per row and keeps a
cross-tenant administrative listing correctly attributed. Dropping the project predicate entirely
would be wrong — it would let a port whose project disagrees with its instance's contribute an
address.

**Alternatives considered**: a lateral join inside the listing query itself (rejected — the address
map needs post-processing in Go to merge fixed and floating entries per network, so it cannot be
assembled in the listing's row scan); leaving the N+1 in place (rejected only because US1 rewrites
the same function; on its own merits FR-045 is the lowest-value requirement in the feature).

---

## R9 — Reusable machinery: what exists, and the one gap

| Need | Exists | Notes |
|---|---|---|
| Filter accumulation | `common.SQLFilter` (`Eq`, `Cond`, `Arg`, `Where`, `CloneArgs`) | `serverFilter` already embeds it; `keystone/list_filters.go` is the pattern to copy, including its `eq`/`eqBool` query-param readers |
| Race-free quota check | `nova.CheckQuota` + `database.WithTx` + `SELECT ... FOR UPDATE` | Reshape to `ctx`, do not re-derive |
| Quota limits and usage | `resolveCinderQuotaLimits`, `cinderQuotaUsage` | Both quota views and the absolute-limits view already read them (`quotas.go:131`, `:219`, `volumes.go:2181`), so FR-026's counter correction lands in all three at once |
| 413 over-limit | `common.NewQuotaExceededError` | Already `overLimit` + resource name |
| 403 forbidden | `common.NewForbiddenError` | For R2's patch refusals |
| 400 bad request | `common.NewBadRequestError`, `NewInvalidValueError` | `NewInvalidValueError(field, value, allowed)` fits FR-004's "naming that filter" |
| Request host precedence | `common.BaseURL` | The *rule* to mirror; not directly callable for the catalog (R7) |
| **406 Not Acceptable** | **Missing** | The one new piece of shared machinery this feature needs |

**Tests to extend rather than create**: `internal/nova/server_filter_test.go`,
`internal/nova/addresses_test.go`, `internal/cinder/quotas_test.go`, `internal/glance/images_test.go`,
and the scripts `test/nova_gaps_test.sh`, `test/quota_test.sh`, `test/spec007_catalog_scheme_test.sh`.
No test currently pins the broken behaviour for any of the seven findings, so nothing needs breaking.
