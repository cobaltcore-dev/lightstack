# Data Model: Stop Caching the Service Catalog

No table, column, index or migration changes. This feature removes a derived copy of the catalog.
It does not change where the catalog comes from.

## Service catalog (derived, per request)

Built by `BuildServiceCatalog` from:

| Source | Fields read | Filter |
|---|---|---|
| `services` | `id`, `type`, `name` | `enabled = true` |
| `endpoints` | `id`, `interface`, `url`, `region` | `enabled = true` (or no endpoint) |

and resolved per request with:

| Input | Origin | Effect |
|---|---|---|
| project id | token scope | `{project_id}`, `$(project_id)s`, `%(project_id)s`, `%s` templates |
| host | request `Host`, else `LIGHTSTACK_ENDPOINT_HOST`, else `localhost` | rewrites `lightstack`/`localhost` hosts |
| scheme | `LIGHTSTACK_ENDPOINT_SCHEME` | forced on each URL |
| s3 endpoint | `glance.s3_endpoint` | replaces the `s3` service's URL; omits it when empty |

**Lifecycle**: built for one response, never stored. Before the change it was also written to
`<key_prefix>service_catalog:<project>:<host>` for 24 h when the host was the configured one.
After the change nothing writes that key.

**Fallback**: a query error, a row-iteration error or an empty result serves the built-in catalog
(`buildHardcodedCatalog`), unchanged.

## Shared cache (Redis, optional)

Unchanged for the single-resource keys `flavor:<id>`, `network:<scope>:<id>` and
`image:<project>:<scope>:<id>`; lists are not cached. Holds no catalog key after the change. Keys an
earlier version wrote are inert and expire (research R7).
