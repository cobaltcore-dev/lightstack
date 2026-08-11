# Phase 1 Data Model: Neutron API Fidelity & Real-Mode Networking

**Feature**: `024-neutron-api-fidelity-realmode` | **Date**: 2026-08-11

One migration, one new column. Everything else is a change to how existing columns are read, written
and reported — which is why five of the six defects need no schema work at all.

---

## Schema changes

### Migration `066_neutron_external_networks`

Next free number: `065_console_auth_tokens` is the highest present.

```sql
-- 066_neutron_external_networks.up.sql
ALTER TABLE networks ADD COLUMN IF NOT EXISTS external BOOLEAN NOT NULL DEFAULT false;

-- 066_neutron_external_networks.down.sql
ALTER TABLE networks DROP COLUMN IF EXISTS external;
```

| Property | Value | Why |
|---|---|---|
| Nullability | `NOT NULL DEFAULT false` | A network is external or it is not; there is no third state. A nullable column would add a "don't know" that no requirement models. |
| Default | `false` | Existing networks are not external. This is what makes FR-035a (grandfathering) necessary rather than optional — see below. |
| Index | none | Only ever used as a predicate alongside `project_id`/`shared` on a table with tens of rows in any realistic deployment. An index here would be speculative. |
| Reversible | yes | Constitution *Backwards Compatibility*: migrations must be up/down. Dropping the column restores prior behaviour exactly, because nothing else reads it. |

**The default is load-bearing.** Every network that exists at upgrade time becomes `external = false`.
Any router gateway or floating IP already configured against one of those networks would fail the new
FR-034/FR-035 checks. FR-035a resolves this by validating **new requests only** and leaving stored
rows — including the reconciler's periodic re-assertion — untouched. The migration deliberately does
*not* back-fill `external = true` for networks currently used as a gateway: that would grant an
administrator-only, tenant-visible attribute to networks no operator designated.

### Not changed

| Table | Why it was a candidate, and why it stays as-is |
|---|---|
| `subnets` | `gateway_ip VARCHAR(50)` is already nullable (`001:91`). B6 is a read/write **code** defect, not a schema gap — the column has always been able to hold the state the API refuses to produce. |
| `security_group_rules` | `port_range_min/max INTEGER`, `remote_ip_prefix`, `remote_group_id`, `protocol` are all already nullable (`001:129-133`). B9's conflation of unset with zero happens in the handler, not the column. |
| `ports` | F10 is a namespace-placement defect. No stored attribute changes. |
| `networks.project_id` | Already `UUID REFERENCES projects(id)`. B11 changes which value is written, not the column. The FK is what makes FR-016 (refuse an absent project) necessary — without the check, the insert fails at the storage layer and surfaces as a 500. |

---

## Entities

### Network

| Attribute | Storage | Change in this feature |
|---|---|---|
| `id` | `uuid` PK | — |
| `name` | `varchar(255)` | — |
| `project_id` | `uuid` FK → `projects` | **B11**: now settable on create by an administrator; must be reported from the row rather than from the caller |
| `shared` | `boolean` | — (the unguarded write is explicitly out of scope) |
| `external` | `boolean` **NEW** | **F11**: settable on create and update by an administrator; reported by all three emitters; a third independent visibility disjunct |
| `admin_state_up`, `status`, `network_type`, `physical_network`, `mtu` | — | — |

**Visibility rule** (FR-033a). A network is visible to a caller when **any** of:

```
project_id = caller  OR  shared = true  OR  external = true
```

The three are independent. `external` does **not** imply `shared` and must not require it — that was
the clarify finding: requiring both leaves the tenant-facing gateway selection empty, which is the
defect F11 reports. Applies to `ListNetworks` and `GetNetwork`.

**Authority rule** (FR-033b). Visibility grants reading only. Being able to see a network — whether
via `shared` or via `external` — never grants authority over it:

| Operation | Owner | Administrator | Other project seeing it via `shared`/`external` |
|---|---|---|---|
| Show / list | yes | yes | yes |
| Attach a subnet | yes | yes | **no** (`callerMaySubnetNetwork`, unchanged) |
| Rename / delete | yes | yes | **no** |
| Set `external` | **no** | yes | **no** |
| Use as router gateway / FIP pool | — | yes | yes, if `external` |

Setting `external` is administrator-only even for the owner (FR-033), because it is a
deployment-level designation rather than a tenant attribute — matching upstream's
`create_network:router:external` → `ADMIN`.

⚠️ **`UpdateNetwork` cannot express this today.** Its statement is
`UPDATE networks SET … WHERE id = $n AND project_id = $n+1` (`network.go:880-881`), so an
administrator marking a *tenant's* network external updates zero rows and reports success. The
owner-scope predicate needs the standard administrator exemption (`common.HasRole` +
`common.RoleAdmin`) or FR-033 is only satisfiable for networks the administrator happens to own.
Discovered during planning (research R6.4); not present in the spec.

**State transitions**: `external` is a free toggle with one asymmetry — turning it **on** is
unconstrained; turning it **off** (or deleting the network) leaves dependent routers and floating IPs
in place and working, per FR-035a, with the condition logged per FR-035b. Nothing is cascaded or
withdrawn.

