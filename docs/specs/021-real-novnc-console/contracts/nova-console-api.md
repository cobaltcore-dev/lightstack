# Contract: Nova console API surfaces (SPEC-021)

Grounded in the Flamingo api-ref and `rest_api_version_history.rst` (Context7 `/openstack/nova`). Constitution I makes these shapes non-negotiable.

## 1. `POST /v2.1/servers/{server_id}/remote-consoles`

Microversion **2.6+**.

**Request**

```json
{"remote_console": {"protocol": "vnc", "type": "novnc"}}
```

**Response `200`**

```json
{"remote_console": {
  "protocol": "vnc",
  "type": "novnc",
  "url": "http://10.2.199.101:6080/vnc_lite.html?path=%3Ftoken%3D9f8c1a2b3d4e5f60718293a4b5c6d7e8"
}}
```

`url` = `<configured origin>` + `/vnc_lite.html?path=%3Ftoken%3D<token>`. The `path` parameter is a URL-encoded `?token=<token>`, matching upstream exactly; the noVNC client appends it to its WebSocket URL, so the browser opens `ws://<origin>/?token=<token>`. **Verified** working against the pinned image (research.md R5).

Upstream's example names `vnc_auto.html`; that page does not exist in noVNC 1.6.0 and returns 404. We name `vnc_lite.html`. The URL is opaque to clients — Horizon iframes it, the CLI prints it — so this is not a contract break.

## 2. `POST /v2.1/servers/{server_id}/action` → `os-getVNCConsole`

Legacy action, retained for compatibility (FR-005). Must return a URL equivalent to surface 1 for the same instance.

**Request**

```json
{"os-getVNCConsole": {"type": "novnc"}}
```

**Response `200`**

```json
{"console": {"type": "novnc", "url": "http://10.2.199.101:6080/vnc_lite.html?path=%3Ftoken%3D…"}}
```

## 3. `GET /v2.1/os-console-auth-tokens/{token}`

Microversion **2.31+**. Compatibility surface only (FR-015) — our proxy resolves tokens from the projection directory and never calls this. The console MUST NOT depend on it.

**Response `200`**

```json
{"console": {
  "instance_uuid": "b48316c5-71e8-45e4-9884-6c78055b9b13",
  "host": null,
  "port": null,
  "internal_access_path": "/var/lib/lightstack/console-sockets/b48316c5-71e8-45e4-9884-6c78055b9b13.sock"
}}
```

The console is a unix socket, which has neither host nor port. Both keys stay, as `null`, because clients read upstream's shape; the socket goes in `internal_access_path`, the field upstream uses for a target that is not `host:port`. `tls_port` is optional upstream and omitted — we terminate no TLS to the guest.

**Response `404`** for a token that is unknown, expired, or whose instance is gone. The reply MUST NOT distinguish these cases.

**Response `500`** when the lookup itself fails (the database is unreachable). Upstream answers 404 there too, but only because its tokens live in per-cell databases and a cell that does not answer is indistinguishable from one without the token; Lightstack has one database, so a failure is never a missing token. The status does not depend on the token, so it reveals nothing about it.

## Status-code matrix

| Condition | Code | Body | Note |
|---|---|---|---|
| Success | `200` | as above | |
| Unsupported console type (`xvpvnc`, `serial`, `spice-html5`, `rdp-html5`) | `400` | `badRequest` naming the type | Matches upstream, which returns 400 for RDP since the Hyper-V driver was removed |
| Malformed body / missing `protocol` or `type` | `400` | `badRequest` | |
| Instance not running, or has no graphical console | `409` | `conflictingRequest` | Upstream maps `InstanceNotReady` to 409 |
| Instance absent, or in another project and caller lacks `admin` | `404` | `itemNotFound` | Never reveals existence (Constitution V) |
| **Stub mode** | `501` | `notImplemented`, naming stub mode | No upstream analogue — a deliberate, documented choice (research.md R2) |
| No console proxy deployed (multi-node, or `consoleProxy.enabled=false`) | `400` | `badRequest`, "Unavailable console type novnc" | Upstream's answer when `[vnc] enabled = false` (`ConsoleTypeUnavailable`); stub mode keeps its 501 (FR-027a) |
| Hypervisor unreachable or the console socket cannot be read | `500` | `computeFault` | Fail-fast; never a fabricated endpoint (FR-003). A real-mode service whose hypervisor never connected answers here too, not with stub mode's 501 |
| Guest console is a TCP listener (a domain defined before the socket console) | `500` | `computeFault` | Refused, never proxied (FR-017); `docs/UPGRADING.md` has the fix |
| Guest console is a socket outside `console-sockets` (a domain defined before 2026-10-01) | `500` | `computeFault`, "console misconfigured on this host" | The proxy cannot reach it, so it is refused, never proxied (FR-017); `docs/UPGRADING.md` has the fix |
| Token unknown / expired (surface 3) | `404` | `itemNotFound` | |
| Token lookup fails — database unreachable (surface 3) | `500` | `computeFault` | Upstream says 404 because of cells; Lightstack has one database |

Error bodies follow Nova's synchronous-fault shape (`{"<faultName>": {"code": N, "message": "…"}}`), which the existing `common.SendError` helpers already produce.

## Behaviour requirements

- Every success **must** be backed by a port read from libvirt for this request (FR-003, FR-004). A response is never produced from a stored or computed port.
- Issuance is synchronous: read the port, insert the row, write the projection entry, then respond (Constitution II).
- A token grants access to exactly one instance and the caller cannot name a target at connect time (FR-012).
- `admin` may open a console in any project; everyone else is confined to their own (FR-013), via the shared `common.HasRole`.
- Issued / refused-with-reason outcomes are logged without recording reusable token values (FR-028).

## Unchanged surfaces

`POST /v2.1/servers/{id}/action` → `os-getConsoleOutput` keeps returning the guest's captured serial log, byte for byte as today (FR-026). It is the documented fallback and a regression if it changes.
