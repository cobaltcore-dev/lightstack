# Phase 0 Research: Project/Tenant Lifecycle & Isolation Fixes

**Feature**: `022-project-tenant-isolation` | **Date**: 2026-08-04 | **Spec**: [spec.md](./spec.md)

Every decision below is anchored to code already in the tree. That is the point of this feature: all
four defects have a working sibling in the repository, so the research question is almost never *"what
should we build"* and almost always *"which existing thing do we copy, and where does it not fit"*.

**NEEDS CLARIFICATION remaining: none.** The four ambiguities the specification pass left open were
resolved during clarification and are recorded in `spec.md` § Clarifications.

---

## R1 — Where the domain resolver lives, and what shape it takes

**Decision**: one package-level function `resolveDomainRef(ctx, ref string) (id string, status)` in a
new file `internal/keystone/domain_resolve.go`. Not a type, not an interface, not a method on
`Service`.

**Rationale**:
- FR-008 requires a *single shared behaviour* across four call sites in three files
  (`handlers.go` × 2, `groups.go`, `domains.go`). A function in its own file gives those four sites a
  neutral home; putting it in `handlers.go` would make `groups.go` and `domains.go` import from an
  arbitrary sibling.
- The repository already has exactly this pattern for the analogous problem:
  `common.ResolveProjectRef` (`internal/common/project.go`) is a plain function that resolves a
  project reference by UUID-or-name for three services' quota routes, with a long comment explaining
  why the ambiguity exists. `resolveDomainRef` is its Keystone counterpart.
- A `Service` method was rejected: the resolver needs only a `context` and the package-level
  `database.DB` that every Keystone handler already uses, so a receiver would add a dependency the
  function does not have.

**Why `internal/keystone` and not `internal/common`**: `internal/common` is for what *several services*
share — `ResolveProjectRef` lives there because Nova, Cinder and Neutron all resolve project
references. No service other than Keystone resolves a domain reference. Placing it in `common` would
advertise reuse that cannot happen.

**Return shape**: a resolved id plus a tri-state outcome (`resolved` / `notFound` / `queryFailed`),
because FR-002 needs to distinguish "the client named a domain that does not exist" (a `400`) from "the
database is unreachable" (a `500`). `ResolveProjectRef` collapses those two into `false` — deliberately,
because every one of its callers answers `404` either way — but here the two answers differ, so the
signature must carry the distinction. `CreateUser`'s current code already makes this split by hand
(`handlers.go:1007-1021`): a failed lookup of a *supplied* domain is a `400`, a failed lookup of the
*default* domain is a `500`.

**Alternatives considered**:
- *A generic path-parameter alias framework* (register aliases per resource, resolve in middleware) —
  rejected as gross over-engineering for one alias on one resource. Article "Simplicity" and Article
  "Anti-Abstraction" both point the other way.
- *Resolve the alias in `uuidParamMiddleware`* — see R4; rejected for putting resource knowledge in a
  generic middleware.

---

## R2 — Resolve the default domain by **name**, never by the hard-coded UUID

**Decision**: the empty and `default` cases both resolve via
`SELECT id FROM domains WHERE name = 'Default' AND enabled = true`. The two hard-coded literals in
`handlers.go:456` and `groups.go:76` are retired. `defaultDomainID` in `bootstrap.go:20` stays, because
bootstrap *creates* the row and legitimately needs a fixed id.

**Rationale — this is not merely tidier, it removes a live inconsistency**: two migrations seed the
default domain with **different UUIDs**:

| Migration | UUID seeded | Conflict clause |
|---|---|---|
| `009_seed_default_domain.up.sql` | `00000000-0000-0000-0000-000000000100` | `ON CONFLICT (name) DO NOTHING` |
| `037_keystone_domains.up.sql` | `00000000-0000-0000-0000-000000000001` | `ON CONFLICT (name) DO NOTHING` |

Both insert the name `Default`. `009` runs first, so it wins and `037`'s insert is a no-op — which is
why the hard-coded `…0100` happens to be correct today. It is correct by migration *ordering*, not by
design. A name-based lookup makes that discrepancy harmless instead of load-bearing, and satisfies
FR-003's requirement that a deployment whose default domain carries a different identifier still works.

