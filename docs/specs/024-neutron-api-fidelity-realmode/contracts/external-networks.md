# Contract: external networks (US6 / F11)

**Covers**: FR-030, FR-031, FR-032, FR-033, FR-033a, FR-033b, FR-034, FR-035, FR-035a, FR-035b, FR-036, FR-037
**Applies to**: `POST/GET/PUT /v2.0/networks`, `PUT /v2.0/routers/{id}` gateway, `POST /v2.0/floatingips`

The only story in this feature that adds a concept, and the only one with a migration
(`066`, `networks.external BOOLEAN NOT NULL DEFAULT false`).

## Attribute

Wire name `router:external` (the colon is part of the upstream attribute name), stored as
`networks.external`.

| Operation | Who | Note |
|---|---|---|
| Set on create | **administrator only** | upstream `create_network:router:external` → `ADMIN` |
| Set on update | **administrator only** | see the blocker below |
| Read | any authenticated caller | |
| Filter | any authenticated caller | |

## Visibility — the clarify decision

A network is visible when **any** of the three holds, independently:

```
project_id = caller  OR  shared = true  OR  external = true
```

`external` must **not** require `shared` (FR-033a). Without this third disjunct an admin-owned
external network never reaches a tenant, so the Set Gateway dropdown populates for administrators and
stays empty for everyone else — which is the defect F11 reports.

Visibility is **read-only** (FR-033b). It confers nothing: a non-owner cannot attach a subnet, rename,
delete, or change external status. #545's `callerMaySubnetNetwork` rule — that sharing a network never
grants authority over its address ranges — applies unchanged.

## Filter

`networkListDecision` (`network.go:553`) currently returns `empty=true` for `router:external=true` and
`ListNetworks` (`:636`) short-circuits to `[]`. Replace the flag with a predicate:

| Query | Result |
|---|---|
| `?router:external=true` | only external networks |
| `?router:external=false` | only non-external networks |
| absent | no external constraint |

The two must be **complements**, never one of them always empty. `neutronBool` (`:540`) already
distinguishes absent from explicit-false, so no new parsing is needed.

## Hardcoded emitters to replace

`network.go:517` (create), `:693` (list), `:766` (show) all emit `"router:external": false` literally.
The list and show `SELECT`s must gain the column — the same two queries `network-ownership.md` changes
for `project_id`. **Sequence the two contracts; do not parallelize them.**

## ⚠️ Blocker found in planning: `UpdateNetwork` is owner-scoped

```go
// network.go:880-881
"UPDATE networks SET %s WHERE id = $%d AND project_id = $%d"
```

Its request struct accepts only `name` and `admin_state_up` (`:841-846`). So marking a network
external needs two changes, not one:

1. accept `router:external` in the update body, and
2. exempt an administrator from the owner predicate — otherwise an administrator marking a *tenant's*
   network updates **zero rows and reports success**, which is the same dishonest-success defect as
   B11.

Use the existing `common.HasRole` + `common.RoleAdmin`. Not in the spec; see research R6.4.

## Consumer validation

Both consumers accept **any** network as external today:

| Consumer | Location | Today |
|---|---|---|
| Router gateway | `router.go:596-620` `configureExternalGateway` | reads `external_gateway_info.network_id`, looks up a subnet, no externality or ownership check |
| Floating IP | `floatingip.go:168-178` `CreateFloatingIP` | same, plus falls back to a hardcoded `192.0.2.0/24` when the network has no subnet |

## Grandfathering (FR-035a/b)

The migration defaults `external` to `false`, so **every** pre-existing router gateway would fail the
new check. Validation therefore applies to **new create/update requests only**.

- Stored gateways and floating IPs keep working.
- `reconcileNAT` (`reconciler.go:70`) keeps re-asserting SNAT for them on every poll — it must **not**
  learn the new check, or an upgrade silently withdraws working egress.
- Nothing back-fills `external = true` to make old state pass: that would grant an
  administrator-only, tenant-visible attribute to networks no operator designated.
- The reconciler logs any router whose gateway network is not external, so an operator can converge
  deliberately. The reconciler is the natural home for this — it already walks exactly that set.

## Contract tests

| ID | Given | When | Then |
|---|---|---|---|
| **X1** | administrator | `POST /v2.0/networks` with `router:external: true` | `201`, reported external; **stored** external |
| **X2** | non-administrator | same | refused |
| **X3** | non-administrator | `PUT /v2.0/networks/{own}` with `router:external: true` | refused |
| **X4** | administrator, a **tenant's** network | `PUT` with `router:external: true` | applied — not "success with zero rows updated" (the blocker above) |
| **X5** | an external network | restart the service, `GET` it | still external — persisted, not in-memory |
| **X6** | one external, one not | `?router:external=true`, then `=false` | disjoint, complementary, neither empty |
| **X7** | external network owned by A, **not shared** | tenant B lists networks | B sees it, reported with **A** as owner |
| **X8** | as X7 | B `POST /v2.0/subnets` onto it | refused — visibility grants no authority (FR-033b) |
| **X9** | as X7 | B renames / deletes / unmarks it | refused |
| **X10** | a non-external network | `PUT /v2.0/routers/{id}` setting it as gateway | refused — today silently accepted |
| **X11** | a non-external network | `POST /v2.0/floatingips` against it | refused |
| **X12** | an external network | both of the above | succeed |
| **X13** | a router gateway configured **before** the migration | restart and reconcile | egress still works; the condition is logged |
| **X14** | an external network in use as a gateway | unmark it | the existing router keeps working; a **new** gateway on it is refused |
| **X15** | two external networks | list and set a gateway | nothing assumes there is exactly one |
| **X16** | an external network exists | open Horizon's router *Set Gateway* dialog | the network is offered (`test/horizon_compat_test.sh`) |
| **X17** | real mode, router with an external gateway | guest sends outbound traffic; a FIP is allocated | egress reaches the outside; the FIP reaches the guest |
| **X18** | the migration | `migrate up` then `migrate down` | both succeed; down leaves the prior behaviour exactly |
