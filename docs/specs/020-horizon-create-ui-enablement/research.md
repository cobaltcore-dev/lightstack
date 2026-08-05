# Phase 0 Research — Horizon create/write UI enablement (F2, F3, F7, F8)

**Feature**: SPEC-020 | **Date**: 2026-07-27 | **Plan**: [plan.md](./plan.md) | **Spec**: [spec.md](./spec.md)

All spec ambiguities were resolved in the spec's **Clarifications** (5 decisions) and **Assumptions**; no
`NEEDS CLARIFICATION` markers remain. This file records the resulting design decisions, each grounded in the
branch's code (file:line) and upstream sources.

Sources: code inspection of `feature/533` (base `71aa00d`, i.e. #532/SPEC-019 already applied); upstream docs
via Context7 — `/openstack/neutron` (policy, quota extension), `/websites/openstack_api-ref` (Networking v2.0
quotas, Block Storage v3 quota-sets); the SPEC-018 report
[`docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`](../../compatibility/HORIZON_COMPATIBILITY_REPORT.md)
and its two runbooks.

---

## R0 — Method: trace-first, then fix (FR-014)

**Decision.** The ticket's instruction ("trace those calls first — one fix likely re-enables several") is
implemented as a **gating first task**, not an assumption. Before any handler is changed, a scripted probe
plus the Horizon container log produces a **confirmed call map**: for each of the four create UIs (Overview
load, Launch wizard, Create Network, Create Volume), the exact requests Horizon issues to O3K and their
status codes.

**Two evidence sources, both already available:**
1. **O3K's own request log** — every service runs `middleware.LoggingMiddleware()` (`cmd/o3k/main.go:511`,
   `:537`, `:560`), emitting structured JSON per request. Filtering it while driving each panel yields the
   precise path + status list, including 404s.
