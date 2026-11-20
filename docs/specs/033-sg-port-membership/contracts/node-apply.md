# Contract: the node-to-node apply call (FR-034, FR-041; R27)

Internal to Lightstack; never in the service catalog.

## Request

`POST http://<tunnel_ip>:<neutron port>/internal/v1/converge`

Header `X-Lightstack-Node-Token: <JWT>` — HS256 with `keystone.jwt_secret`; claims `aud: "lightstack-node"`, `iat`, `exp` ≤ `iat` + 10 s, `node` (the target's node name), `body_sha256` (hex of the body).

```json
{"unit": "router", "ids": ["<router id>"], "absent": false}
{"unit": "dhcp",   "ids": ["<network id>"], "absent": false}
{"unit": "ports",  "ids": ["<port id>", "…"], "absent": ["<port id being deleted>"]}
```

`absent` asks for the unit, or the listed ports, to converge as if their records were gone (a deletion's first step, FR-040). Nothing else in the body: the target reads the records.

## Answers

| Answer | Meaning to the caller |
|---|---|
| `204` | converged: the target's state matches the records (or absence) |
| `401` | token missing, expired, wrong audience, wrong `node`, or body digest mismatch |
| `409` | the target is not the node for this unit: not the network node, or a port listed for presence that is not bound to it. Ports listed in `absent` are accepted whatever their binding, so a moved or cleared binding's old node can remove their chains |
| `500` | the target could not converge; its unit is as the records held before, or reconciliation converges it |
| no answer within one second | failed step |

Every answer but `204`, and every timeout, is a failed step for the request that called (FR-008, FR-040). The caller never retries.

## Target behaviour

The target takes the unit's lock and applies under **one shared deadline of 900 ms from receipt** (lock wait included; `500` when missed), so it gives up before its caller's one second does. It reads the records, renders, compares, applies, and answers. A caller that timed out undoes; the undo re-converges the unit from the restored records, so whatever the target finished late is put back. A call to the node itself bypasses HTTP. A user token is refused here (`401`), and a node token is refused by every `/v2.0` route (`401`).