**Verification**: `test/contract/keystone/projects_test.go` and two sibling contract files assert
`…0100` in five places. Name-based resolution returns exactly the row those assertions describe, so
they act as the regression guard for this decision.

**Alternatives considered**:
- *Keep the literal and merely validate it exists* — leaves the ordering dependency in place and
  fails FR-003.
- *Fix migration 037 to seed the same UUID* — a migration edit for a row that is never inserted;
  changes nothing observable and violates the project's own "migrations are immutable once shipped"
  practice. See R9.

---

## R3 — Detect the duplicate project name by SQLSTATE, not by string matching

**Decision**: `errors.As(err, &pgErr)` on `*pgconn.PgError` and test `pgErr.Code == "23505"`
(`unique_violation`), then answer `409` via `common.NewConflictError`.

**Rationale**: the repository has both idioms, and one of them is better:

| Site | Idiom |
|---|---|
| `internal/nova/flavors.go:62` | `if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == "23505"` ✅ |
| `internal/keystone/handlers.go:1049` | `strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "users_name_key")` |

The string form depends on the server's error text and on a constraint name, both of which are outside
the code's control. `pgconn` is already an indirect dependency and `flavors.go` already imports it, so
the better idiom costs nothing. `common.NewConflictError` exists (`internal/common/errors.go:101`).

**Revised in branch review (2026-08-06).** The decision stands; its *location* changed. Once
`CreateGroup` became a third caller, three copies of the same SQLSTATE test existed, so it moved to
`common.IsUniqueViolation` (`internal/common/pgerror.go`) and all three now call that. This also
repairs the `flavors.go` idiom marked ✅ above: a bare type assertion matches only an unwrapped error,
so a single `fmt.Errorf("...: %w", err)` between pgx and the handler — which this project's own error
convention asks for — would silently downgrade a `409` to a `500`. `errors.As` unwraps; the helper's
test pins the wrapped and doubly-wrapped cases.

**Scope note**: `CreateUser`'s existing string match is **not** rewritten. It works, it is out of this
feature's requirements, and changing it would widen the diff on a handler this feature is already
touching for a different reason. Recorded as an observation for a later cleanup, not a task.

---

## R4 — The #515 exception is a route-group move, not a middleware change

**Decision**: move `GET /domains/:id` from the `val` group to the `mgmt` group in `RegisterRoutes`, and
have `GetDomain` resolve the alias and then validate the id itself. `PATCH /domains/:id`,
`DELETE /domains/:id` and `GET /domains/:id/config` stay on `val`.

**Rationale**: `uuidParamMiddleware` (`internal/keystone/uuid_middleware.go`) answers Keystone's
verbatim 404 for any path param named `id` or `*_id` that is not a well-formed UUID. `default` is not a
UUID, so it never reaches the handler. Two ways to fix that; the repository has already chosen one:

`handlers.go:57-66` documents the val/mgmt split verbatim — *"Those stay on `mgmt` and validate the id
in-handler AFTER their authz check, so 401/403 still precede the 404"*. Four routes already live on
`mgmt` for exactly this reason (`DELETE /users/:id`, `POST /users/:id/password`, and the EC2 credential
routes). Moving one more route for one more reason uses the existing mechanism rather than adding a
second one.

Registering `GET` on one group and `PATCH`/`DELETE` on another for the same path is already done in
this file for `/users/:id` (`GET` on `val`, `DELETE` on `mgmt`), so gin handles it and the pattern is
precedented.

**Alternatives considered**:
- *An alias allowlist inside `uuidParamMiddleware`* — puts resource-specific knowledge (which resource
  has which alias) into a middleware whose entire documented job is a syntactic check. It would also
  apply the exception to `PATCH` and `DELETE`, which nothing asks for.
- *Leave the read path broken* — clarification Q1 rejected this: standard clients resolve `--domain`
  client-side, so the alias would work only via a fallback.

**Guard**: `TestRegisterRoutesMiddlewareWiring` (`internal/keystone/routes_wiring_test.go:19`) already
asserts the split end-to-end with no database. It gains a case for the moved route: a malformed
(non-alias) id on `GET /domains/:id` must still be refused, now by the handler rather than the
middleware.

