# Phase 0 Research: Neutron API Fidelity & Real-Mode Networking

**Feature**: `024-neutron-api-fidelity-realmode` | **Date**: 2026-08-11 | **Base**: `f34d498`

The spec carried no `[NEEDS CLARIFICATION]` markers into planning — the seven open decisions were
settled during clarify. This phase resolves *how*, by reading the code the six defects live in. Two
findings change the size of the work substantially (R1 and R5); one changes which fix direction is
even coherent (R3).

---

## R1 — B10: the 244 call sites are a red herring; there is exactly one choke point

**Decision**: make the error envelope a property of the *service engine*, decided once, and render it
inside the existing `common.SendError`. **Zero of the 244 call sites change.**

**Rationale**: every error path in the service — not just handler returns — already funnels through
one function.

| Path | Where | Reaches `SendError`? |
|---|---|---|
| Handler errors (244 sites) | `internal/neutron/*.go` | directly |
| Auth failure / abort | `internal/middleware/auth.go:32,39` → `common.AbortWithError` | yes (`error_helpers.go:69`) |
| Panic recovery | `internal/middleware/errors.go:23` | yes |
| `c.Errors` from bind failures | `internal/middleware/errors.go:37,42` | yes |
| Unrouted path 404 | `internal/middleware/errors.go:59` | yes |
| Wrong method 405 | `internal/middleware/errors.go:67` | yes |
| Database helpers | `internal/common/error_helpers.go` (all 6 helpers) | yes |

The **only** error emission in the whole neutron package that bypasses it is
`auto_allocated_topology.go:30` — the hand-rolled correct `NeutronError`, which FR-009 already
requires folding in. So the conversion is: add a service marker to the context on the Neutron
**engine**, teach `OpenStackError.ToJSON` (or a renderer beside it) to emit Neutron's shape when that
marker is present, and delete the hand-rolled block.

⚠️ **Engine level, not the route group** — the table above is exactly why. `createNeutronServer`
(`cmd/lightstack/main.go:626-637`) applies `AuthMiddleware` at `:632` and registers `NoRoute` at `:633` and
`NoMethod` at `:635`, all *before* `svc.RegisterRoutes(r.Group(""))` at `:637`. Auth aborts before any
group middleware runs, and `NoRoute`/`NoMethod` never run inside a group at all, so a group-level
marker would convert the handler paths and leave three client-visible paths in the compute envelope.
Neutron owns its `gin.New()` engine, so an engine-level marker cannot reach another service.

This also satisfies the spec's edge case "an error occurs before the request has been authenticated
or routed to a resource handler" **by construction** rather than as extra work — those paths are
already listed above.

**Alternatives rejected**:
- *Mechanical replacement of 244 `common.SendError` → `neutron.sendError`.* 244 edits across 14
  files, and it misses every middleware path, so auth failures and unrouted 404s on `:9696` would
  keep answering in the compute envelope. Worse: it looks finished.
- *A response-rewriting middleware that unmarshals and re-wraps the body.* Needs a body-capturing
  `ResponseWriter`, parses JSON it just produced, and cannot recover the resource name for the
  `type` field. All cost, no fidelity.
- *A second parallel constructor set (`NewNeutronNotFoundError`, …).* Duplicates 20 constructors and
  leaves the two sets free to drift — the exact failure mode B9 is.

**Consequence for the shared type**: `OpenStackError` needs one additive field carrying the resource
name (`NewNotFoundError` formats it into the message today and discards it), because the `type` value
is derived from it. Additive, no call-site change, no behaviour change for other services.

---

## R2 — B10: the `type` vocabulary, from upstream source

**Decision**: derive `type` from `(StatusCode, Code, Resource)`. Use upstream's real class name where
one exists; one documented generic otherwise. Enumerate the whole mapping in a single table.

**Rationale**: upstream's `type` is literally `e.__class__.__name__`
(`neutron/api/api_common.py`: `err_data = {'type': e.__class__.__name__, 'message': e, 'detail': ''}`).
The names come from `neutron_lib/exceptions/__init__.py`:

