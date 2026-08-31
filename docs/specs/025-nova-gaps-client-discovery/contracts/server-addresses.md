# Contract: floating IPs in the address map (US6 / B17)

**Covers**: FR-034 – FR-039
**Applies to**: `GET /v2.1/servers/{id}`, `GET /v2.1/servers/detail`

## Now

`getInstanceAddresses` (`nova/handlers.go:1152`) selects `p.network_id, p.fixed_ips, n.name` (`:1158`) and
hardcodes `"OS-EXT-IPS:type": "fixed"` (`:1191`). Only one address kind can ever appear.

```jsonc
{"rt-net": [{"OS-EXT-IPS:type": "fixed", "addr": "10.244.10.77", "version": 4}]}
```

## After

Both kinds, same per-network list, each labelled from the address rather than a constant — upstream's
shape (FR-034, FR-035).

```jsonc
{"rt-net": [
  {"OS-EXT-IPS:type": "fixed",    "addr": "10.244.10.77", "version": 4},
  {"OS-EXT-IPS:type": "floating", "addr": "192.0.2.100",  "version": 4}
]}
```

Join `floating_ips` on `port_id`; the columns and the index are already there
(`migrations/003_add_routers.up.sql:28-40,58`).

## Care point

`getInstanceAddresses` takes the instance's **owning** project on purpose — its own doc comment explains
that passing the caller's project returns an empty map for an administrator viewing another tenant's
server. **The new join must widen with its parent** or FR-039 fails in exactly the way the parent query
already documents.

## Rules

| Case | Expected |
|---|---|
| FIP associated to the server's port | appears, tagged `floating` (FR-034) |
| no FIP | address map identical to today (FR-037) |
| FIP allocated but not associated | appears on no server (FR-038) |
| two FIPs on one port | both appear |
| server on several networks, FIP on one | only that network's list gains an entry |
| administrator reading another project's server | sees it too (FR-039) |
| single read vs listing | identical addresses (FR-036) |

## Test obligations

- Unit on the address builder with and without a floating row.
- Real host (router + external network + associated FIP): `server show`, `server list` and the dashboard's
  instance list all report both addresses.
- Out of scope, noted: collapsing the per-instance query in a listing is a pre-existing N+1 concern.