---

## R5 — The subnet entitlement check is one query that answers all three outcomes

**Decision**: replace nothing in `CreateSubnet`'s existing flow; insert, before the `INSERT`:

```
SELECT project_id, shared FROM networks WHERE id = $1
```

then branch: no row → `404`; `project_id` matches caller → allowed; caller holds `admin` → allowed;
`shared = true` → `403`; otherwise → `404`.

**Rationale**: one round trip decides everything FR-010/FR-010a/FR-011 need. `CreatePort`
(`internal/neutron/ports.go:57-65`) supplies the *shape* — resolve the network once, treat
`pgx.ErrNoRows` as "not found" — but not the rule: its predicate is
`WHERE id = $1 AND (project_id = $2 OR shared = true)`, which folds the decision into the `WHERE`
clause and therefore cannot distinguish `403` from `404`. Selecting the two columns instead of
filtering on them is what makes the two refusals separable.

**Ordering**: the check goes before both the `INSERT` and the `applyDHCPForNetwork` call
(`network.go:958`), satisfying FR-010's "no address-assignment activity may result". Note
`applyDHCPForNetwork` returns immediately in stub mode (`internal/neutron/reconciler.go:232`), so this
ordering is unobservable in stub and matters only in real mode — which is why it is stated as a
requirement rather than left to a test.

**Admin bypass**: `common.HasRole(c, common.RoleAdmin)` (`internal/common/roles.go:20`). This is
`internal/neutron`'s **first** use of it — the package has zero role checks today — so the call shape is
kept identical to the existing users in `internal/common/project.go` and `internal/keystone`.

**Malformed identifier**: checked with `uuid.Validate` before the query, answering `400` (FR-011a).
Without it the string reaches a `uuid`-typed column comparison and Postgres raises `22P02`, surfacing
as the `500` this feature exists to remove. `google/uuid` is already imported by `network.go`.

**Verified non-issues** (checked so the fix is not scoped wider than the defect):
- `CreateSubnetRequest` (`network.go:895`) is a **single** subnet object — there is no bulk-create path
  to cover.
- `UpdateSubnet` (`network.go:1191`) and `DeleteSubnet` (`network.go:1234`) both filter
  `AND project_id = $2`, and `UpdateSubnet` accepts only `name`, so a subnet cannot be moved between
  networks. No adjacent breach of the same class exists.

**Corrected during the branch review.** "No adjacent breach" holds for *writes into another tenant's
network*, which is what B8 is. It does not hold for the state B8's own administrator exemption
(FR-012) leaves behind. `CreateSubnet` attributes the row to the caller — upstream does the same
(`neutron_lib/api/attributes.py::populate_project_id` copies `context.project_id`) — so an
administrator's subnet on tenant B's network belongs to the administrator's project. Reading the
upstream policies rather than assuming them:

| Action | upstream `check_str` (`neutron/conf/policies/subnet.py`) | network owner? |
|---|---|---|
| `create_subnet` | `ADMIN_OR_NET_OWNER_MEMBER` | yes — what B8 implements |
| `get_subnet` | `PROJECT_READER or rule:shared or rule:external_network or ADMIN_OR_NET_OWNER_READER or SERVICE` | **yes** |
| `delete_subnet` | `PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER` | **yes** |
| `update_subnet` | `PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER` | **yes** |

with `network_owner` = `project_id:%(network:project_id)s` (`neutron/conf/policies/base.py`). So O3K's
`GetSubnet`, `UpdateSubnet` and `DeleteSubnet` were all **stricter than upstream**, while `ListSubnets`
— which SPEC-020 gave `n.project_id = $1` for an unrelated Horizon reason — already matched it. Tenant
B could see the subnet in its listing, get `404` by id, and neither rename nor delete it. FR-013a
brings the three outliers into line.

Note the two mutating rules are **identical** upstream, which is why rename came along with delete:
permitting the removal of a subnet while refusing to rename it draws a line upstream does not draw, and
renaming is the strictly weaker act. `rule:shared` is in `get_subnet` only — sharing a network lets
other tenants read its subnets, never mutate them.

---

