# Contract: Subnet creation entitlement (B8) 🔒

**Feature**: `022-project-tenant-isolation` | **Requirements**: FR-009 – FR-013
**Service**: Network (Neutron) v2.0, port 9696 | **Endpoint**: `POST /v2.0/subnets`

This is the parent issue's only security defect. Today `CreateSubnet` takes `network_id` from the
request body, `project_id` from the token, and never asks whether they are consistent — so any tenant
can define the address range, gateway and DHCP behaviour on any other tenant's network, and the
victim's own network and subnet views then present the result as theirs.

## The check

Inserted before the `INSERT` and before any DHCP work:

```
1. uuid.Validate(network_id)          → invalid  ⇒ 400
2. SELECT project_id, shared FROM networks WHERE id = $1
                                       → no row  ⇒ 404
3. project_id == caller's project      → ALLOW
4. caller holds role "admin"           → ALLOW
5. shared == true                      → 403
6. otherwise                           → 404
```

One query answers all three outcomes. This is why the check selects the two columns rather than
filtering on them the way `CreatePort` does — a `WHERE … AND (project_id = $2 OR shared = true)`
predicate folds the decision into the query and cannot separate `403` from `404`.

## Entitlement matrix

Caller **B** (project `P_B`, role `member` unless stated), target network owned by **A** (project `P_A`).

| # | Target network | Caller | Result | Status | Subnet created? | DHCP touched? | Audit record? |
|---|---|---|---|---|---|---|---|
| 1 | owned by `P_B` | B | allow | `201` | yes | yes, if `enable_dhcp` | no |
| 2 | owned by `P_A`, `shared = false` | B | refuse | **`404`** | no | no | **yes** |
| 3 | owned by `P_A`, `shared = true` | B | refuse | **`403`** | no | no | **yes** |
| 4 | owned by `P_A` (either flag) | admin | allow | `201` | yes | yes, if `enable_dhcp` | no |
| 5 | owned by `P_B` | admin | allow | `201` | yes | yes | no |
| 6 | well-formed id, no such network | anyone | refuse | **`404`** | no | no | no — nothing was targeted |
| 7 | malformed id (not an identifier) | anyone | refuse | **`400`** | no | no | no |
| 8 | `network_id` absent from the body | anyone | refuse | `400` | no | no | no — existing `binding:"required"` |

**Rows 2 and 3 are the fix.** Both succeed with `201` today.

### Why `403` for shared and `404` for private

A private network the caller cannot see must not be confirmed to exist — `404` discloses nothing. A
**shared** network is already visible to that caller through the network list
(`buildNetworkListFilter` returns `project_id = caller OR shared = true`), so answering `404` would be a
lie the caller can immediately detect by listing networks. `403` tells the truth: you can see it, you
may not define its address ranges. This was clarification Q1; the "always 404" variant was rejected for
exactly this reason.

### Why this differs from `CreatePort` — and why that must be written down

| Handler | Rule | Upstream policy |
|---|---|---|
| `CreatePort` (`ports.go:58`) | own network **or any shared** network | `create_port` — shared networks are usable |
| `CreateSubnet` (this contract) | own network **only**, or administrator | `create_subnet: rule:admin_or_network_owner` — shared excluded |

The divergence is upstream's, not O3K's invention: sharing a network lets other tenants *attach to* it,
never *define* it. But the two handlers sit in the same package, read almost identically, and now decide
differently — which is the exact shape that gets "unified" back into a defect during a later cleanup.
**FR-010/A5 require the reason to be stated in the code at the point of decision**, and the matrix test
fails if row 3 ever starts returning `201`.

## Ordering guarantee

The refusal happens before **every** side effect:

| Side effect | Where | Must not run on refusal |
|---|---|---|
| `INSERT INTO subnets` | `network.go:943` | ✅ |
| `applyDHCPForNetwork(ctx, network_id, true)` | `network.go:958` | ✅ |

`applyDHCPForNetwork` returns immediately in stub mode (`reconciler.go:232`), so in stub the ordering is
unobservable — it matters only in real mode, where it would otherwise start dnsmasq and assign a bridge
gateway address on the victim's network. That is why the ordering is a stated requirement rather than
something a stub-mode test could catch.

## Audit record (FR-011b)

A refused cross-tenant write emits exactly one structured warning. A legitimate creation emits none.

| Field | Value |
|---|---|
| level | `warn` |
| message | identifies this as a refused cross-tenant subnet write |
| calling project | `project_id` from the token |
| target network | the requested `network_id` |
| network owner | `project_id` read from the `networks` row |

Emitted for matrix rows **2 and 3 only**. Rows 6–8 emit nothing: no existing network was targeted, so
there is no cross-tenant attempt to record — only a malformed or unlucky request. Rows 1, 4 and 5 emit
nothing: they are ordinary successful operations.

Uses the package's existing `zerolog` logger, the same one `CreateSubnet` already uses for database
errors (`network.go:949`).

## Response bodies

Refusals use O3K's existing shared error envelope, not upstream's `NeutronError` wrapper — no O3K
Neutron error uses that wrapper today, and introducing it for one new status would make the service's
error surface internally inconsistent (research R7).

