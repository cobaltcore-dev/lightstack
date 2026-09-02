# Contract: service catalog addressing (US4, FR-030 – FR-034)

**Surface**: the catalog embedded in the token response (`POST /v3/auth/tokens`, and token validation)
**Today**: endpoint hosts are rewritten from `LIGHTSTACK_ENDPOINT_HOST`, defaulting to `localhost`; the host
the client addressed is never consulted.

## Host precedence

```
host = request host              # what the client actually addressed, port stripped
     ↓ if absent
     = LIGHTSTACK_ENDPOINT_HOST         # the configured fallback
     ↓ if absent
     = localhost                 # last resort, so the URL is always parseable
```

The same precedence self-links follow. **Each endpoint keeps its own port** — the ports live in the
seeded endpoint rows and are not replaced.

## Request / response

```
POST /v3/auth/tokens        Host: lightstack.example.com:35357
  (deployment configured with LIGHTSTACK_ENDPOINT_HOST=lightstack.internal)
→ catalog names http://lightstack.example.com:8774/v2.1  for compute
                                    ^^^^ addressed host, ^^^^ endpoint's own port
```

## Invariants

1. **Resolvable by the receiver.** A client reaching the deployment under any name it can resolve
   gets a catalog it can use, and completes token → catalog → version document → API call with no
   per-command override.
2. **Never another caller's host** (FR-031a). Two clients in one project that addressed different
   hostnames each receive a catalog naming their own. The catalog cache key distinguishes the host —
   the request host is client-supplied, so a shared key would let one caller choose where another
   sends its token.
3. **Always absolute and parseable**, including for a request carrying no usable host.
4. **No two-port authorities.** A request host carrying a port must not produce
   `http://host:35357:8774`.
5. **The external object-storage endpoint is served as configured**, untouched by this contract.
6. **The fallback catalog obeys the same rule** — a partial database read must not serve a
   differently-addressed catalog.
7. **Forwarded headers are not trusted** (FR-031b). `X-Forwarded-Host` is not read; deployments
   behind a proxy configure it to preserve `Host`.

## Evidence required

A catalog requested under a host other than the configured one, and two catalogs requested under
different hosts in succession with neither served the other's (FR-034).