## R6 — **Four** project-scoped sub-resource names are shadowed

**Decision**: add `GET /volumes`, `GET /volumes/detail`, `GET /snapshots/detail` and
`GET /types/default` to Cinder's `/v3/:project_id` group. Do **not** add `GET /backups/detail`.

**Rationale — this corrected the specification.** Shadowing requires a by-identifier sibling
registered at the same level: the literal name then matches that sibling and the request is served as
a lookup for a resource with that name. Counting routes present in the `/v3/:project_id` group:

| Sub-resource | `GET /<res>/:id` in the project group? | Routes for `<res>` in that group | Verdict |
|---|---|---|---|
| `volumes/detail` | yes | 10 | **shadowed** — in scope |
| `snapshots/detail` | yes | 11 | **shadowed** — in scope |
| `types/default` | yes | 12 | **shadowed** — in scope |
| `os-volume-transfer/detail` | yes (`volumes.go:282`) | 4 | **shadowed** — in scope *(added in the o3k#82 review)* |
| `backups/detail` | **no** | **0** | absent, answers an honest `404` — out of scope |
| `qos-specs/…` | no | 0 | absent — out of scope |

The specification pass inferred the shadowed set from the flat-vs-project-scoped route *difference*,
which is the wrong test — it identifies what is missing, not what is wrong. `backups` has no route at
all in the project group, so nothing swallows `backups/detail`.

**Corrected again in the o3k#82 branch review.** The test above is right; the *candidate list* it was
applied to was not. This table originally enumerated five names and never considered
`os-volume-transfer` at all, yet the project group registers `GET /os-volume-transfer/:id`
(`volumes.go:282`) — so `os-volume-transfer/detail` was shadowed the whole time, answering `500
"failed to get transfer"` from `GetVolumeTransfer` with `id="detail"`. That is the same signature as
#533 F8 (`/v3/groups/detail`). Found by probing every absent route against the real route tree rather
than reasoning about a hand-written list; the probe now lives in
`TestProjectScopedSubResourcesNotShadowed`.

So the census is **four shadowed, 14 merely absent** (18 missing − 4 in scope). Note the tally returns
to the four/14 the requirements checklist recorded before Iteration 5 — the number was right there by
coincidence, since that pass counted `backups/detail` (not shadowed) and missed
`os-volume-transfer/detail` (shadowed), two errors that cancelled in the total. The lesson is the one
this section already states, applied one level further out: derive the set from the route tree, not
from a list someone typed.

**Handlers** (confirmed present, reused unchanged — the project-scoped forms ignore `:project_id` and
read the caller's project from the token, exactly as every other route in that group does):
`ListVolumes` (`volumes.go:412`), `ListVolumesDetail` (`:495`), `ListSnapshotsDetail` (`:1153`),
`GetDefaultVolumeType` (`:1204`), and `ListVolumeTransfers` (`transfers.go:88`) — the flat twin
registers the `ListVolumeTransfersNoProject` alias, which does nothing but call it.

**gin ordering**: irrelevant. `volumes.go:112` documents it, verified against the vendored gin v1.12.0:
a static child is spliced ahead of an existing wildcard however it is registered. What *does* panic is
reusing a different param name at a position already claimed by one, which is why every route in that
group uses `:id`.

**Alternatives considered**: full flat↔project parity behind a generated guard test (clarification Q2
option C) — rejected, because it adds 19 routes with no coverage beyond the guard itself.

---

## R7 — Keep O3K's existing error envelope for the new Neutron `403`

**Decision**: use `common.NewForbiddenError` and `common.NewNotFoundError`, producing O3K's standard
envelope `{"<code>": {"message": …, "code": <status>}}`. Do not introduce upstream Neutron's
`{"NeutronError": {…}}` wrapper.

**Rationale**: upstream Neutron wraps errors in `NeutronError`, and O3K does not — anywhere. Every
existing Neutron refusal, including `CreatePort`'s `404` for an unreachable network
(`ports.go:63`), uses the shared envelope. Adopting the upstream wrapper for one new `403` would make
Neutron's error surface internally inconsistent, which is worse for clients than being uniformly
non-upstream: real clients (python-neutronclient, gophercloud, Horizon) branch on the HTTP status and
surface the message string, and both are correct here.

`NewForbiddenError` already exists (`internal/common/errors.go:56`) and is currently unused in
`internal/neutron` — this is its first use there.

**Recorded as an observation, not a task**: O3K's Neutron error envelope diverges from upstream
across the whole service. That is a real Article-I question, far larger than this feature, and it
belongs in #536's family rather than here.

---

## R8 — Harness: trap-based teardown, and guards that catch the literal `null`

**Decision**: in `test/multi_tenant_test.sh`, register a teardown trap on `EXIT` *before* the first
resource is created, move the existing end-of-script cleanup block into it, and add a guard helper
applied to every id and token the setup produces.

**Rationale**:
- The suite runs `set -e` and creates `test-project-1` / `test-project-2` under **fixed** names, with
  cleanup only at the very end. Once FR-004 turns a duplicate name into a `409` and FR-017 exits setup
  early, a run that dies part-way leaves a project behind and every later run aborts at setup. The
  fixes in this very feature create that failure mode, so the harness change is not optional polish.
- A trap keeps the fixed, readable names (they appear in the suite's own output) and removes the
  duplication that a second cleanup path would introduce.
- The guard must reject the literal string `null` as well as the empty string. That is the precise
  defect T1 was: `jq -r` prints `null` for a missing field, and `[ -z "null" ]` is false, so the
  existing `ADMIN_TOKEN` guard shape (`multi_tenant_test.sh:43`) would not have caught it. T1 fixed the
  token *extraction* (all three suites now read `X-Subject-Token`); it did not add guards for the ids.

**Precedent**: the three suites repaired in the #534 review — `multi_tenant_test.sh:41`,
`performance_test.sh:28`, `vxlan_multinode_test.sh:243,315` — already extract from the header. This
feature verifies that (FR-020) rather than redoing it.

**New checks** (FR-018, FR-019): the cross-tenant subnet write extends the existing read-only network
isolation check (`multi_tenant_test.sh:146-166`); the project-scoped volume **list** is genuinely new —
the suite creates and deletes volumes on that URL form (`:171`, `:285`) but never lists, which is
exactly how B2 escaped it.

---

## R9 — No migration, and migration `037`'s dead seed is left alone

**Decision**: this feature ships **no** migration. Migration `037_keystone_domains.up.sql`'s second
`Default` seed (with the other UUID) is not corrected or removed.

**Rationale**: every requirement is behavioural — a lookup, a rule, four routes, a log line and test
coverage. No column is added, altered or dropped; `data-model.md` records the columns each requirement
reads and confirms all of them already exist.

The dead seed in `037` inserts nothing (`ON CONFLICT (name) DO NOTHING`, and `009` already holds the
name), so it has no runtime effect. Editing a shipped migration would change a file that deployments
have already applied and recorded, for zero observable benefit — and R2 removes the code's dependence
on which UUID won, which is the real fix. Recorded as an observation.

**Migration reversibility gate** (Constitution, Backwards Compatibility): trivially satisfied — there is
nothing to reverse.

---

## Observations for follow-up (T056)

Found while implementing, deliberately **not** fixed here. Each is either out of this feature's scope or
belongs to #549.

| # | Observation | Why not here |
|---|---|---|
| O1 | `CreateProject` has **no admin gate**, while `CreateUser` does (`handlers.go:972`). Upstream restricts `identity:create_project` to administrators | A policy decision of exactly the kind #536 moved to #549 (A6, FR-023) |
| O2 | `GET /v3/domains/default/config` still cannot resolve the alias — only the retrieval route moved off the validating group | Nothing asks for it; the exception was deliberately scoped to one route (A11) |
| O3 | O3K's Neutron error envelope diverges from upstream's `NeutronError` wrapper **service-wide**, not just for the new 403 | A real Article-I question far larger than this feature (R7) |
| O4 | `CreateUser`'s duplicate detection still string-matches the error text (`handlers.go:1049`) rather than checking SQLSTATE, which `CreateProject` now does | Works today; rewriting it would widen the diff on a handler already touched for another reason (R3) |
| O5 | Migration `037` seeds a second `Default` domain with a different UUID; the insert is a no-op | Editing a shipped migration for zero observable benefit; R2 removes the code's dependence on which won |
| O6 | **Placement's port is hardcoded** at `cmd/o3k/main.go:715` (`:8778`) — the only service whose port is not read from config. Since `main.go:300` does `log.Fatalf` on any bind failure, two O3K instances can never coexist on one host, which also blocks running a second instance for verification | Discovered while trying to stand up an isolated stack. Not a defect this feature owns, but a real gap: the chart cannot relocate placement either |
| O8 | **Identity `users` and `domains` responses omit `links`**, so `openstack user create` and `openstack domain show` end in a CLI-side `'links'` KeyError even when the API returns `201`/`200` and the resource is created. `projects` responses do carry it, which is why `openstack project create` works. Proven pre-existing: `openstack domain show <UUID>` fails identically | A response-shape gap across the identity surface, not something this feature's alias work touches |
| O9 | **`GET /v3/roles?name=…` ignores the filter** — the same class as `ListDomains` did — returning every role, so `.roles[0]` is `admin` (alphabetically first). Combined with there being no `_member_` role (O3K seeds `admin`/`member`/`reader`), `multi_tenant_test.sh` had been granting **admin** to both test users, making every isolation assertion in it a tautology. Fixed client-side in the suite; the server-side filter is untouched | FR-001a scoped the filter work to domains, the one path clients use for `--domain` resolution. Roles' filter is the same one-line fix and an obvious follow-up |
| O10 | **A false FAIL in `multi_tenant_test.sh`**: the cross-project volume-attach check read `jq -r '.error // .badRequest // "success"'`, but O3K refuses with `{"itemNotFound": …}`, so jq fell through to the literal `"success"` and it reported a breach that does not exist (the attach really returns `404`). Fixed here — it is in the suite US4 owns and it blocked SC-006 | In scope: the suite must report truthfully, which is the whole of US4 |
| O7 | The `((VAR++))` + `set -e` bug appears in **9** suites (`horizon_compat_test.sh`, `horizon_dashboard_test.sh`, `horizon_full_test.sh`, `horizon_resource_lifecycle_test.sh`, `phase3_test_suite.sh`, `performance_test.sh`, `quick_test.sh`, `integration_test.sh`, `quickstart_validation_test.sh`). Only `horizon_compat_test.sh` is fixed here — the one SC-008 names. The other **8** are a follow-up | Fixing 9 suites is scope creep; leaving the one SC-008 depends on broken would make its verdict meaningless |
| O7b | `test/horizon_compat_test.sh` **could not report more than one failure**: `set -e` (line 5) plus `((FAILED++))` in `print_fail` (line 31) terminates the script, because the arithmetic expression evaluates to `0` — a non-zero exit status — on the first failure. So the summary never shows the full picture and later checks are unreachable | Same family as T1. Fixed here only because SC-008 cannot otherwise be verified past the first check; see EVIDENCE.md |

## Summary of decisions

| # | Decision | Anchored to |
|---|---|---|
| R1 | One resolver function in `internal/keystone/domain_resolve.go`, tri-state return | `common.ResolveProjectRef`; `CreateUser`'s hand-rolled 400/500 split |
| R2 | Resolve the default domain by **name**; retire both hard-coded literals | migrations `009` vs `037` seeding different UUIDs |
| R3 | SQLSTATE `23505` for the duplicate-name `409` | `internal/nova/flavors.go:62` |
| R4 | Route-group move for `GET /domains/:id`, not a middleware change | `handlers.go:57-66`; `routes_wiring_test.go:19` |
| R5 | One `SELECT project_id, shared` decides 404 / 403 / allowed | `internal/neutron/ports.go:57-65` (shape, not rule) |
| R6 | **Three** shadowed routes, not four; `backups/detail` is absent | route-group census; corrected the spec |
| R7 | Keep O3K's error envelope for the new `403` | every existing Neutron refusal |
| R8 | Trap teardown + guards that reject literal `null` | the T1 defect; `multi_tenant_test.sh:43` |
| R9 | No migration; leave `037`'s dead seed | `ON CONFLICT (name) DO NOTHING` |