| Upstream class | Upstream message template |
|---|---|
| `NetworkNotFound` | `Network %(net_id)s could not be found.` |
| `SubnetNotFound` | `Subnet %(subnet_id)s could not be found.` |
| `PortNotFound` | `Port %(port_id)s could not be found.` |
| `NetworkInUse` | `Unable to complete operation on network %(net_id)s. %(reason)s.` |
| `SubnetInUse` | `Unable to complete operation on subnet %(subnet_id)s: %(reason)s.` |
| `PortInUse` | `Unable to complete operation on port %(port_id)s for network %(net_id)s. …` |
| `BadRequest` | `Bad %(resource)s request: %(msg)s.` |
| `NotAuthorized` | `Not authorized.` |
| `AdminRequired` | `User does not have admin privileges: %(reason)s.` |
| `NeutronException` | `An unknown exception occurred.` (base) |

Lightstack's 20 distinct `NewNotFoundError` resource strings map cleanly, because the resource is already
passed in: `"network"` → `NetworkNotFound`, `"subnet"` → `SubnetNotFound`, `"port"` → `PortNotFound`,
`"router"` → `RouterNotFound`, `"floating IP"` → `FloatingIPNotFound`, `"security group"` →
`SecurityGroupNotFound`, `"security group rule"` → `SecurityGroupRuleNotFound`, and so on for
`policy` (8 sites), `trunk` (5), `subnet pool` (3), `port forwarding` (3), `metering label` (3),
`address scope` (3), `RBAC policy` (2), `agent` (2).

Non-404 statuses map from the existing `Code` key, which already carries the semantic:
`badRequest` → `BadRequest`, `unauthorized` → `NotAuthorized`, `forbidden` → `NotAuthorized`,
`conflict`/`conflictingRequest` → the resource's `InUse` where one exists else `Conflict`,
`overLimit` → `OverQuota`, `badMethod` → `BadRequest`.

`computeFault` — Lightstack's internal-error `Code`, and a *compute* name that must never appear on a
network response — maps to the single documented generic **`NeutronException`**, matching upstream's
base class. This is where "failed to create bridge" and "failed to attach TAP to bridge" land: they
have no upstream counterpart, and per the clarify decision they get one honest generic rather than an
invented per-site name.

**Message text**: upstream capitalizes the resource and includes the id
(`Network <id> could not be found.`); Lightstack's `NewNotFoundError` produces `network could not be found.`
and `NewResourceNotFoundError` produces `network <id> could not be found.`. Prefer the id-carrying
form where an id is in hand. Not byte-exact fidelity — no client parses it, Horizon only displays it —
so this is a quality improvement, not a requirement, and it must not become a reason to touch call
sites.

**`detail`**: always present, usually empty, populated from the existing `Details` field when set.
The numeric `code` key must be absent (FR-006) — upstream has no such key.

---

## R3 — F10: two unrelated halves, and only one fix direction is coherent

**Decision**: put the port's TAP in the **root** namespace, beside the bridge. Stop passing
`inNamespace=true` for ports.

**Rationale**: everything else real is *already* in the root namespace, so the TAP is the lone
outlier rather than the thing that is right.

| Object | Namespace | Evidence |
|---|---|---|
| Network bridge | **root** | `network.go:479-480` — `CreateBridge(bridge, false, "")`, comment: "not project namespace, for libvirt access" |
| dnsmasq | **root** | `reconciler.go:279,281` — `RestartDHCP(cfg, "")` / `EnsureDHCP(cfg, "")`, empty `nsName` |
| Egress SNAT | **root** | `router.go` WS-3 comment: "masquerade … in the default namespace, where the VMs and their gateway bridge live" |
| Bridge address | **root** | `EnsureBridgeIP` uses netlink with no namespace |
| **Port TAP** | `light-ns-<project>` | `ports.go:89-90`, `:98`, `:100`, `:115`; delete at `:440`, `:471` |

The calls and line numbers in this table quote the code as it was diagnosed, before F10. F10 then
removed the namespace parameters altogether — `CreateBridge(name)`, `CreateTAPDevice(name)`,
`AttachToBridge(tap, bridge)`, `EnsureDHCP(cfg)` / `RestartDHCP(cfg)` — so a bridge, a TAP or a
DHCP server can no longer be placed outside the namespace Lightstack runs in.

And the namespace it names is never created on that path — the only `CreateNamespace` caller in the
repo is `auto_allocated_topology.go:113`. So real mode fails at `CreateTAPDevice` (or at the attach,
which is the message the ticket reports).

