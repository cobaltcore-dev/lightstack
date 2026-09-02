# Contract: compute microversion negotiation (US6, FR-038 – FR-044)

**Surface**: every compute response, via `OpenStack-API-Version` /
`X-OpenStack-Nova-API-Version`
**Today**: 2.1–2.90 is advertised; the request header is read at exactly one decision point, and
comments elsewhere describe gates that do not exist.

## Negotiation

| Request | Applied version |
|---|---|
| `OpenStack-API-Version: compute 2.21` | 2.21 |
| `X-OpenStack-Nova-API-Version: 2.21` | 2.21 (the other header wins if both are present) |
| `latest` | The advertised maximum |
| **No header** | The advertised **maximum** — see the divergence below |
| Malformed (`compute banana`) | Treated as no negotiation; the request succeeds |
| Above the advertised maximum | **406 Not Acceptable** |

Every response reports the version actually applied.

## Two deliberate divergences from upstream

1. **No header resolves to the maximum, not the minimum.** Upstream assumes the *minimum* supported
   microversion when no header is sent. Lightstack uses the maximum, and FR-040 requires it to keep doing so
   — adopting upstream's default would remove behaviour from every existing caller in one release.
   **Consequence**: the gates below only ever affect a client that negotiates *explicitly*. A stock
   client sending no header keeps receiving the newest shape of every response.
2. **A malformed header is tolerated**, where upstream answers `400`. Decided in SPEC-025 and
   recorded in the code as A-006: the gate exists to avoid granting more than a client negotiated,
   and that holds without inventing a rejection.

## Gating

Bounded to behaviours the product **emits** (FR-042). A behaviour upstream gates but the product does
not implement acquires no gate, and the advertised range is not narrowed to match the audit.

```
GET /v2.1/servers/{id}   OpenStack-API-Version: compute 2.1
→ a response with no field upstream introduced after 2.1

GET /v2.1/servers/{id}   (no header)
→ today's response, unchanged
```

## Invariants

1. **Never more than was negotiated.** A field upstream introduced part-way through the range is not
   served to a client that negotiated below it.
2. **Never less than today for a non-negotiating client.**
3. **The audit is written down** (FR-043), including behaviours found gated upstream and deliberately
   left ungated here, and including divergence 1 above — so that the advertised `min_version`'s
   limits are stated rather than rediscovered.
4. **Comments claiming a gate that does not exist are corrected**, not reproduced.

## Evidence required

Each gate tested at both sides of its boundary, negotiating **explicitly** — a test relying on the
default header would exercise the maximum and prove nothing (FR-044).
