# Contract: Neutron error envelope (US2 / B10)

**Covers**: FR-006, FR-007, FR-007a, FR-008, FR-009, FR-010, FR-011
**Applies to**: every response with a 4xx or 5xx status from the network service (`:9696`)

## Shape

Upstream builds the body in `neutron/api/api_common.py`:

```python
body = serializer.serialize({'NeutronError': get_exception_data(e)})
err_data = {'type': e.__class__.__name__, 'message': e, 'detail': ''}
```

So the contract is:

```json
{
  "NeutronError": {
    "type": "NetworkNotFound",
    "message": "Network 00000000-0000-0000-0000-999999999999 could not be found.",
    "detail": ""
  }
}
```

**Exactly three inner keys.** `detail` is always present and usually empty. There is **no `code`
key** — that is the compute envelope's field and its presence is the defect.

## Contract tests

| ID | Given | When | Then |
|---|---|---|---|
| **E1** | an id matching no network | `GET /v2.0/networks/{id}` | `404`; body has exactly the key `NeutronError`; `jq 'keys'` → `["NeutronError"]` |
| **E2** | any Neutron error response | inspect the inner object | keys are exactly `type`, `message`, `detail`; **no `code`** |
| **E3** | absent network / subnet / port / router / floating IP / security group | `GET` each by a non-existent id | each `404` with the resource's own `type` (`NetworkNotFound`, `SubnetNotFound`, …), not one shared value |
| **E4** | a malformed request body | `POST /v2.0/networks` with `{` | `400`, `type` = `BadRequest`, Neutron envelope |
| **E5** | no auth token | `GET /v2.0/networks` | `401`, `type` = `NotAuthorized`, **Neutron** envelope — this path goes through the shared auth middleware, so it is the one most likely to be missed |
| **E6** | an unrouted path under the Neutron port | `GET /v2.0/nonexistent` | `404`, Neutron envelope — not the framework default and not the compute envelope |
| **E7** | a wrong method on a real route | `DELETE /v2.0/networks` | `405`, Neutron envelope |
| **E8** | a handler panic (fault injection) | any route | `500`, `type` = `NeutronException`, Neutron envelope |
| **E9** | the auto-allocated-topology 404 | `GET /v2.0/auto-allocated-topology/{project}` with none | `404`, Neutron envelope, produced by the **shared** path — the hand-rolled block at `auto_allocated_topology.go:30-34` is gone (FR-009) |
| **E10** | compute, block-storage, identity and image errors | request an absent resource from each | envelopes **unchanged**: `itemNotFound` etc. still present (FR-010) |
| **E11** | the committed error-consistency check | `make test-errors` | 10/10, exit 0 — fails 1 before the change (`test/error_handling_test.sh:158`) |
| **E12** | the neutron contract suite | `make test-contract` | still passes; it keys off status, not envelope, which is why it never caught this |

## Type mapping

Derived from `(StatusCode, Code, Resource)`. Enumerated in **one** place (FR-007a) so it cannot drift.

### 404 — from the resource name already passed to `NewNotFoundError`

| Resource string in code | `type` |
|---|---|
| `network` | `NetworkNotFound` |
| `subnet` | `SubnetNotFound` |
| `port` | `PortNotFound` |
| `router` | `RouterNotFound` |
| `router interface` | `RouterInterfaceNotFound` |
| `floating IP` | `FloatingIPNotFound` |
| `security group` | `SecurityGroupNotFound` |
| `security group rule` | `SecurityGroupRuleNotFound` |
| `rule` | `SecurityGroupRuleNotFound` |
| `policy` | `QosPolicyNotFound` |
| `subnet pool` | `SubnetPoolNotFound` |
| `address scope` | `AddressScopeNotFound` |
| `port forwarding` | `PortForwardingNotFound` |
| `metering label` | `MeteringLabelNotFound` |
| `metering label rule` | `MeteringLabelRuleNotFound` |
| `RBAC policy` | `RbacPolicyNotFound` |
| `trunk` | `TrunkNotFound` |
| `agent` | `AgentNotFound` |
| `auto-allocated topology` | `AutoAllocationFailure` |
| `project` | `ProjectNotFound` |
| anything else | `NotFound` |

### Other statuses — from the existing `Code` key

| `Code` today | Status | `type` |
|---|---|---|
| `badRequest` | 400 | `BadRequest` |
| `unauthorized` | 401 | `NotAuthorized` |
| `forbidden` | 403 | `NotAuthorized` |
| `conflict` / `conflictingRequest` | 409 | the resource's `InUse` where one exists, else `Conflict` |
| `overLimit` | 413 / 429 | `OverQuota` |
| `badMethod` | 405 | `BadRequest` |
| `serviceUnavailable` | 503 | `ServiceUnavailable` |
| **`computeFault`** | 500 | **`NeutronException`** |

`computeFault` is the important row twice over: it is O3K's internal-error code *and* a compute name
that must never appear on a network response. It maps to upstream's base class name — the single
documented generic of FR-007a. Everything with no upstream counterpart lands here: `failed to create
bridge`, `failed to attach TAP to bridge`, `database error`.

## Implementation constraint

The renderer is selected **per service**, not by the call site. Every error path already funnels
through `common.SendError` (research R1), including the shared auth middleware, panic recovery,
`c.Errors`, the unrouted-404 handler and the 405 handler. Marking Neutron once and rendering inside
`SendError` converts all of them and changes **none** of the 244 call sites.

⚠️ **The marker must be set at engine level, not on the route group.** Verified in
`cmd/o3k/main.go:626-637` (`createNeutronServer`):

```go
r := gin.New()
r.Use(middleware.ErrorHandlingMiddleware())   // :628  panic recovery → E8
...
r.Use(middleware.AuthMiddleware(authService)) // :632  401 → E5
r.NoRoute(middleware.NotFoundHandler())       // :633  unrouted 404 → E6
r.HandleMethodNotAllowed = true
r.NoMethod(middleware.MethodNotAllowedHandler()) // :635  405 → E7
svc.RegisterRoutes(r.Group(""))               // :637  the route group starts HERE
```

Auth runs at engine level *before* the group exists, and `NoRoute`/`NoMethod` never run inside a group
at all. A group-level marker would therefore miss **E5, E6 and E7** — the three paths a client is most
likely to hit first — while E1–E4 and E8–E12 passed, producing exactly the half-fix FR-008 forbids.

Register the marker as the **first** `r.Use(...)` in `createNeutronServer`, ahead of
`ErrorHandlingMiddleware`, so panic recovery renders through it too. Neutron has its own `gin.New()`
engine, so an engine-level marker cannot leak into another service (E10).

A partial conversion that *looks* finished is explicitly not acceptable (FR-008). E2 is the assertion
that catches it — it checks for the **absence** of `code`, not just the presence of the wrapper.