The reverse direction — move the bridge into `light-ns-<project>` — would break libvirt's access to
it, orphan dnsmasq and the SNAT rules, and require a veth pair per network to reach the root
namespace. It is rejected: A1 already states the reason, and this table is the evidence for it.

**Blast radius is small**: two functions (`CreatePort`, `DeletePort`) plus their three rollback
calls. `auto_allocated_topology.go`'s in-namespace bridge is a *separate*, self-consistent path
(creates the namespace, then the bridge in it) and is left alone.

**The stub half is a different bug.** `BridgeManager.stubBridges` is an in-process
`map[string]bool` (written by `createBridgeStub`, `netns.go:219`; read by the stub branch of
`AttachToBridge`, `:363-368`). It is not persisted and not rebuilt on startup, so a port on any
network created before the current process 500s `failed to attach TAP to bridge` — which is exactly
the reported symptom, and why the ticket's repro fails on a long-running compose stack
(`restart: unless-stopped`, `networking_mode: stub` from the baked `config/lightstack.yaml`).

**Decision for the stub half**: the `networks` table is the authority. In stub mode the attach must
not consult the in-process map at all — a recorded network is a valid attach target. Rejected
alternative: rebuild the map at startup from the database, which adds a reconcile path to keep in
sync for zero user-visible gain, in a mode whose entire purpose is to avoid host state.

---

## R4 — B6: three sub-defects, one of them with an in-repo pattern to copy

**Decision**: `*string` for the gateway on read, `*string` on the request to separate "omitted" from
"explicitly null", and log-and-continue on an unreadable row.

**Findings**:
1. **No filters at all.** `ListSubnets` (`network.go:1081`) parses only `limit`, `offset`, `marker`.
   The sibling `buildNetworkListFilter` (`:573`) is the pattern: pure function, returns
   `(conds, args, empty)`, numbers placeholders from `$1`, caller continues numbering. Reuse the
   shape — a `buildSubnetListFilter` — rather than inlining conditions, so it is unit-testable
   without a database (`network_test.go:95` already tests the network one that way).
2. **Gateway scanned into `string`.** Three sites: `ListSubnets:1149` (inside the `continue`),
   `GetSubnet:1218`, `UpdateSubnet:1379`. Column is `gateway_ip VARCHAR(50)` — nullable
   (`001_initial_schema.up.sql:91`). A NULL makes the first drop the row and the other two return
   500. Fix: `*string`, which marshals to `null`, exactly as the security-group-rule emitters
   already do (`ports.go:1081` and its comment).
