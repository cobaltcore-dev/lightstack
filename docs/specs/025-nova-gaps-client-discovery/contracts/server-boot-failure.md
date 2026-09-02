# Contract: a boot that cannot get its network (US3 / B13)

**Covers**: FR-015 – FR-021, and FR-055 from US9
**Applies to**: `POST /v2.1/servers`, `GET /v2.1/servers/{id}`

## Two paths, split by when the fault is knowable (FR-016a)

```
                        ┌─ knowable before the create is answered ──> 400, nothing created
requested network ──────┤
                        └─ fails after the 202 ──────────────────────> ERROR + fault, visible on read
```

### Path 1 — pre-flight refusal (FR-017)

Before the instance row is inserted (`nova/handlers.go:394`, alongside the existing flavor, quota,
keypair and security-group validation), each requested network must exist and be usable by the caller.
"Usable" is the network service's **attach** rule, which the port allocation applies too — do **not**
write a second one. It is narrower than the visibility rule, which also admits external networks: a
shared network takes anyone's guest; an unshared external network takes only an administrator's,
whoever owns it (upstream `network:attach_external_network`, admin by default); any other network takes
only its owner's.

```jsonc
// POST /v2.1/servers  with an unknown or unusable network
400 {"badRequest": {"code": 400, "message": "... <network id> ..."}}   // names the network
// ... with an unshared external network, as a non-administrator: upstream's ExternalNetworkAttachForbidden
403 {"forbidden": {"code": 403, "message": "It is not allowed to create an interface on external network <network id>"}}
```

### Path 2 — visible failure (FR-015, FR-016)

The current fail-open is `nova/handlers.go:664`, `// Continue with empty networks rather than failing`.
Replace with: mark `ERROR`, write the fault, stop. `svc.markInstanceError` (`:385`) exists but records no
reason — it needs the fault text.

```jsonc
// GET /v2.1/servers/{id}  after a post-accept allocation failure
{"server": {
  "status": "ERROR",
  "addresses": {},
  "fault": {"code": 500, "created": "2026-08-31T…", "message": "…could not allocate a port on network …"}
}}
```

`details` is omitted deliberately — upstream fills it with a traceback for administrators; Lightstack has none.

The tenant reads `message`, so it carries the network service's reason only when the tenant can act on
it — the network is unknown or forbidden, has no subnet, or has no free address. A database or host
failure is recorded as `could not attach the server to network <id>` and logged in full, as upstream
records only a non-Nova exception's class name (`nova/compute/utils.py`, `exception_to_dict`). The same
rule holds for Path 1: a network check that *fails*, rather than refuses, is a `500` without the error's
text.

## Invariants

- **FR-018**: reported state and reported addresses never contradict. `ACTIVE` with `{}` for a server
  that requested a network is not reachable.
- **FR-020**: several networks requested, one fails → the whole boot fails. No partly-networked success.
- **FR-019**: anything already created is released or stays attributed to the failed server. The failed
  server stays deletable by its owner.
- **FR-021**: a server requesting **no** network is unaffected. This story constrains requested networks.
- **FR-055**: US9's volume-creation failure lands here too, not in an image-boot fallback.

## Test obligations

- Stub: unknown network → 400 and no instance row. Unusable (another project's) network → 400.
- Real: induce a port failure → never `ACTIVE`, `fault.message` readable from `server show`.
- Multi-network partial failure behaves as total failure.
- The failed server can be deleted.
