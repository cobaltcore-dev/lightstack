# Contract: server-list filters (US1, FR-001 – FR-009)

**Surface**: `GET /v2.1/servers`, `GET /v2.1/servers/detail`
**Today**: every filter parameter is accepted and none is applied.

## Honoured filters

Applied to the rows returned. A value matching nothing yields an empty collection.

| Parameter | Semantics | Availability |
|---|---|---|
| `name` | Regular expression over the display name (upstream: "exact and regex matching") | All callers |
| `status` | The status vocabulary the listing itself emits | All callers |
| `image` | Image UUID | All callers |
| `flavor` | Flavor UUID | All callers |
| `changes-since` | Servers updated at or after the timestamp | All callers |
| `user_id` | Owning user | Administrators |
| `project_id` / `tenant_id` | Owning project — **narrows within scope only** | Administrators |
| `key_name`, `locked`, `task_state`, `power_state` | Equality | Administrators |

## Refused filters

`400`, naming the filter. Upstream honours these for administrators and **silently ignores** them for
other callers; the product records none of the attributes, so it refuses both cases.

`host`, `hostname`, `availability_zone`, `tags` — and `ip` until the batched address lookup exists.

**`ip` is conditional** (FR-003b): it is in upstream's general-caller set and the product does hold
the data in the ports' fixed addresses, so honouring it costs a subquery that US7's address rewrite
already builds. Until that lands `ip` is refused; once it lands `ip` is honoured. It is never
ignored.

## Ignored parameters

Any parameter that is neither honoured nor refused is ignored and the request succeeds — paging
(`limit`, `marker`, `offset`), sorting, and anything a client sends incidentally. Refusing these
would newly break callers that work today (FR-004a, FR-007).

## Request / response

```
GET /v2.1/servers?name=web
→ 200 {"servers": [ {…"name": "web"…} ]}

GET /v2.1/servers?name=doesnotexist
→ 200 {"servers": []}                       # NOT the project's servers

GET /v2.1/servers?status=ERROR
→ 200 {"servers": [ …only failed servers… ]}

GET /v2.1/servers?host=compute-01
→ 400 {"badRequest": {"message": "…host…"}}  # names the filter

GET /v2.1/servers?some_unknown_param=x
→ 200 {"servers": [ …unfiltered… ]}          # ignored, as today
```

## Invariants

1. **A filter never widens.** A non-administrator naming another project sees only their own servers.
   An administrator naming another project *without* `all_tenants` sees an empty collection — the
   filter does not imply the opt-in (FR-006a).
2. **Both representations agree.** The brief and detailed listings honour the same filters and return
   the same set of servers for the same query.
3. **Filters compose with paging.** The marker lookup runs under the same predicates as the page
   query; no server is skipped or repeated across a filtered page boundary.
4. **The unfiltered request is unchanged**, byte for byte, from before this feature.
5. **An invalid value never surfaces as a server fault** — `?flavor=not-a-uuid` is a client error or
   an empty result, never a 500.

## Evidence required

For **each** honoured filter: a non-matching value returns empty. A test asserting only the matching
case passes against today's code and is not acceptable evidence (FR-009).