2. **Horizon's apache error log** — the SPEC-018 report already documents the retrieval command for the
   Django traceback (report F9 follow-up):
   `kubectl -n o3k exec deploy/horizon-o3k-horizon -- sh -c 'tail -80 /var/log/apache2/*error*'`
   (compose equivalent: the horizon container's logs). This names the failing Python call for a broken panel.

**Rationale.** The code inspection below produces high-confidence *candidate* gaps, but the spec is explicit
that they are "the starting hypothesis, not an assumed-complete list" (FR-014). Confirming with real traffic
before editing avoids fixing endpoints Horizon never calls and catches gaps the code map cannot predict.
Findings that the trace adds get folded into the work list.

**Alternatives considered.** (a) Fix all candidate gaps blind and re-test — rejected: violates FR-014 and
risks shipping unused endpoints (Art. VII Simplicity). (b) Read Horizon's Python source instead of tracing —
useful as corroboration (used below), but the deployed kolla image is the authority and only the trace proves
what *this* Horizon build calls.

---

## Trace results (T003 / T005 — 2026-07-27, stub stack)

Executed `test/horizon_create_ui_test.sh` against a live stub O3K
(`deployments/docker-compose.yml`, image built from this branch). **Endpoint-level evidence, empirical.**
Result: **15 passed / 11 failed**. Every failure maps to a predicted gap; one prediction was materially
refined and one new gap was found.

| Gap | Trace verdict | Evidence |
|---|---|---|
| **G1** nova limits ⇄ quota-set drift | ⚠️ **LATENT, not active** | For the seeded default project they **agree** at 10/20/51200 — migration `007` seeds `quotas` rows that override `GetLimits`' own 100/200/512000 default map, so the drift is masked. Verified: `SELECT resource,hard_limit FROM quotas WHERE project_id='…0002'` → `cores 20, instances 10, ram 51200`, and an **unseeded** project id returns quota-set `10/20/51200` while `GetLimits`' fallback map would give `100/200/512000`. **So G1 is a real correctness bug for any project without seeded rows, but it is NOT the F2 root cause in a seeded deployment.** Fix still required (FR-003); expectations adjusted. |
| **G2** nova `/os-quota-sets/{id}/detail` | ✅ confirmed | `404 itemNotFound` |
| **G4** neutron `/quotas/{id}/default` | ✅ confirmed | `404 itemNotFound` |
| **G5** neutron `/quotas/{id}/details.json` | ✅ confirmed | `404` (plain `/details` → 200) |
| **G6** neutron quota not project-scoped | ✅ confirmed | body is a bare 7-key map — **no `project_id`, no `tenant_id`** |
| **G7** `quota_details` not advertised | ✅ confirmed | absent from `/v2.0/extensions` |
| **G8** network lacks `project_id` | ✅ confirmed | `1/1 network(s) are missing project_id`; `tenant_id` present |
| **G9** cinder quota-set `detail` + `defaults` | ✅ confirmed for `/defaults`; `detail` **misconceived** | both `404`. *(Corrected 2026-08-26: the 404s are this probe's own synthetic requests, not client traffic. Block Storage v3 defines no `/detail` sub-resource — it exposes usage via `?usage=true` — so no client can request it and the 404 is not a gap. `/defaults` is a real gap and was fixed.)* |
| **G10** cinder `/v3/extensions` | ✅ confirmed | `404` — so `AvailabilityZones` is unadvertised |
| **G12** cinder limits ignore `cinder_quotas` | ✅ **confirmed and active** | `maxTotalVolumes=1000` vs `quota_set.volumes=100` — an immediate, unmasked disagreement (unlike G1) |
| **NEW — G15** subnets lack `project_id` | 🆕 **found** | `GET /v2.0/subnets` returns `tenant_id` but `project_id: null` — the same omission as G8, one level down. R4 flagged this as "to confirm"; it is confirmed. Fix alongside G8. |
| Subnet-reference integrity | ✅ passes in the simple case | With one network + one subnet, every referenced id resolves. The divergence needs the pagination / cross-project edge case to bite, so the fix and its test stay in scope as a guard rather than an observed break. |
| Volume types / default type / AZ | ✅ already working | `/v3/types` → 1 type, `/v3/types/default` → `__DEFAULT__`, `/v3/os-availability-zone` → 1 zone. Not part of the F8 root. |

**Scope decisions closed by this trace** (per T005):
- **FR-002 `.json`**: only Neutron needs it — `/details.json` 404s while nova/cinder clients never append the suffix. Spec already narrowed accordingly.
- **FR-013 alias set**: the required addition is **`quota_details`** for Neutron and a **new `/v3/extensions`** for Cinder advertising `AvailabilityZones`. No other alias is implicated.

**Out-of-scope defect noticed** (not this feature): `POST /v3/projects` returns
`500 failed to create project` for a well-formed request with `"domain_id": "default"`. Unrelated to the
create-UI cluster; worth its own ticket.

**Browser-level panel tracing (the rest of T003/T004) is host-gated** — driving Horizon's four panels and
reading its apache traceback needs an interactive browser session, exactly like the SPEC-018 runbook passes.
The endpoint-level evidence above is what determines the code changes; the panel observations belong to the
verification tasks (T029, T045, T047–T048, T053–T054).

---

## R1 — Confirmed code-level gaps (from inspection of this branch)

These were verified directly in the tree and are real regardless of the trace. Each maps to a requirement.

| # | Gap | Evidence (file:line) | Req |
|---|---|---|---|
| G1 | **Nova absolute limits disagree with the quota-set** — `GetLimits` uses instances/cores/RAM = 100/200/512000; `GetQuotaSet`/`GetQuotaSetDefaults` use 10/20/51200 | `internal/nova/handlers.go:1957`; `internal/nova/quotas.go:52,209` | FR-003 |
| G2 | **Nova `GET /os-quota-sets/{id}/detail` missing** — only `:id` and `:id/defaults` registered | `internal/nova/handlers.go:227-229` | FR-002 |
| G3 | **Nova usage counters hardcoded 0** — `totalFloatingIpsUsed`, `totalSecurityGroupsUsed`, `totalServerGroupsUsed` | `internal/nova/handlers.go:2010-2012` | FR-004 |
| G4 | **Neutron `GET /v2.0/quotas/{id}/default` missing** | `internal/neutron/network.go:164-165` (only `:id`, `:id/details`) | FR-002 |
| G5 | **Neutron quota details registered without the `.json` form** — `/quotas/:id/details` only | `internal/neutron/network.go:165` | FR-002 |
| G6 | **Neutron `GetQuota` ignores the project entirely** — returns a hardcoded map, not project-scoped, and omits `project_id`/`tenant_id` | `internal/neutron/network.go:320-335` | FR-002a, FR-004 |
| G7 | **Neutron advertises 6 extension aliases; `quota_details` is not among them** — `security-group`, `router`, `port-security`, `binding`, `provider`, `quotas` | `internal/neutron/network.go:273-318` | FR-013 |
| G8 | **Network object exposes `tenant_id` but not `project_id`** — built inline in three places | `internal/neutron/network.go:462` (create), `:637` (list), `:709` (show) | FR-007 |
| G9 | **Cinder usage view and `/defaults` missing** — `/defaults` is absent; usage is unserved because `?usage=true` is ignored. *(Corrected 2026-07-29: this row originally read "`/detail` and `/defaults` missing", inferred from O3K's code by analogy with Nova. Block Storage v3 has no `/detail` path — it documents the `usage` query parameter — and unlike G2/G5 this gap was never observed in the trace, so the `/detail` half was a false premise. See the FR-002 amendment.)* | `internal/cinder/volumes.go:154-156`, `:221-223` | FR-002 |
| G10 | **Cinder has no `/extensions` endpoint at all** | grep: no `extensions` route in `internal/cinder/` | FR-011, FR-013 |
| G11 | **No *unit* tests exist for any quota/limits endpoint** — `internal/**/*_test.go` has zero quota/limits coverage. *(Corrected 2026-07-27: **contract and script-level suites do exist** and cover these endpoints — `test/contract/nova/limits_test.go`, `test/contract/cinder/quotas_test.go`, `test/contract/neutron/quotas_test.go`, `test/quota_test.sh`, `test/quota_admin_test.sh`. They must be run as a regression gate; see R8.)* | grep of `internal/**/*_test.go` → zero; `test/contract/**` + `test/quota*.sh` → present | FR-015, FR-016 |
| G12 | **Cinder limits ignore `cinder_quotas` entirely** — all five `maxTotal*` values are hardcoded, so `PUT /v3/quota-sets/{id}` has *no effect* on `GET /limits` (the Cinder analogue of G1) | `internal/cinder/volumes.go:1974-1978`, `:2017-2021` vs `internal/cinder/quotas.go:37-44` | FR-003 |
| G13 | **`GetLimits` and `GetLimitsNoProject` are byte-identical duplicates** | `internal/cinder/volumes.go:1948-1988` / `:1990-2030` | FR-003 |
| G14 | **Cinder quota-set endpoints have no admin check at all** — any authenticated caller may read/write/delete *any* project's quota (nova has one; neutron has no writes) | `internal/cinder/quotas.go` (whole file) | FR-002a |

**Limit-default duplication (the mechanism behind G1).** The nova default map is written out **four** times —
`internal/nova/quotas.go:52` (`GetQuotaSet` local), `:207` (`GetQuotaSetDefaults` literal), `:228` (package var
`quotaDefaults`, used only by `CheckQuota`), and `internal/nova/handlers.go:1957` (a **local that shadows the
package var with different values**). Cinder duplicates its map twice (`internal/cinder/quotas.go:37`, `:143`).
There is **no** shared "load limits for project" helper anywhere. That is precisely why the values drifted.

**Ruled out (checked, not a gap).** Nova's catalog base URL is fine: the DB-backed catalog seeds
`http://o3k:8774/v2.1` with **no** project suffix (`migrations/052_fix_endpoint_hostnames.up.sql:3`), which
matches Nova's `/v2.1/...`-only routes (`internal/nova/handlers.go:128`) — this is why the Instances panel
already lists servers. *(Observation, not in scope: the **fallback** `buildHardcodedCatalog` does emit a
project-suffixed nova URL, `internal/keystone/auth.go:722`, which would break every Nova call if the DB
catalog read ever failed. Noted for the trace; a fix belongs in its own ticket.)*

Likewise **Nova `/extensions` is deliberately NOT added**: upstream Nova removed the extensions API
(deprecated at microversion 2.36), so Flamingo Nova does not serve it and adding it would *diverge* from
Art. I. Cinder v3, by contrast, still serves `GET /v3/extensions` upstream — hence G10 is a real gap.

---

## R2 — F2: the Overview limits widget

**Decision.** Make all three legs of Horizon's project-usage aggregation succeed and agree: Nova limits,
Cinder limits, Neutron quota. Concretely: reconcile Nova's limits to the `quotas` table (G1), populate the
zeroed usage counters (G3), and make the Neutron quota project-scoped and detail-capable (G4–G7).

**Rationale.** Horizon's Overview builds one usage object from **all three** services; if any leg raises, the
whole widget renders "Unable to retrieve limits information" — which is exactly the observed symptom, and why
a single missing/failing endpoint blanks the panel. The Neutron leg is the weakest: `GetQuota` is a hardcoded,
non-project-scoped map and the detail variant is unreachable at the path clients use.

**Design note — prefer the deterministic path.** Horizon reads network usage via the quota **detail** call
when the `quota_details` extension is advertised, and otherwise falls back to counting resources by listing
them (several extra calls, more failure surface). Advertising `quota_details` (G7) **and** serving
`/quotas/{id}/details[.json]` correctly puts network usage on a single endpoint O3K fully controls. Confirmed
against Context7 `/openstack/neutron`: "The Quota Detail API extension allows users … to retrieve detailed
quota information per project, including used, limit, and reserved counts."

**Alternatives considered.** (a) Leave `quota_details` unadvertised and harden the fallback's several list
calls — rejected: more moving parts, more ways to fail. (b) Return fabricated usage — rejected: FR-004
requires real consumption.

---

## R3 — F3: the Create Network action

**Decision.** Treat F3 as a **quota-metadata** fix, not a policy change (spec Clarification 5, FR-009/FR-018).
Serve a project-scoped network quota with real usage (G4–G6) so Horizon computes headroom.

**Rationale.** Upstream Neutron's default policy is `create_network: rule:admin_or_project_member` (scope
`project`) — confirmed via Context7 `/openstack/neutron` — so stock Horizon already permits a project member
to create a network; policy cannot be why the control is hidden. Horizon's Project-panel create actions are
additionally gated on **available quota**: when the quota lookup fails or reports no headroom, the action is
not rendered. That matches the observed asymmetry (present in Admin, absent in Project), because the Admin
panel's create action is not quota-gated the same way.

**Alternatives considered.** (a) Change O3K's policy/roles — rejected: nothing to change (O3K has no Neutron
policy engine) and it would not affect Horizon's client-side gating. (b) Patch Horizon's policy file —
rejected by FR-018 (stock Horizon is the bar).

---

## R4 — F7: Launch Instance action + the wizard's Networks step

**Decision.** Two distinct sub-fixes:

1. **The missing Launch action** — same root as F3: compute quota/limits must resolve and show headroom
   (G1–G3). Horizon gates the Launch action on Nova quota availability.
2. **The unselectable Networks step** — make the network (and its subnets) fully renderable:
   - add `project_id` alongside `tenant_id` on every network response (G8);
   - guarantee **subnet-reference integrity**: every id in a network's `subnets` array must be resolvable
     from the subnet list the same caller can read.

**Rationale for sub-fix 2.** Horizon expands a network's `subnets` **ID list** against a separate subnet
listing and substitutes the resolved object per id; an id that the subnet list doesn't return becomes a null
entry, and a table/wizard row built from it fails to render — the report itself places F7 in "the same
data-rendering family as F9's volume table". In this tree the two queries use *different* scopes:
`networkSubnetIDs` returns **all** subnets of a network with no project predicate
(`internal/neutron/network.go:1287-1304`), while `ListSubnets` filters
`WHERE (s.project_id = $1 OR n.shared = true)` **and paginates** with LIMIT/OFFSET
(`internal/neutron/network.go:977-984`). Any subnet visible via the first path but not the second yields
exactly the null-row failure observed. The fix aligns the two scopes (and is cheap to assert in a unit test).

**Note.** The `subnets` payload's own fields are also worth confirming during the trace: `ListSubnets`'
SELECT does not fetch `project_id` (`internal/neutron/network.go:978`), so subnet responses may omit
`tenant_id`/`project_id` that the wizard reads.

**Alternatives considered.** (a) Inline full subnet objects into the network response — rejected: diverges
from the Neutron v2.0 contract (Art. I), which specifies `subnets` as a list of ids. (b) Drop unresolvable
ids server-side — rejected: hides a real inconsistency instead of fixing it.

---

## R5 — F8: the Create Volume modal

**Decision.** Add **Cinder `GET /v3/extensions`** (G10), advertising only the extensions O3K genuinely
implements, and add the Cinder quota `detail`/`defaults` variants (G9); confirm the volume-type and
availability-zone lookups the modal performs resolve on the path Horizon uses.

**Rationale.** Horizon's volume-create form loads its supporting metadata while *constructing* the dialog —
volume types, availability zones (guarded by an `extension_supported(...)` check), and quota/limits. Because
Horizon's modals are fetched over AJAX, an exception during form construction returns no dialog body, so the
button appears to do nothing — precisely the reported symptom ("Create Volume button opens no dialog"), and
distinct from F5's endpoint problem (the real pass proved the v3 endpoint is clean). Cinder v3 upstream does
serve `/extensions`, so a 404 there is an Art. I compatibility gap in its own right.

Volume types and AZ already exist (`ListVolumeTypes` `internal/cinder/volumes.go:1306`, `:91/:190`;
`/v3/os-availability-zone` `:128`) — but note the **AZ route is registered only at the flat path**, not in the
`/v3/:project_id` group, unlike types/limits/quota-sets which are registered in both. With the current
catalog (`http://o3k:8776/v3`, no project suffix) the flat path is what gets called, so this is a latent
asymmetry to confirm in the trace rather than an assumed break.

**Alternatives considered.** (a) Advertise a large stock extension list — rejected: advertising capabilities
O3K lacks invites worse failures than a 404 (Art. I is about *matching* behavior). (b) Assume the modal fails
on quota only — rejected: `/extensions` returning 404 is confirmed by inspection, quota is inferred.

---

## R6 — Reconciling limits to one source (FR-003)

**Decision.** Per spec Clarification 3: the **`quotas` table is the single source** for Nova's absolute
limits, with code defaults (upstream values instances=10, cores=20, ram=51200) applied when a project has no
row. In each service, **one package-local helper** resolves "limits for project P" (defaults overlaid with
table rows), and every limits/quota handler in that package calls it:

- **Nova** — the four duplicated default maps collapse to the existing package var `quotaDefaults`
  (`internal/nova/quotas.go:228`, already the upstream values); `GetLimits`, `GetQuotaSet`,
  `GetQuotaSetDefaults`, and the new `detail` variant all read through the helper. The shadowing local map at
  `internal/nova/handlers.go:1957` (100/200/512000) is deleted. `GetLimits` keeps the `maxTotalKeypairs` /
  `maxServerGroups` / `maxServerGroupMembers` keys, which only it needs, by adding those resources to the
  shared default set.
- **Cinder** — the two duplicated maps (`internal/cinder/quotas.go:37`, `:143`) collapse likewise, and
  `GetLimits` starts reading `cinder_quotas` so a quota override finally affects the limits view (G12). The
  duplicate `GetLimitsNoProject` (G13) delegates to `GetLimits` instead of repeating its body.
- **Neutron** — has **no** quota table (only `quotas` and `cinder_quotas` exist). Its limits stay code
  defaults; the fix is to hoist the two hardcoded copies (`internal/neutron/network.go:320-333`, `:362-400`)
  into one map so `/quotas/{id}`, `/quotas/{id}/details`, and the new `/default` cannot drift.

**Rationale.** Hand-maintained duplicate default maps are the direct mechanism of the drift (G1/G12). One
helper per package is the minimal de-duplication (Art. VII/VIII — a package-local function, not a new
abstraction layer or shared quota service), and reading the table means an admin's `PUT` override is reflected
in the limits view too, which is not true today in either Nova or Cinder.

**Scope guard.** Adding a Neutron quota *table* is **out of scope** — the spec limits this feature to the read
surface, defaults are sufficient for UI enablement, and a new table would mean a migration this plan
deliberately avoids.

**Alternatives considered.** (a) Keep the higher values — rejected by Clarification 3 (non-standard). (b) One
shared constant, no table read — rejected: admin overrides would silently not apply to the limits view.
(c) A cross-service shared quota package — rejected: three services with three different tables (and one with
none) do not share enough to justify it (Art. VII).

---

## R7 — Endpoint variants, path forms, and authorization

**Decision — path forms.** Register the missing variants explicitly, and register the `.json` form as an
additional route to the *same* handler wherever a client may append it (Neutron's quota details/default).
Neutron historically accepts a `.json` suffix, and registering both paths in gin is two lines with no routing
conflict (distinct static segments under `:id`) — cheaper and more certain than a suffix-stripping middleware,
which would change behavior repo-wide (FR-016 forbids collateral change).

**Decision — response shapes.** Follow the upstream shapes verified via Context7 `/websites/openstack_api-ref`:
- Networking quota list/show carries the resource limits plus `project_id` **and** `tenant_id`
  (both present upstream); the **detail** form reports per-resource `{limit, used, reserved}`.
- Block Storage `quota_set` carries `id` plus per-resource values; its **detail** form reports per-resource
  `{in_use, limit, reserved}`. *(Upstream also emits per-volume-type keys such as `volumes___DEFAULT__`;
  aligning with the `__DEFAULT__` type name SPEC-019 introduced is a natural follow-on but is only added if
  the trace shows Horizon needs it.)*
- Compute quota-set **detail** reports per-resource `{in_use, limit, reserved}`.

**Decision — authorization (FR-002a).** Every quota endpoint this feature adds or modifies resolves the
target project as: the caller's own `project_id` from the JWT, unless the caller holds the `admin` role, in
which case the URL id is honored. A non-admin asking for another project's id is never served that project's
data.

**Use the existing shared helper — and unite every caller on it (FR-019, clarified 2026-07-27).**
`common.HasRole(c, "admin")` (`internal/common/roles.go`) already documents itself as "the single shared
role-check helper for all services", yet only Glance calls it directly. The decision is a **repo-wide**
unification:

| Site | Today | After |
|---|---|---|
| `internal/common/roles.go` | the implementation | unchanged |
| `internal/glance/images.go:506` | `common.HasRole` | unchanged |
| `internal/nova/quotas.go:166` | hand-rolled loop | `common.HasRole` |
| `internal/nova/advanced_actions.go:478`, `:1224` | hand-rolled loop ×2 | `common.HasRole` |
| `internal/cinder/volumes.go:980` | longer hand-rolled variant | `common.HasRole` |
| `internal/keystone/handlers.go:147` | pass-through wrapper `hasRole` | **deleted** |
| `internal/keystone/{handlers.go:978,1204,1243, ec2_credentials.go:56,79}` | call the wrapper | call `common.HasRole` directly |
| `internal/neutron/*` | no role check at all | `common.HasRole` (new code) |

Behavior-preserving in every case but one, which is unreachable in practice: Cinder's `os-reset_status`
currently distinguishes a *missing* roles key (403) from a *wrong-typed* one (500); `HasRole` collapses both
to "no role" → 403. The auth middleware always sets `roles` as `[]string`
(`internal/middleware/auth.go:46`), so the 500 branch is dead code. Net effect: one implementation, four
copies and one wrapper removed (Art. VII/VIII).

**Quota write authorization (FR-020, clarified 2026-07-27).** Cinder's `PUT`/`DELETE /v3/quota-sets/{id}` have
**no** authorization check whatsoever — any authenticated caller can rewrite or reset any project's quota.
They become admin-only, matching Nova and upstream Cinder. Nova's `UpdateQuotaSet` additionally binds the
request body *before* its admin check (`internal/nova/quotas.go:156-160` precede `:162-174`), so a non-admin
with a malformed body receives 400 instead of 403; the check moves first. Both are deliberate, tested
exceptions to FR-016 — recorded rather than slipped in.

**Schema note.** `quotas.project_id` is `UUID NOT NULL REFERENCES projects(id)`
(`migrations/007_add_quotas.up.sql`), so the resolved target must be a UUID — a project *name* in the URL
would fail at the DB layer. Scoping to the caller's own JWT `project_id` (a UUID) for non-admins removes that
failure mode for the common path; Cinder's existing name→UUID lookup is retained for the admin path.

**Alternatives considered.** (a) A generic `.json`-stripping middleware — rejected as too broad (FR-016).
(b) Keeping the existing loose URL-id handling for consistency — rejected: it would ship *new* endpoints with
a cross-project read (Art. V). (c) A fourth hand-rolled admin loop — rejected in favor of `common.HasRole`.

**Gin routing constraints discovered (must be respected when adding routes).**
- **Cinder**: every flat `/v3/<static>/…` route must be registered **before** the `/v3/:project_id` group
  (`internal/cinder/volumes.go:158`). *(Corrected 2026-07-29: order is irrelevant — gin splices a static child ahead of an existing wildcard, tested in both orders on the vendored v1.12.0. The panic risk is a conflicting param **name**, not ordering.)* The file said so in
  comments at `:63` and `:145`. Precedent that a static sibling is fine: `/v3/types/default` coexists with
  `/v3/types/:id` (`:92`, `:94`).
- **Nova**: new siblings under `/os-quota-sets/:id` must reuse the param name `:id`; a differently-named param
  at the same position panics at registration.
- **Neutron**: `/quotas/:id/default` is a static child of an existing `:id` param — no conflict. A bare
  `/quotas/default` **would** be ambiguous, so `default` (if ever needed) is handled *inside* the `:id`
  handler, which is exactly the idiom nova already uses (`internal/nova/quotas.go:29`).

---

## R8 — Testing strategy (FR-015, Art. Test-First)

**Existing coverage (corrected 2026-07-27).** The changed endpoints are **not** untested today — they have no
*unit* tests, but three gophercloud contract tests and two bash scripts already exercise them:
`test/contract/nova/limits_test.go`, `test/contract/cinder/quotas_test.go`,
`test/contract/neutron/quotas_test.go`, `test/quota_test.sh`, `test/quota_admin_test.sh`. Two properties of
that suite were verified while planning, and they de-risk this feature's two intentional behavior changes:

- Its limit assertions are **value-agnostic** (`assert.GreaterOrEqual(..., 0)`), so the FR-003 change from
  100/200/512000 to 10/20/51200 does **not** break them.
- The contract client authenticates as **`admin`/`secret`** by default (`test/contract/helpers.go`), so making
  Cinder's quota `PUT`/`DELETE` admin-only (FR-020) leaves `TestCinderUpdateQuotaSet_Contract` /
  `TestCinderDeleteQuotaSet_Contract` passing.

Both must still be **confirmed by running the suites**, not assumed — hence layer 3 below. Note `make test`
deliberately **excludes** `test/contract`; the dedicated targets are `make test-contract` and
`make test-integration`.

**Decision.** Three layers, all using existing tooling — no new harness:

1. **Go unit tests** per service, written RED before the fix: the new/repaired quota + limits + extensions
   endpoints return 200 with the required keys; limits and quota-set **agree** (a direct G1/G12 regression
   guard); quota reads are project-scoped (admin vs non-admin, FR-002a); network responses carry `project_id`;
   every `subnets` id resolves in the subnet list (the F7 guard).

   **Harness (confirmed).** Each package already has an `httptest` router that wires the real `RegisterRoutes`
   behind a stub middleware setting `project_id`/`user_id`/`roles` from `X-Test-*` headers —
   `newNovaTestRouter()` (`internal/nova/testrouter_test.go`), `newNeutronTestRouter()`
   (`internal/neutron/network_db_test.go`), `newCinderTestRouter()` (`internal/cinder/volumes_db_test.go`).
   No test in the repo mints a real JWT; **omitting `X-Test-Roles` yields a non-admin caller**, which is
   exactly how the FR-002a 403/scoping cases are expressed.
   - DB-backed tests call `requireDB(t)` and skip unless **`O3K_TEST_DB_URL`** is set (set-but-broken is
     fatal, never a silent skip).
   - Handlers that touch **no** DB — nova `GetQuotaSetDefaults`, neutron `GetQuota` / `ListExtensions`, and
     the new Cinder `/extensions` — are testable **without** a database, so a useful part of the RED suite
     runs everywhere.
   - Quota tests use a **fresh throwaway project** (the `freshProjectID` idiom from
     `internal/nova/spec017_helpers_test.go`) so they never perturb the 13 quota rows migration `007` seeds
     for the default project.
   - Two harness papercuts to fix in passing: neutron's `doReq` takes **no body argument** (4 params, unlike
     nova/cinder's 5), so it must be extended if a quota write is tested; and cinder's `NewService` starts
     goroutines with a `Shutdown()` the harness never calls.
2. **A bash endpoint probe** in `test/`, following the established `test/horizon_compat_test.sh` pattern
   (token → `curl` each endpoint → assert status/keys with `jq`). This doubles as the R0 trace tool and as the
   integration-level guard that "every endpoint Horizon's aggregation calls returns success" (SC-005).
3. **The existing contract + script suites as a regression gate** — `make test-contract` plus
   `test/quota_test.sh`, `test/quota_admin_test.sh` and `test/horizon_compat_test.sh`, run against a running
   O3K. These already cover the changed endpoints, so FR-016's "existing unit **and integration** suites must
   continue to pass" is only actually verified if they are run.

**Rationale.** No *unit* coverage exists for these endpoints (G11), so new unit tests give fast,
mode-independent protection; the probe script asserts the same contract against a running binary and is
reusable in both runbook passes; and running the pre-existing contract/script suites is what proves the two
intentional behavior changes (FR-003 values, FR-020 authorization) broke nothing.

**Alternatives considered.** An automated browser suite — explicitly out of scope (spec, SPEC-018).

---

## R9 — Verification in both modes (FR-017, SC-006)

**Decision.** Re-run the two SPEC-018 runbooks unchanged, observing all four workflows:
- **Stub** — `deployments/docker-compose-horizon.yml`, per `018-…/HOST_TESTING.md`.
- **Real** — O3K on **k3s** via `charts/o3k` (real mode), **Horizon via Helm** (`deployments/horizon-chart/`),
  **Ceph + RGW (S3) via Rook on minikube** (`deployments/rook/setup-rook-rgw.sh`), per
  `018-…/HOST_TESTING_REAL_MODE.md`.

All four fixes are response-shape/routing changes with no mode-specific branches, so behavior is identical in
stub and real (Art. IV); the real pass additionally proves the launched instance actually boots (FR-008). A
feature-specific `HOST_TESTING.md` is generated later by the host-test tooling; this plan does not duplicate
those runbooks.

---

## Cross-cutting decisions

- **No new Go modules; no new packages.** All changes are in existing handler files plus tests (Art. VI/VII).
- **No migration, no schema change.** The `quotas` (migration `007`) and `cinder_quotas` (migration `039`)
  tables already exist and are sufficient; limits are reconciled *onto* them. Nothing here needs a column.
- **Read surface only.** No new quota *enforcement* on create paths (spec Assumption; out of scope) — the
  existing `CheckQuota` behavior is untouched.
- **No regression (FR-016).** Existing response fields stay; additions are additive (`project_id` alongside
  `tenant_id`, new routes, new keys). The one intentional value change is Nova's absolute limits (G1/FR-003),
  which is the point of the fix.
- **Report update.** On completion, tag F2/F3/F7/F8 as fixed in
  `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, mirroring the existing F4/F5/F6/F9 update block.
