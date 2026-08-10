# Phase 0 Research: Tenant Scoping & Authorization Model

**Feature**: `023-tenant-scoping-authz` | **Date**: 2026-08-10  
**Base**: `feature/545` @ `9992ebf`

The specification carries zero `[NEEDS CLARIFICATION]` markers — all seven policy questions were settled during clarification. This document resolves the *design* unknowns that remain, and records three defects discovered while surveying the code.

---

## R1 — Shape of the override rule

**Covers**: FR-001, FR-002, FR-003.

**Decision**: Two shapes, chosen per operation by where the target project comes from. Neither is new. The rule keys off the role the caller's token carries, never off membership of the target project (FR-002) — both shapes read `common.HasRole` and neither consults `role_assignments` for the target.

| Shape | Used when | Existing implementation |
|---|---|---|
| **Scope-derived** — look the resource up, return its owner; non-administrators get a project-scoped lookup instead | The project comes from the caller's token (every `/servers/{id}` operation) | `lookupConsoleInstance`, `internal/nova/console.go:189` |
| **Parameter-derived** — resolve the project named in the path; a non-administrator naming any project but their own is refused (`403`) before any lookup | The project is a path parameter (`os-simple-tenant-usage/:id`, quota routes) | `common.QuotaTargetProject`, `internal/common/project.go:78` |

Generalise `lookupConsoleInstance` into a nova-level helper that every server operation calls, and have the console call the generalised form. That removes the console's status as a special case (a stated goal of FR-001) without inventing a third pattern.

**Rationale**: Both shapes are already in production, both are already documented, and one of them (`lookupConsoleInstance`) already encodes the disclosure rule FR-003 requires. Writing a third mechanism would leave three ways to ask the same question.

