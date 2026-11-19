# Contract: security groups through the compute API

`POST /v2.1/servers/{id}/action` with `addSecurityGroup` or `removeSecurityGroup`, interface attach, and the boot. Errors use the compute error envelope (`itemNotFound`, `badRequest`, `computeFault`).

## Request

```json
{"addSecurityGroup": {"name": "<name or id>"}}
{"removeSecurityGroup": {"name": "<name or id>"}}
```

`name` holds a group's name or its id. It is resolved in the **server's** project. An id match wins over a name match, as at server create. For an administrator acting on another project's server, the server's project is the tenant's: this is constitution register entry 5, while upstream resolves a name in the caller's own project.

## Answers

| Situation | `addSecurityGroup` | `removeSecurityGroup` | Effect |
|---|---|---|---|
| Server not found, or not visible to the caller | `404` | `404` | none |
| `name` missing or empty | `400` | `400` | none |
| The group does not resolve in the server's project, including another project's group named by id | `404` | `404` | none |
| The server has no port (no network, or the boot has not created one yet) | `404`, "instance_id `<id>` could not be found as device id on any ports" | same | none |
| Add: some port already carries the group | `400` | n/a | none |
| Remove: no port carries the group | n/a | `404` | none |
| Success | `202`, empty body | `202`, empty body | Every port of the server gains the group, or every port carrying it loses it; each changed port's `updated_at` moves; the floating-IP filters of the changed ports are rebuilt on the serving node before the answer |
| Removing the server's only group | n/a | `202` | Its ports carry no group; each floating IP admits replies only |
| The serving node cannot rebuild a changed port's filter: a command fails or takes longer than one second, or the wait for another change to the node's floating-IP state takes longer than one second | `500` (`computeFault`) | `500` | This request's change is undone on each port whose list still equals what the request left (compare-and-set); a port another request has changed since keeps that change. The old filters stay whole, and the log names the port and address |

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