### Subnet

| Attribute | Storage | Change in this feature |
|---|---|---|
| `gateway_ip` | `varchar(50)` **nullable** | **B6**: NULL becomes both *producible* (an explicit request for none is honoured) and *readable* (scanned as a pointer, reported as `null`) |
| `network_id`, `cidr`, `ip_version`, `enable_dhcp`, `dns_nameservers`, `project_id` | — | **B6**: all become filterable on the listing |

**Gateway tri-state on the request** — the distinction the current plain `string` cannot express:

| Request body | Meaning | Stored | Reported |
|---|---|---|---|
| attribute omitted | "choose one for me" | first host address (today's behaviour, unchanged) | that address |
| `"gateway_ip": null` | "no gateway" | `NULL` | `null` |
| `"gateway_ip": "10.0.0.1"` | explicit | `10.0.0.1` | `10.0.0.1` |

Requires `*string` on `CreateSubnetRequest`. Today the field is `string`, so `null` and omitted are
identical and both get an invented address (`network.go:1027-1030`) — which is why a NULL gateway is
currently unreachable through the API, and why the read-side defect is only reachable by writing the
database directly (spec A5).

**Gatewayless behaviour** (FR-023a): no default route is advertised, the metadata route still is.
See `contracts/port-and-dhcp.md`.

### Security-group rule

| Attribute | Storage | Change in this feature |
|---|---|---|
| `protocol` | `varchar(10)` nullable | — (already correct on all four emitters) |
| `port_range_min`, `port_range_max` | `integer` nullable | **B9**: unset must store NULL *and* report `null`; a deliberate `0` must store `0` and report `0` |
| `remote_ip_prefix` | `varchar(50)` nullable | **B9**: unset must report `null`, not `""` |
| `remote_group_id` | `uuid` nullable FK | **B9**: as above |

**Three-valued, and today's write path collapses it to two.** The request struct already carries
pointers (`ports.go:945-949`), so the wire distinction arrives intact and is then destroyed:

| Wire value | Should store | Stores today | Should report | Reports today |
|---|---|---|---|---|
| omitted / `null` | `NULL` | `NULL` | `null` | `0` / `""` |
| `0` | `0` | **`NULL`** ← wrong | `0` | `0` |
| `443` | `443` | `443` | `443` | `443` |

The `0` row is the one the spec's A6 flagged as uncertain: the columns can hold it, the handler
cannot. `Valid: portMin > 0` (`ports.go:1024`) discards a legitimate zero — a valid ICMP type — so
FR-028 requires a write-path fix, not only a response fix.

### Port

No stored attribute changes. F10 is about where the port's device is placed:

| Object | Namespace today | Namespace after |
|---|---|---|
| Network bridge | root | root (unchanged) |
| dnsmasq | root | root (unchanged) |
| Egress SNAT | root | root (unchanged) |
| **Port TAP** | `light-ns-<project>` — never created | **root** |

The TAP is the only object not already in the root namespace, which is what forces the direction
(research R3). Per-network L2 isolation continues to come from one bridge per network, not from
namespaces, so nothing is weakened by the move.

### Error response *(not persisted — a response contract)*

Two shapes, selected by which service is answering:

| Field | Compute / block-storage / identity (unchanged) | Network (this feature) |
|---|---|---|
| wrapper key | `itemNotFound`, `badRequest`, `computeFault`, … | `NeutronError` |
| `message` | present | present |
| `code` | numeric status | **absent** |
| `details` | when set | renamed `detail`, always present, often empty |
| `type` | — | **new** — the condition name |

The internal `OpenStackError` gains one additive field carrying the resource name, because `type` is
derived from it and `NewNotFoundError` currently formats it into the message and discards it. No
existing field changes meaning, so no other service is affected.

---

## Invariants

- **I1**: A network's reported owning project always equals its stored `project_id`. No response
  reports the caller's project for a resource the caller may not own. (FR-017; #545 established this
  for subnets, this feature extends it to networks.)
- **I2**: Visibility never implies authority. Seeing a network via `shared` or `external` grants no
  write of any kind. (FR-033b; preserves #545's `callerMaySubnetNetwork` rule.)
- **I3**: `external = false` on an existing row never withdraws working egress. Validation applies to
  new requests; stored state is grandfathered and logged. (FR-035a/b.)
- **I4**: An absent gateway and a present gateway are distinguishable at every layer — request,
  storage, response — and an absent gateway never suppresses the metadata route. (FR-020a/023/023a.)
- **I5**: Unset and zero are distinguishable at every layer for security-group rule ports. (FR-028.)
- **I6**: A row that cannot be read is logged and skipped, never silently dropped and never fatal to
  the whole listing. (FR-024/024a.)
- **I7**: Every error from the network service carries the network envelope; no other service's
  envelope changes. (FR-006/010.)
- **I8**: List pagination is stable — ordering is total (`created_at, id`), so page boundaries do not
  shift between requests. (FR-025.)