**Alternatives rejected**:
- *Gin middleware applied to the route group* — the target identifier is a path parameter with different names across routes, and several handlers need the owning project as a **value** (for FR-007's attribution), not just a yes/no. Middleware would have to stash it in the context anyway, which is a less direct version of a function call.
- *A generic `authorize(c, resource, action)` policy engine* — Constitution Art. VII (simplicity) and the explicit Out-of-Scope entry. One rule does not need an engine.

**Note on reuse**: `QuotaTargetProject` is named for quotas and treats `default` and the empty string as "my project". `os-simple-tenant-usage/:id` has no `default` alias upstream, so reusing it makes `/os-simple-tenant-usage/default` mean "my usage" — a benign superset, consistent with the repo already accepting project *names* where upstream accepts only UUIDs. Reuse as-is; forking it for one caller would duplicate the security-relevant "do not resolve a non-administrator's foreign id" behaviour.

---

## R2 — Parsing the cross-tenant flag

**Covers**: FR-005, FR-005a, FR-005b.

**Decision**: A small shared helper. `strconv.ParseBool` is insufficient on two counts.

| Input | Result |
|---|---|
| parameter absent | false |
| present, **empty value** (`?all_tenants`) | **true** |
| `1 t true on y yes` (case-insensitive) | true |
| `0 f false off n no` | false |
| anything else | false |

**Rationale**: Verified against the Nova API reference — `all_tenants` "defaults to True" when used without a value, and accepts the wider vocabulary above. `strconv.ParseBool` accepts only `1/t/T/TRUE/true/True/0/f/F/FALSE/false/False`, so it rejects `on`, `yes`, `y`, `no` and cannot express the valueless form at all. Horizon and `openstack server list --all-projects` both send forms this must accept.

The repo already has two ad-hoc bool query parses (`internal/neutron/network.go:541`, `internal/cinder/quotas.go:140`), the latter carrying a comment about python-cinderclient sending `True` — evidence that the vocabulary question is live. The helper is placed in `internal/common` so those can adopt it later without this feature touching them.

**Alternatives rejected**: *Use `strconv.ParseBool` and accept the narrower vocabulary* — silently ignores `?all_tenants` and `?all_tenants=yes`, i.e. exactly the calls this feature exists to serve.

---

## R3 — Paging stability across projects

**Decision**: Add `id` as a tiebreaker to the ordering in both list handlers, make the marker comparison composite, and drop the project predicate from the marker lookup when the cross-tenant view is active.

**Problem found**: the two list handlers page *differently*, and neither ordering is total.

- `ListServers` (`internal/nova/handlers.go:906`) resolves its marker with `ROW_NUMBER() OVER (ORDER BY created_at DESC)` filtered by project, then uses `LIMIT/OFFSET`.
- `ListServersDetail` (`:977`) resolves its marker to a `created_at` value and filters `i.created_at < $n`.

Both order by `created_at DESC` alone. `created_at` is not unique, so ties already make page boundaries non-deterministic; widening to every project makes ties substantially likelier (scripted and seeded instances share timestamps). FR-005 requires page boundaries that neither skip nor repeat.

Both marker lookups are scoped `AND project_id = $2`, so under a cross-tenant listing an administrator's marker pointing at another project's server would silently fail to resolve — and both handlers *ignore* that failure (one logs a warning and returns the full page, the other leaves the condition off), so the caller would silently get page 1 again.

**Rationale**: `(created_at, id)` is total because `id` is the primary key. This is the standard keyset fix and needs no schema change; `idx_instances_created_at` already exists.

**Alternatives rejected**: *Leave paging alone and document the limitation* — FR-005 makes stable boundaries a requirement, and the silent page-1 repeat is a correctness bug an operator would not detect.

---

## R4 — Which handlers are in scope

58 handlers in `internal/nova` read the caller's project or scope a query by it. Classified:

| Class | Count | Treatment |
|---|---|---|
| **Server operations — override applies** (get, delete, action, metadata, tags, diagnostics, instance-actions, volume attach/detach, interface attach/detach, update, console, and every action in `advanced_actions.go`) | 44 | Scope-derived shape (R1) |
| **Listings — override applies via opt-in** (`ListServers`, `ListServersDetail`) | 2 | R2 + R3 |
| **Creation — explicitly excluded** (`CreateServer`) | 1 | Unchanged; FR-004a |
| **Project-as-parameter** (`GetQuotaSet`, `GetQuotaSetDefaults`, `GetQuotaSetDetail`, `GetTenantUsage`, `ListTenantUsage`) | 5 | Usage two per FR-008b–d; the three quota routes already resolve their `:id` through `QuotaTargetProject` |
| **Caller-scoped, correctly** (`GetLimits`) | 1 | **Untouched.** It reads `c.GetString("project_id")` directly and has no `:id` parameter — `GET /v2.1/limits` reports the caller's own limits by definition, which matches upstream. It is *not* a quota route and does not use `QuotaTargetProject`. Upstream additionally accepts an optional `tenant_id` for administrators; Lightstack does not implement that and this feature does not add it. |
| **Deployment-scoped — administrative gate** (`ListMigrations`, 9 aggregate handlers) | 10 | R11; FR-008e, FR-008f |
| **Per-server migrations — ownership check *plus* override** | 4 | R11; FR-008g |
| **Out of scope** (flavors, hypervisors, availability zones, services, server groups) | 4 | Untouched — not server operations, not named by FR-004 |

**Server groups are out of scope.** `os-server-groups` is a distinct resource, not a server operation, and FR-004's enumeration does not name it. Recorded here so the omission is deliberate rather than overlooked.

**Hypervisors, availability zones and services** report deployment topology and are arguably the same class as the aggregates in R11. They are *not* included: unlike aggregates they are read-only, and unlike the migration list they expose no per-project data. Flagged here so the boundary is a decision rather than an oversight.

---

## R5 — Where to reject a domain scope

**Decision**: Reject once, in the HTTP handler (`AuthenticateToken`, `internal/keystone/handlers.go`), immediately after binding and before dispatching to either authentication path. `ScopeField` gains a `Domain` member that is *parsed but not acted upon*.

**Rationale**: FR-016 requires the password and token paths to agree. A single check placed before the fork satisfies that by construction rather than by discipline — the two paths carry duplicated scope logic today (`internal/keystone/auth.go:201` and `:385`) and have already drifted once.

Rejecting inside `ScopeField.UnmarshalJSON` was the obvious alternative and is wrong: `AuthenticateToken` turns any bind failure into `400 "invalid request body: " + err.Error()`, which would conflate an unsupported scope with malformed JSON and bury the message. Placing the check in the handler yields a controlled `400` naming the domain scope.

Rejecting **before** resolving the reference is what makes the refusal uniform across domains that exist, do not exist, the caller has no role in, and malformed identifiers — which is how FR-014's non-disclosure property is obtained. It falls out of the design rather than needing to be enforced.

**Bad credentials return `401`**, so a `400` is already distinguishable from a credential failure (FR-013) without special wording.

**Critical regression risk**: `ScopeField.Project` embeds its own `Domain` qualifier (`internal/keystone/auth.go:60`). That nested form is what Horizon, Terraform and the CLI all send on every authentication. The new check must test for a **top-level** domain only. This is the single highest-risk line in the feature and gets a dedicated contract test.

---

## R6 — Recording cross-project actions

**Decision**: Pass the owning project to the `instance_actions` insert, and extend the write path to the operations the override covers. No migration.

**Schema is already sufficient** (`migrations/024_nova_instance_actions.up.sql`): `instance_id`, `action`, `request_id`, `user_id`, `project_id UUID NOT NULL`, `start_time`, `message`. Recording the owner is a change of *which value* is passed, not of shape — confirming spec assumption A-007.

**Gap found**: there is exactly **one** insert site (`internal/nova/handlers.go:621`, on create). Every other operation writes nothing. FR-007a exists because of this: recording cross-project actions against the owning project is meaningless if the operations the override covers never produce a row. The read side (`internal/nova/diagnostics.go:84`) selects by `instance_id` behind the instance ownership check, so rows carrying the owner surface correctly in the tenant's own view once written.

Action names follow upstream's vocabulary (`delete`, `stop`, `start`, `reboot`, `resize`, `confirmResize`, `revertResize`, `rebuild`, `pause`, `unpause`, `suspend`, `resume`, `lock`, `unlock`, `shelve`, `unshelve`, `migrate`, `live-migration`, `evacuate`, `resetState`, `changePassword`, `createImage`, `createBackup`, `restore`, `attach_volume`, `detach_volume`, `attach_interface`, `detach_interface`; a force delete is recorded as `delete`, as upstream records it) so the existing `os-instance-actions` endpoint stays client-compatible.

**Refusal logging** mirrors `internal/neutron/network.go:964`, which #545 added under *its own* FR-011b (that identifier belongs to SPEC-022, not to this spec — this feature's equivalent is FR-007b): a `Warn` naming caller project, target and owner, emitted only on refusal.

---

## R7 — Identity collection filters

**Decision**: Follow the shape #545 established on `ListDomains` (`internal/keystone/domains.go:20`) — build the `WHERE` conditionally from present query parameters, leaving the unfiltered path byte-identical.

Six collections currently read **zero** query parameters:

| Collection | Handler | Filters to honour (minimum: `name`) |
|---|---|---|
| roles | `internal/keystone/handlers.go:624` | `name` |
| users | `internal/keystone/handlers.go:324` | `name`, `domain_id`, `enabled` |
| projects | `internal/keystone/handlers.go:385` | `name`, `domain_id`, `enabled`, `parent_id` |
| groups | `internal/keystone/groups.go:17` | `name`, `domain_id` |
| services | `internal/keystone/services.go:18` | `type`, `name` |
| endpoints | `internal/keystone/services.go:269` | `interface`, `service_id`, `region_id` |

**Out of scope by FR-027**: the network service's subnet collection (`ListSubnets` ignores `network_id`) — same bug class, owned by #546's B6.

---

## R8 — Usage endpoints

**Decision**: `ListTenantUsage` gains an administrative-role gate; `GetTenantUsage` routes its `:id` through `common.QuotaTargetProject`; both stop reporting zeroes for errors.

Confirmed against the Nova API reference: `GET /os-simple-tenant-usage` "lists usage statistics for **all tenants**", `GET /os-simple-tenant-usage/{tenant_id}` reports one. The aggregate form is inherently cross-tenant, which is why FR-008b gates it.

**Current state** (`internal/nova/tenant_usage.go`): the aggregate form groups by `i.project_id` with no predicate and no role check; the per-project form passes `c.Param("id")` straight into `WHERE i.project_id = $1` with neither. Both swallow every error — including a malformed-UUID cast failure — into a `200` reporting zeroes, which a caller cannot distinguish from a real project owning no instances (FR-008d).

**Existing contract test is unaffected**: `test/contract/nova/tenant_usage_test.go` authenticates as `admin` and asserts `200`, which still holds after the gate.

---

## R9 — Test strategy

**Covers**: FR-019, FR-020, FR-021, FR-022.

**Decision**: Revive the disabled RBAC contract test, extend the multi-tenancy suite, add targeted contract tests for the two highest-risk behaviours.

FR-020's deliberately-red assertion is a documented mutation, not permanent code: remove one project predicate from a point lookup, confirm the suite reports failure, restore it. FR-021 is satisfied by *not regressing* #545's guarantees — the suite's existing member≠admin guard, non-admin token assertion and `trap teardown EXIT` are re-run unchanged and must still hold after the override lands.

**`test/contract/nova/rbac_test.go.broken` already tests reset-state and evacuate** — the exact two operations FR-008a repairs. It was written as T048 of SPEC-002 and is explicitly excluded from coverage accounting (`docs/testing/BLUEPRINT_TEST_COVERAGE.md:51`).

Cause of breakage confirmed by compiling it: `undefined: resetstate.ResetStateOpts`. The pinned `gophercloud v1.14.1` signature is `ResetState(client, id string, state ServerState)`, so the fix is `resetstate.ResetState(adminClient, server.ID, resetstate.StateError)` — a two-line change per call site, not a rewrite.

It is necessary but **not sufficient** for FR-008a: its administrator creates the server itself, so the admin path exercises the administrator's *own* project and passes today. It needs a second project's server to prove the fix.

Highest-risk behaviours needing dedicated tests:
1. The nested `scope.project.domain` form still authenticates (R5's regression risk).
2. A non-administrator passing the cross-tenant flag is ignored, not refused, and the answer is byte-identical to the same request without it (FR-005b).

The multi-tenancy suite already supplies non-administrative fixtures, the member≠admin guard and trap-based teardown (#545), so User Story 4 only adds positive override coverage and the deliberately-red check.

---

## R10 — Disclosure rule

**Decision**: For point lookups, answer identically whether the resource is absent or owned by another project — the console's existing rule.

The repo contains two different disclosure ladders. `lookupConsoleInstance` returns no-rows for both cases "because a 403 would confirm the instance exists". Glance's `imageDeleteAuthz` (`internal/glance/images.go:454`) returns `403` when an image is *visible but not owned* and `404` otherwise. The difference is justified: Glance has a `public` visibility concept, so some images are legitimately visible to a caller who may not delete them. Compute has no such concept — a server is either yours or invisible — so the ladder has no second rung to stand on.

FR-003 requires behaviour identical to today's for non-administrators, and today's is a project-scoped query returning no rows. The console rule is therefore both the precedent and the status quo.

---

## R11 — Deployment-scoped surfaces

**Covers**: FR-008e, FR-008f, FR-008g. Added after planning surfaced these and the decision was taken to fold them in.

**Decision**: Gate them administratively, not with the owner-or-administrator override. These resources have **no owning project**, so the override's fallback branch has nothing to fall back to.

That absence is also the root cause. Every other handler in `internal/nova` was written against a table with a `project_id` column, so a scoping predicate was the obvious thing to write. `aggregates` and the deployment-wide migration listing have no such column, so no predicate was written — and no gate was written in its place.

| Surface | Handlers | Current state | Fix |
|---|---|---|---|
| `GET /os-migrations` | `ListMigrations`, `internal/nova/migrations.go:14` | No gate. Returns every project's `server_uuid`, `source_node`, `dest_node`, flavour changes. Its own comment reads *"admin endpoint in real OpenStack"*. | Administrative gate |
| `/os-aggregates` (6 routes, 9 handlers) | `internal/nova/aggregates.go` | **Zero** role checks, zero forbidden paths. Six are writes: create, update, delete, add host, remove host, set metadata. | Administrative gate on every one |
| `/servers/{id}/migrations` (4 routes) | `internal/nova/migrations.go:72,132,176,201` | **Zero** project predicates and **zero** role checks. Any tenant may read, act on and **delete** another tenant's server migration records. | Ownership check **plus** the override (R1) |

**Rationale for gating rather than overriding**: upstream's aggregate policies are administrator-only across the board, including index and show — aggregate topology describes the deployment's scheduling layout, which is operator information. The migration listing is the same shape as the usage aggregate already handled by FR-008b, and takes the same treatment.

**Note on the per-server migration handlers (FR-008g)**: these are the odd ones out. FR-004 names "migrations" among the operations the override covers, but there is no existing scoping to relax — the work is to *add* an ownership check and then apply the override on top. This makes them the only in-scope handlers where the change tightens behaviour for a non-administrator rather than loosening it for an administrator.

**Alternatives rejected**:
- *Owner-or-administrator on aggregates via the hosts they contain* — hypervisors have no project either; the chain never reaches an owner.
- *Leave the aggregate read paths open and gate only the writes* — upstream gates reads too, and the topology is operator information. Half-gating would need a documented divergence for no benefit.
- *Defer to a follow-up ticket* — considered and rejected by decision: shipping a feature that establishes "administrators act across projects, tenants do not" beside three surfaces contradicting it would leave the rule stated but not true.

---

## R12 — Found during host validation: `CreateNetwork` silently ignores a requested project

**Not in scope.** Recorded because it is adjacent to this feature's subject and it cost real
debugging time during the real-mode host pass.

`internal/neutron/network.go` `CreateNetwork` reads `c.GetString("project_id")` and never looks at
`project_id`/`tenant_id` in the request body. So `openstack network create --project other-tenant`
run as an administrator returns **`201`** and creates the network in the *administrator's* project.

This is the same dishonesty pattern this feature fixes for domain scope: the client asks for
something, the server ignores it and reports success. FR-004a settles the equivalent question for
servers explicitly — an administrator may **not** create into another project, and is refused rather
than silently redirected. Neutron neither honours nor refuses.

How it surfaced: the runbook's fixture used `--project`, so both tenant networks landed in the
admin's project. Tenant A's instance then failed port allocation with
`network … not found` — **and booted anyway**, because that error is logged rather than returned —
leaving `addresses: {}`. That is indistinguishable from the override defect R1 exists to detect, so
it read as a product failure until the network ownership was checked directly.

Two consequences worth carrying forward:

- The runbook now builds networks with the **tenant's own token** and asserts each network's owner
  before creating instances.
- "An instance boots successfully with no port" is itself a fail-open worth questioning. Out of scope
  here, but it belongs with D2/D3 in the follow-up conversation.

## R13 — Found during host validation: `os-start` after `os-stop` fails in real mode

**Not in scope. Not caused by this feature.** Recorded because it fails an R3 line and would
otherwise be mistaken for an authorization defect.

`POST /servers/{id}/action {"os-stop": null}` returns `202`, then `{"os-start": null}` returns
**`500`** with:

```
domain not found: Domain not found: no domain with matching uuid '<instance id>'
```

Evidence it is not authorization:

- The **owner** acting on their **own** server fails identically (`os-stop` `202`, `os-start` `500`).
  Authorization is not consulted differently in the two cases, so the override cannot be the cause.
- Nothing in SPEC-023 touches `StopVM`/`StartVM` in `pkg/hypervisor/libvirt.go`.
- The lightstack pod had **0 restarts** between instance creation and the failure, so a lost in-pod libvirt
  state does not explain it.

What the state looks like afterwards: the database still reports `status=ACTIVE`, `power_state=1`
and a populated `libvirt_domain_id`, while libvirt has no domain with that uuid. **The database and
the hypervisor have diverged**, and nothing reconciles them — so the API keeps reporting a running
server that does not exist.

Two things worth noting for whoever picks this up. Instances are created with
`DomainDefineXML` + `DomainCreate` (persistent, so a shutdown domain should survive as "shut off"),
and `StopVM` calls `DomainShutdown` rather than destroy/undefine — so on the face of it the domain
should still be there. And the divergence is silent: `GET /servers/{id}` reports `ACTIVE` either way.

Belongs with D2/D3 and R12 in the follow-up conversation.

## Backwards-compatibility note

FR-008e and FR-008f are the only changes in this feature that **remove** access a caller has today. Everything else either grants an administrator something they lacked, refuses something that was silently degraded, or is invisible to existing clients.

No supported client depends on the current behaviour: Horizon reaches both surfaces only from its Admin panel, authenticated as an administrator, and neither `python-openstackclient` nor the Terraform provider calls them on a tenant path.

The existing contract tests continue to pass. Verified: `tenant_usage_test.go`, `migrations_list_test.go` and `aggregates_test.go` all build their client through `setupNovaClient`, which authenticates as `OS_USERNAME` defaulting to `admin`. One caveat worth carrying into implementation — that default means the suite would break if `OS_USERNAME` were pointed at a non-administrative user, so the new gates make an implicit assumption explicit rather than introducing one.
