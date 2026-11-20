# Contract: security groups through the compute API

`POST /v2.1/servers/{id}/action` with `addSecurityGroup` or `removeSecurityGroup`, interface attach, and the boot. Errors use the compute error envelope (`itemNotFound`, `badRequest`, `computeFault`).

## Request

```json
{"addSecurityGroup": {"name": "<name or id>"}}
{"removeSecurityGroup": {"name": "<name or id>"}}
```

`name` holds a group's name or its id. An id match wins over a name match, as at server create. An id names any group the caller can see: a member's own project's, an administrator's any project's, as upstream. A name is resolved in the **server's** project; for an administrator acting on another project's server that is constitution register entry 5, while upstream resolves a name in the caller's own project.

## Answers

| Situation | `addSecurityGroup` | `removeSecurityGroup` | Effect |
|---|---|---|---|
| Server not found, or not visible to the caller | `404` | `404` | none |
| `name` missing or empty | `400` | `400` | none |
| The group does not resolve: a name not in the server's project, or an id of no group the caller can see (for a member, another project's group; FR-004) | `404` | `404` | none |
| The server has no port (no network, or the boot has not created one yet) | `404`, "instance_id `<id>` could not be found as device id on any ports" | same | none |
| Add: some port already carries the group | `400` | n/a | none |
| Remove: no port carries the group | n/a | `404` | none |
| Success | `202`, empty body | `202`, empty body | Every port of the server gains the group, or every port carrying it loses it; each changed port's `updated_at` moves; the changed ports' chains are converged on the nodes they are bound to before the answer (FR-041; round 1 rebuilt the floating-IP filters on the serving node) |
| Removing the server's only group | n/a | `202` | Its ports carry no group; each floating IP admits replies only |
| A changed port's node cannot converge its chains: a command fails, the node call fails or exceeds its second, or the node's unit lock is not acquired in time (round 1: the serving node could not rebuild a filter) | `500` (`computeFault`) | `500` | This request's change is undone on each port whose list still equals what the request left (compare-and-set); a port another request has changed since keeps that change. The old chains stay whole, and the log names the port and node |

The `400 cannot remove last security group from instance` answer no longer exists.

## Interface attach (`POST /v2.1/servers/{id}/os-interface`)

| Body | The attached port's groups |
|---|---|
| `{"interfaceAttachment": {"net_id": …}}` | `{default}`, the project's group, created if missing. The server's other groups are not copied, as upstream |
| `{"interfaceAttachment": {"port_id": …}}` | Unchanged |

Detaching a port leaves its groups on it; the server stops reporting them.

## Boot

| Port | Groups |
|---|---|
| The boot's first port | The groups the create resolved; `default` when none were requested |
| Each later port of the same boot | The current groups of the boot's first port, so an add or remove accepted in between is carried over |

A change requested before the first port exists answers `404` (see Answers).

## Round 2 additions (FR-051)

| Request | Answer |
|---|---|
| `addSecurityGroup` that would leave any of the server's ports with both stateful and stateless groups | `409` (`SecurityGroupConnectionStateConflict`), nothing changed |
| `os-interface` attaching a port whose kind differs from the server's other ports | allowed: the check is per port, and the attached port keeps its groups (FR-013) |
| server create whose groups mix both kinds | `202`; the boot fails and the server ends `ERROR` with a fault |
| a group name several of the caller's groups share, in server create, `addSecurityGroup` or `removeSecurityGroup` | `409` (`NoUniqueMatch`), nothing changed (FR-026) |
