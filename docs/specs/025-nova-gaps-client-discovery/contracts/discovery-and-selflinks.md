# Contract: discovery and self-links (US1 / B3, B4)

**Covers**: FR-001 – FR-009
**Applies to**: `GET /v2.1`, `GET /`, and **every** response in the product carrying a `links` entry

## The one rule

```
scheme  := LIGHTSTACK_ENDPOINT_SCHEME, else "https" if the request arrived over TLS, else "http"
host    := request Host, else LIGHTSTACK_ENDPOINT_HOST
port    := the port the service is reachable on
result  := scheme://host[:port]<path>          -- always absolute, always with a scheme
```

The precedence on `host` is the reversal (FR-002/FR-003). `LIGHTSTACK_ENDPOINT_HOST` is a **fallback**, not an
override. There is no setting to restore the old order (FR-003a).

## Compute version documents

`GET /` (version list) already carries links. `GET /v2.1` does not — that is B4.

```jsonc
// GET /v2.1  — after
{"version": {
  "id": "v2.1", "status": "CURRENT", "version": "2.90", "min_version": "2.1",
  "links": [{"rel": "self", "href": "http://<host>:8774/v2.1"}]   // NEW
}}
```

The client failure this removes: `openstack flavor list` aborts inside the client library with an
indexing error when `version.links` is absent, unless the caller pins `OS_COMPUTE_API_VERSION` (FR-008).

## Every other emitter

Four failure forms, all currently unresolvable for a remote client. All become the rule above (FR-007a).

| Site | Now | After |
|---|---|---|
| `cinder/volumes.go:2253` | `http://lightstack:8776/v3/` | derived |
| `neutron/network.go:255` | `/v2.0` | derived |
| `placement/placement.go:39,60` | `"http://" + Host` | derived |
| `nova/server_update.go:105` | `Host + "/v2.1/servers/…"` | derived |
| `keystone/handlers.go:588,656,739,778,825` | `Host + "/v3/…"` | derived |
| `keystone/groups.go:47,121,156,221` | `Host + "/v3/groups/…"` | derived |

FR-007b: this includes **every service's** version document, not only compute's — a client reads whichever
service it is about to use.

## Console origin

`deriveConsoleOrigin` (`nova/console.go:299`) exists only because the port argument was dropped outside
the configured-host branch. Once FR-004 holds, re-assess it. Whatever happens, the console link must not
advertise the API's own port 8774 (FR-009, SPEC-021 FR-021).

## Test obligations

- **RED first**: `internal/common/baseurl_test.go:43,49` asserts the *current* precedence, passing the
  request host as the literal `"ignored:1"`. Rewrite before changing the function.
- New cases: port applied without `LIGHTSTACK_ENDPOINT_HOST`; request with no host still yields an absolute URL;
  TLS-terminated-ahead request advertises `https`.
- Contract: every service's version document has an absolute `links[].href`.
- Integration: `openstack flavor list` with no version pinned; one `OS_*_ENDPOINT_OVERRIDE` honoured.
