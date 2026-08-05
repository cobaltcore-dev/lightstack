# Contract — Keystone service-catalog endpoint host (F5)

**Service**: Identity (Keystone) v3 service catalog. **Clients**: Horizon, python-openstackclient, gophercloud.

## Endpoints

- `GET /v3/auth/catalog` and the catalog embedded in a token issue/validate response
- `openstack catalog show volume` / `volumev3` / `s3`

## Contract

In any deployment where a non-`localhost` endpoint host is configured (`LIGHTSTACK_ENDPOINT_HOST`), **no advertised
endpoint URL may contain `localhost`**. Specifically:

- The `volume` service-type alias (service `…015`) MUST resolve to the configured host (not `localhost`).
- The `volumev3` service (service `…013`) MUST continue to resolve to the configured host (already correct).
- The `s3`/object-store service (service `…016`) MUST resolve to the configured **RGW endpoint**
  (`cfg.Glance.S3Endpoint`) — never `localhost`, and never the Lightstack host: the RGW is an external
  service whose address is unrelated to `LIGHTSTACK_ENDPOINT_HOST`. It MUST be omitted from the catalog
  entirely when RGW is not configured.

Mechanism, per service:

- `…015` (`volume` alias): migration `064` reseeds its URL to the `http://lightstack:` convention that runtime
  substitution already rewrites to the configured host. This is the migration's **only** statement.
- `…016` (`s3`): **not** reseeded. `BuildServiceCatalog` special-cases the service — it serves the resolved
  `cfg.Glance.S3Endpoint` verbatim (`internal/keystone/auth.go:635-637`) and omits the service when RGW is
  unconfigured (`:618-620`), so migration `062`'s `localhost` placeholder is never advertised. Reseeding it to
  the Lightstack host would be wrong, because the real value is an external address. See `research.md` §F5.
- All services: a defensive substitution normalizes any residual `localhost` host
  (`substituteURLTemplates`, `auth.go:697-701`), so no future `localhost`-seeded endpoint can recur this bug.

## Behavioral guarantees

- Local/dev (endpoint host unset → defaults to `localhost`) is unaffected: endpoints resolve to `localhost`
  as before (no regression).
- A client that resolves `volume`/`volumev3`/`s3` from the catalog can reach the corresponding service.

## Contract-test assertions

1. With a non-`localhost` `LIGHTSTACK_ENDPOINT_HOST`, `catalog show volume` and `volumev3` → every URL uses the
   configured host; grep for `localhost` in the resolved catalog returns nothing for either.
1a. `catalog show s3` → the URL equals the configured `glance.s3Endpoint` exactly (the external RGW address),
   and in particular contains no `localhost`. With RGW unconfigured, the `s3` service is absent from the
   catalog rather than present with a placeholder.
2. A `volume list`/`show` through the resolved block-storage endpoint succeeds.
3. With the host unset, resolved URLs remain `localhost` (dev regression guard).
4. Migration `064` up then down leaves the catalog in its prior state (reversible).
