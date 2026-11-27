# Contract: the node-to-node apply call (FR-034, FR-041; R27)

Internal to Lightstack; never in the service catalog.

## Request

`POST http://<tunnel_ip>:<neutron port>/internal/v1/converge`

Header `X-Lightstack-Node-Token: <JWT>` — HS256 with `keystone.jwt_secret`; claims `aud: "lightstack-node"`, `iat`,
`exp` ≤ `iat` + 10 s, `node` (the target's node name), `body_sha256` (hex of the body).

```json
{"unit": "router", "ids": ["<router id>"]}
{"unit": "router", "ids": ["<router id>"], "absent_ids": ["<port forwarding, interface port or subnet, gateway port being deleted>"]}
{"unit": "router", "ids": ["<router id>"], "absent": true}
{"unit": "dhcp",   "ids": ["<network id>"]}
{"unit": "ports",  "ids": ["<port id>", "…"], "absent_ids": ["<port id being deleted>"]}
```

`absent` and `absent_ids` ask for the unit, or some of its records, to converge as if those records were gone: a
deletion's first step (FR-040). Each has one shape, and either may be left out (SPEC-033 T220). For a router or DHCP,
`"absent": true` reads the whole unit as gone, and `absent_ids` lists the unit's records to read as gone (a port
forwarding, an interface's port, a subnet of an interface's port that keeps others (T315), the gateway port, a
floating IP, a subnet, a port); a request giving both is a `400`.
For ports, `absent` is refused (`400`) and `absent_ids` lists the ports being deleted, read as gone wherever they are
bound and from the remote-group expansions; `ids` names the ports the request changed, which must be bound to the
target. The target converges every port bound to it, so a request naming no port converges it as its records say: the
old node of a binding that moved, which the records no longer bind there. Nothing else in the body: the target reads the
records.

## Answers

| Answer | Meaning to the caller |
|---|---|
| `204` | converged: the target's state matches the records (or absence) |
| `400` | the body is not a unit request, or names no unit the target converges: an unknown kind, a router or DHCP without exactly one id or absent both whole and in part, ports absent as a whole, an id that is not one or is given twice |
| `401` | token missing, expired, wrong audience, wrong `node`, or body digest mismatch |
| `409` | the target is not the node for this unit: not the network node, or a port listed in `ids` that is not bound to it. Ports listed in `absent_ids` are accepted whatever their binding |
| `500` | the target could not converge; its unit is as the records held before, or reconciliation converges it |
| no answer within one second | failed step |

Every answer but `204`, and every timeout, is a failed step for the request that called (FR-008, FR-040). The caller
never retries.

## Target behaviour

The target takes the unit's lock and applies under **one shared deadline of 900 ms from receipt** (lock wait included;
`500` when missed), so it gives up before its caller's one second does. It reads the records, renders, compares,
applies, and answers. A caller that timed out undoes; the undo re-converges the unit from the restored records, so
whatever the target finished late is put back. A call to the node itself bypasses HTTP. A user token is refused here
(`401`), and a node token is refused by every `/v2.0` route (`401`).
