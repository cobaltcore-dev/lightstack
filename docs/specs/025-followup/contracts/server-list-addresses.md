# Contract: server-list addresses (US7, FR-045 – FR-048)

**Surface**: `GET /v2.1/servers/detail`
**Today**: one address query per server in the page.

## Change

One query for the whole page, keyed by instance id. Round trips do not grow with the server count.

## Invariants

1. **Identical output.** Each server's address map is exactly what the per-server view reports,
   floating IPs included — the `floating_ips` aggregation from B17 is preserved unchanged.
2. **Correct attribution.** A port contributes an address only to the server it belongs to, and only
   when its project matches that server's. A cross-tenant administrative listing stays correctly
   attributed.
3. **Empty, not absent.** A server with no ports reports an empty address map, not `null` and not a
   missing key.
4. **Bounded round trips.** The number of address queries is independent of the number of servers.

## Evidence required

Equivalence with the per-server form asserted, and the round-trip count asserted rather than assumed
(FR-048).
