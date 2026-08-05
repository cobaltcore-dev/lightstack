# Contract — Keystone service-catalog endpoint host (F5)

**Service**: Identity (Keystone) v3 service catalog. **Clients**: Horizon, python-openstackclient, gophercloud.

## Endpoints

- `GET /v3/auth/catalog` and the catalog embedded in a token issue/validate response
- `openstack catalog show volume` / `volumev3` / `s3`

## Contract

In any deployment where a non-`localhost` endpoint host is configured (`O3K_ENDPOINT_HOST`), **no advertised
endpoint URL may contain `localhost`**. Specifically:

- The `volume` service-type alias (service `…015`) MUST resolve to the configured host (not `localhost`).
- The `volumev3` service (service `…013`) MUST continue to resolve to the configured host (already correct).
- The `s3`/object-store service (service `…016`) MUST resolve to the configured host (not `localhost`).

Mechanism: migration `064` reseeds the `…015`/`…016` URLs to the `http://o3k:` convention that runtime
substitution already rewrites to the configured host; a defensive substitution also normalizes any residual
`localhost` host.

## Behavioral guarantees

- Local/dev (endpoint host unset → defaults to `localhost`) is unaffected: endpoints resolve to `localhost`
  as before (no regression).
- A client that resolves `volume`/`volumev3`/`s3` from the catalog can reach the corresponding service.

## Contract-test assertions

1. With a non-`localhost` `O3K_ENDPOINT_HOST`, `catalog show volume`, `volumev3`, and `s3` → every URL uses the
   configured host; grep for `localhost` in the resolved catalog returns nothing for these services.
2. A `volume list`/`show` through the resolved block-storage endpoint succeeds.
3. With the host unset, resolved URLs remain `localhost` (dev regression guard).
4. Migration `064` up then down leaves the catalog in its prior state (reversible).
