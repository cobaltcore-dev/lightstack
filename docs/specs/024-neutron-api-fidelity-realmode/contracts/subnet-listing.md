# Contract: subnet listing and gateway state (US4 / B6)

**Covers**: FR-019, FR-020, FR-020a, FR-021, FR-022, FR-023, FR-024, FR-024a, FR-025
**Applies to**: `GET /v2.0/subnets`, `GET/PUT /v2.0/subnets/{id}`, `POST /v2.0/subnets`

## Filters

Seven, mirroring what `buildNetworkListFilter` already supports for networks (FR-020). Anything else
is **not advertised** and must leave the result unchanged (FR-020a).

| Query parameter | Predicate |
|---|---|
| `network_id` | `s.network_id = $n` |
| `id` | `s.id::text = $n` |
| `name` | `s.name = $n` |
| `cidr` | `s.cidr = $n` |
| `ip_version` | `s.ip_version = $n` |
| `enable_dhcp` | `s.enable_dhcp = $n` (parsed with the existing `neutronBool`) |
| `tenant_id` / `project_id` | `s.project_id = $n` |

Built by a **pure** `buildSubnetListFilter(...) (conds []string, args []any)` returning placeholders
numbered from `$1`, so it is unit-testable with no database — the shape of `buildNetworkListFilter`
(`network.go:573`), which `TestBuildNetworkListFilter` (`network_test.go:95`) already tests that way.

Filters compose with the existing visibility scope, which is unchanged:
`(s.project_id = caller OR n.project_id = caller OR n.shared = true)`.

## Contract tests

| ID | Given | When | Then |
|---|---|---|---|
| **S1** | networks A and B, one subnet each | `GET /v2.0/subnets?network_id=<A>` | only A's subnet — **fails today**, returns both |
| **S2** | as above | `openstack subnet list --network <A>` | same, via the CLI |
| **S3** | subnets differing in name / cidr / ip_version / enable_dhcp | filter by each in turn | only the matching rows |
| **S4** | any subnets | `GET /v2.0/subnets?bogus_filter=x` | identical to the unfiltered result — an unknown parameter never narrows |
| **S5** | a subnet on the caller's network but owned by another project | list and filter by that network | still visible (preserves #545 FR-013a) and reported with **its own** owner |
| **S6** | more subnets than one page, several sharing a `created_at` | walk pages with `marker` | every subnet appears exactly once — no repeats, no omissions |
| **S7** | a filter plus paging | `?network_id=<A>&limit=1` | the filter applies to the whole set, not just the page |

## Gateway tri-state

`CreateSubnetRequest.Subnet.GatewayIP` becomes `*string` so the three cases are distinguishable:

| Request | Meaning | Stored | Reported |
|---|---|---|---|
| omitted | choose one | first host address (unchanged) | that address |
| `"gateway_ip": null` | none | `NULL` | `null` |
| `"gateway_ip": "10.0.0.1"` | explicit | `10.0.0.1` | `10.0.0.1` |

| ID | Given | When | Then |
|---|---|---|---|
| **S8** | a `/24` and no `gateway_ip` key | `POST /v2.0/subnets` | gateway is the first host address — today's behaviour, unchanged |
| **S9** | `gateway_ip: null` | `POST /v2.0/subnets` | `201`, gateway reported `null`, stored `NULL` — **fails today**, an address is invented |
| **S10** | `openstack subnet create --gateway none …` | CLI | as S9 |
| **S11** | a gatewayless subnet | `GET /v2.0/subnets` | it **appears**, `gateway_ip` is `null` — today the row silently vanishes (`network.go:1149` `continue`) |
| **S12** | a gatewayless subnet | `GET /v2.0/subnets/{id}` | `200`, `gateway_ip: null` — today `500` |
| **S13** | a gatewayless subnet | `PUT /v2.0/subnets/{id}` renaming it | `200` — today `500` |
| **S14** | a subnet with a gateway | `PUT` with `gateway_ip: null` | gateway removed, persisted, reported `null` |

Read sites to convert to `*string`: `ListSubnets:1149`, `GetSubnet:1218`, `UpdateSubnet:1379`.

## Unreadable rows

| ID | Given | When | Then |
|---|---|---|---|
| **S15** | a subnet row that cannot be scanned | `GET /v2.0/subnets` | `200`; the other subnets are returned; a **warning naming that row's id** is logged; the request does **not** fail |
| **S16** | as above | inspect the response | no non-upstream "partial result" field was added (FR-024a) |

The pattern already exists in this package — `ListSecurityGroupRules` does
`log.Warn().Err(err).Msg("failed to scan security group rule row"); continue` and checks `rows.Err()`
afterwards (`ports.go:1087`). Copy it and add the row id.

**Why not fail the request**: one corrupt row would blank every caller's subnet list, including the
dashboard's — a worse outage than the silent drop it replaces (clarify decision).

## Ordering

`ORDER BY created_at DESC` alone is **not** a total order; ties make page boundaries unstable. Use
`(created_at, id)` and an exclusive marker, as `ListNetworks` already does (`network.go:684`, `:700`).
This is the same defect #549 found in the compute listings, where it caused rows to repeat across
pages — S6 is the guard.