3. **A NULL is currently unreachable through the API.** `CreateSubnet:1027-1030` substitutes
   `incrementIP(ipNet.IP, 1)` whenever `GatewayIP == ""`, and the request field is a plain `string`
   — so `--gateway none` (which sends `"gateway_ip": null`) is indistinguishable from omission and
   is silently overwritten. Fix: `*string` on the request. `nil` → auto-assign (today's behaviour);
   explicit `null` → store NULL. This is what makes (2) testable at all, per A5.
4. **Log-and-continue already exists in this package.** `ListSecurityGroupRules` does
   `log.Warn().Err(err).Msg("failed to scan security group rule row"); continue` (`ports.go:1087`)
   and checks `rows.Err()` afterwards. That is precisely FR-024's required behaviour — copy it, and
   include the row id in the warning.

**Also**: `ListSubnets` orders by `created_at DESC` alone and pages with `created_at < marker`,
which is not a total order — ties give unstable page boundaries. `ListNetworks` was already fixed to
`(created_at, id)` (`:684`, `:700`). FR-025 requires the same here; this is the same defect #549
found in the compute listings.

---

## R5 — B9: the pointers already exist; the handler throws them away

**Decision**: stop flattening. Carry the request pointers straight through to the INSERT and the
response.

**Rationale**: `CreateSecurityGroupRuleRequest` already declares `Protocol`, `PortRangeMin`,
`PortRangeMax`, `RemoteIPPrefix`, `RemoteGroupID` as pointers (`ports.go:945-949`) — so the wire
distinction is *received correctly* and then destroyed:

```go
portMin := 0                                    // :968  sentinel
if req.SecurityGroupRule.PortRangeMin != nil { portMin = *req.…PortRangeMin }
…
sql.NullInt32{Int32: int32(portMin), Valid: portMin > 0}   // :1024  0 becomes NULL
…
"port_range_min": portMin,                      // :1048  unset becomes 0
```

So the defect is one flatten-and-re-derive round trip, and it is **worse than the spec assumed**:
`Valid: portMin > 0` means a deliberate `port_range_min: 0` is *stored* as NULL. A6 asked whether
unset and zero are distinguishable — in the columns yes (`port_range_min INTEGER`, nullable), in
this write path **no**. FR-028 therefore requires a write-path change, not only a response change.
Same for `remoteIP`/`remoteGroup` via `Valid: remoteIP != ""`, which conflates unset with `""`.

Keeping the pointers fixes the response (FR-026/027), the storage (FR-028) and the
`networking.SecurityGroupRule` struct hand-off in one move. The iptables rule builder at `:995`
takes value types, so it keeps a local dereference with an explicit default — that is the one place
a sentinel is legitimate, because a kernel rule has no concept of "unset".

---

## R6 — F11: the flag, and a scoping collision the spec did not see

**Decision**: migration `066` adds `networks.external BOOLEAN NOT NULL DEFAULT false`; the list/show
predicate gains a third disjunct; `UpdateNetwork` gains the attribute **and** an owner-scope
exemption for administrators.

**Findings**:
1. **No column exists.** `networks` is created in `001:73` and extended by `004` (`network_type`,
   `physical_network`, `mtu`). Nothing about external. Next free migration number is `066`
   (`065_console_auth_tokens` is the highest). Down migration drops the column — reversible, per the
   constitution's Backwards Compatibility rule.
2. **The filter plumbing is already there.** `networkListDecision` (`:553`) returns
   `empty=true` for `router:external=true` and `ListNetworks` (`:636`) short-circuits to `[]`.
   `neutronBool` (`:540`) already distinguishes absent from explicit-false. So this is replacing an
   `empty` flag with a predicate — no new parsing, and `network_test.go:70,95` already unit-test
   both functions, so the RED step is a table entry.
3. **Three hardcoded emitters**: `network.go:517` (create), `:693` (list), `:766` (show). The list
   and show `SELECT`s must also gain the column — they currently select 8 fields and neither selects
   `project_id` either, which is B11's half of the same edit. **B11 and F11 modify the same three
   response builders and the same two queries** — sequence them, do not parallelize.
4. 🔑 **`UpdateNetwork` is owner-scoped and cannot mark another project's network.** Its statement is
   `UPDATE networks SET … WHERE id = $n AND project_id = $n+1` (`:880-881`), and its request struct
   accepts only `name` and `admin_state_up` (`:841-846`). FR-033 says an administrator marks a
   network external — but an administrator marking a *tenant's* network hits the owner predicate and
   silently updates zero rows. This needs the same admin override the rest of the codebase already
   uses (`common.HasRole` + `common.RoleAdmin`), or FR-033 is only satisfiable for networks the
   administrator happens to own. **Not in the spec; recorded here and reflected in the plan's tasks.**
5. **Consumers accept any network today.** `configureExternalGateway` (`router.go:596-620`) reads
   `external_gateway_info.network_id` and only looks up a subnet on it; `CreateFloatingIP`
   (`floatingip.go:168-178`) does the same and falls back to a hardcoded `192.0.2.0/24` when the
   network has no subnet. Neither checks ownership or externality. FR-034/035 add the check;
   FR-035a grandfathers what is already stored.
6. **Grandfathering has a concrete surface**: `reconcileNAT` (`reconciler.go:70`) re-asserts SNAT for
   every router with an external gateway on every poll. It must keep doing so regardless of the new
   flag (FR-035a) and log the non-external ones (FR-035b) — which makes the reconciler the natural
   place for the startup diagnostic, not a new component.

---

## R7 — B11: bind, authorize, and stop echoing the caller

**Decision**: add both spellings to the request struct; resolve the effective owner once; select and
report the stored owner in all three emitters.

**Findings**:
1. `CreateNetworkRequest.Network` (`:401-407`) declares `Name`, `AdminStateUp`, `Shared` — **no
   project field of either spelling**, so the value is discarded by `ShouldBindJSON` before any
   handler logic runs. That is why the ticket's `201` is silent: there is nothing to ignore, the
   field never existed.
2. All three emitters report the caller: `CreateNetwork:512-513`, `ListNetworks:686-687`,
   `GetNetwork:760-761`. The list and show queries **do not select `project_id`**, so this is a
   query change plus a response change, not a one-line swap. Both handlers can legitimately return a
   network owned by someone else (`shared = true`, and now `external = true`), so the caller is the
   wrong answer by construction.
3. The corrected shape is already in this file for subnets — `ListSubnets:1157-1159` and
   `GetSubnet:1202-1204` carry the comment "tenant_id from the row, not the caller" and select
   `s.project_id`. #545 fixed subnets and left networks. Copy it verbatim.
4. **The ticket's own B11 probe cannot pass on the bind fix alone**: `openstack network show
   b11-net -c project_id` reads through `GetNetwork`, which echoes the caller. Both halves land
   together or the acceptance check stays red for the wrong reason.
5. `GetNetwork` caches the built response under `network:<id>` for 30 minutes (`:774`). Once
   `project_id` is row-derived the cached value is caller-independent and therefore safe to share —
   but it must be verified, because caching a caller-derived field would leak one tenant's view to
   another.
6. Refusal status is `400` for all three rejection reasons (clarify decision, FR-014/015/016).
   `common.NewBadRequestError` already produces it; the message text comes from upstream (R2).

---

## R8 — Test strategy: where each defect's RED comes from

**Decision**: one RED source per defect (FR-038), preferring an existing committed check over a new one.

| Defect | RED source | New test needed? |
|---|---|---|
| B10 | `test/error_handling_test.sh:158` TEST 6 already fails (`Makefile:93`, in `test-all` at `:125`) | No — free, and it predates the branch |
| B9 | `TestRuleValuesAgreeAcrossAllThreeEmitters` (`security_group_shape_test.go:245`) — extend to the 4th emitter; the name is the evidence it was missed | Extend |
| B6 | New unit tests beside `network_db_test.go` (has a `TestMain` + `requireDB`) and a pure test for `buildSubnetListFilter` beside `TestBuildNetworkListFilter` | Yes |
| B11 | New unit test using the `X-Test-Project` / `X-Test-Roles` harness in `network_selectability_test.go:38` | Yes |
| F11 | Table entry in `TestNetworkListDecision` / `TestBuildNetworkListFilter` for RED; DB test for the flag; host check for egress | Yes |
| F10 | Unit test: port creation against a network absent from the in-process bridge map. Real half is host-only | Yes + host |

**`make test-all` is a realistic bar** (checked, not assumed): it composes
`test chart-test test-contract test-integration test-multi-tenant test-errors test-horizon-ui
test-performance` (`Makefile:125`). Every one runs against the docker-compose stack; none needs a
Kubernetes cluster. Only F10's and F11's real-mode halves need a privileged Linux host, and those are
host-runbook items, not CI gates.

**Baseline must be captured first**: `make test-all` is already red on B10, and the shared
`lightstack-postgres` container is used by ~12 worktrees, so a poisoned quota or project row produces
failures that have nothing to do with this branch. Record the pre-change state before touching code.

**Contract suites**: the neutron contract suite (`test/contract/neutron/`, 59 tests) passes today and
must keep passing. It keys off HTTP status rather than the error envelope, which is exactly why it
never caught B10 — so it is a regression guard here, not a source of RED.

---

## Summary of decisions

| # | Decision | Effect on scope |
|---|---|---|
| R1 | One choke point: render the envelope in `common.SendError`, marked once per **service engine** | **244 call sites → 0 changed.** Biggest reduction in the feature |
| R2 | `type` from `(status, Code, Resource)`; upstream names + `NeutronException` generic | One table, one place |
| R3 | TAP moves to the root namespace; stub attach stops consulting the in-process map | Two functions; direction is forced by where everything else already is |
| R4 | Pure filter builder, `*string` gateway on request and read, log-and-continue | Adds a total order fix (FR-025) |
| R5 | Keep the request pointers; do not flatten | Grew: the **write path** is wrong too, not just the response |
| R6 | Migration `066`, third visibility disjunct, admin override on update | Grew: `UpdateNetwork`'s owner scope blocks FR-033 |
| R7 | Add both spellings; select and report the stored owner | Two halves, must land together |
| R8 | Reuse committed checks for RED where they exist | B10 and B9 need no new RED |

**No new Go module. One migration (`066`). Two new files.**