```
403 → {"forbidden":   {"message": "…", "code": 403}}
404 → {"itemNotFound":{"message": "network could not be found.", "code": 404}}
400 → {"badRequest":  {"message": "…", "code": 400}}
```

**The success response is unchanged in every field** — same eleven persisted columns, same
`allocation_pools`, same `dns_nameservers` empty-slice defaulting, same timestamps (FR-013).

## Scope boundary — verified, not assumed

Adjacent paths were checked so the fix is not scoped wider than the defect:

| Path | Finding |
|---|---|
| `UpdateSubnet` (`network.go:1296`) | **changed** — same predicate as `DeleteSubnet`, since upstream gives both rules the same check_str; still accepts only `name`, so a subnet cannot be moved onto another tenant's network (FR-013a) |
| `GetSubnet` (`network.go:1189`) | **changed** — was `(s.project_id = $2 OR n.shared)`, now also `OR n.project_id = $2` (FR-013a) |
| `DeleteSubnet` (`network.go:1234`) | **changed** — was `AND project_id = $2`, now joins `networks` and matches `(s.project_id = $2 OR n.project_id = $2)`, no `shared` disjunct (FR-013a) |
| bulk subnet create | **does not exist** — `CreateSubnetRequest` (`network.go:895`) is a single object |
| `CreatePort` | already checks the target network; rule differs by design (above) |

The *write* defect is confined to `CreateSubnet`. Its **aftermath** is not, and that was missed on the
first pass: row 5 of the entitlement matrix — an administrator adding a subnet to another tenant's
network — leaves a subnet whose project differs from its network's, because `CreateSubnet` attributes
the row to the caller (matching upstream's `populate_project_id`, which copies `context.project_id`).
The network's owner then saw it in `ListSubnets`, got `404` fetching it by id, and could not delete
it. Upstream grants the owner all three — `get_subnet` carries `ADMIN_OR_NET_OWNER_READER`, while
`update_subnet` and `delete_subnet` carry `ADMIN_OR_NET_OWNER_MEMBER`, `network_owner` being
`project_id:%(network:project_id)s` — so O3K was stricter than upstream on exactly those three paths
while its own `ListSubnets` already applied the rule. FR-013a closes that; `shared` stays out of the
two mutating paths because upstream keeps it out.

Two consequences to keep in view:

- **The write must not re-filter.** `DeleteSubnet`'s `DELETE` and `UpdateSubnet`'s `UPDATE` no longer
  carry `project_id`; they target the id the entitlement query already resolved. Keeping both made a
  network owner's call a silent no-op — `204`/`200` with the row unchanged, because the two predicates
  disagreed. One decision, one place. This was found by a test, not by review.
- **`tenant_id` comes from the row.** `UpdateSubnet`, `GetSubnet` and `ListSubnets` all reported the
  caller's project — correct only while each could return nothing but the caller's own rows. Two
  changes broke that assumption: SPEC-020's `n.project_id = $1` on the listing, and FR-013a on the
  other two. All three now select `s.project_id`, so a network owner reading or renaming another
  project's subnet is not told the resource is theirs. `CreateSubnet` still reports the caller, which
  is correct there — it is the project it just wrote into the row.

  The listing and by-id read were found by the **host pass**, after three unit tests covering FR-013a
  had gone green: the rename test asserts on the `PUT` response, which was already fixed, so nothing
  in the suite ever read `tenant_id` back through a `GET`. `TestForeignSubnetReportsItsOwnTenantID`
  now covers both read paths.

The broader Nova owner-or-administrator sweep and the administrator bypass on these subnet paths are
#549.

## Test obligations

| Case | Level |
|---|---|
| All eight matrix rows | unit (`internal/neutron/subnet_ownership_test.go`), DB-gated |
| No subnet row exists after rows 2, 3, 6, 7 | unit — assert on the `subnets` table |
| Victim's `GET /v2.0/networks/{id}` and `GET /v2.0/subnets` are unchanged after a refusal | unit + integration |
| Exactly one warning for rows 2–3, none for rows 1, 4–8 | unit — capture the log writer |
| First `common.HasRole` use in `internal/neutron` behaves like the existing users | covered by rows 4–5 |
| Network owner may fetch **and** delete a subnet owned by another project on their network (FR-013a) | unit — `TestNetworkOwnerMaySeeAndRemoveForeignSubnet` |
| Network owner may **rename** one; the new name is actually stored, and `tenant_id` names the owner not the caller | unit — `TestNetworkOwnerMayRenameForeignSubnet` |
| `tenant_id` names the subnet's own project on the by-id read **and** in the listing, and still does for a caller who owns it | unit — `TestForeignSubnetReportsItsOwnTenantID` |
| A caller owning neither the subnet nor its network still gets `404` on fetch, rename and delete, and the row is unchanged | unit — both tests |
| A **shared** network grants read but not delete (FR-013a) | unit — `TestSharedNetworkGrantsReadNotDelete` |
| Live cross-tenant refusal with two real tenants | `test/multi_tenant_test.sh` (FR-018) |
| Static: the handler now consults `networks` | `awk '/func \(svc \*Service\) CreateSubnet/,/^}/' internal/neutron/network.go \| grep -c 'FROM networks'` → non-zero |
