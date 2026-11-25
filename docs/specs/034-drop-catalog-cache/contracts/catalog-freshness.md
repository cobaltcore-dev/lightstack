# Contract: Service-catalog freshness

The API's shape does not change. `POST /v3/auth/tokens` and `GET /v3/auth/tokens` return the same
`token.catalog` structure as before (Identity v3, Flamingo). This contract pins down the one
behaviour that changes, freshness, so tests can assert it.

| # | Given | When | Then |
|---|---|---|---|
| C1 | any `cache.enabled` value | `POST /v3/endpoints` returns `201` | the next `POST /v3/auth/tokens` and `GET /v3/auth/tokens` list the endpoint |
| C2 | any `cache.enabled` value | `DELETE /v3/endpoints/{id}` returns `204` | the next token's catalog does not list it |
| C3 | any `cache.enabled` value | `PATCH /v3/services/{id}` or `DELETE /v3/services/{id}` succeeds | the next token's catalog reflects it |
| C4 | any `cache.enabled` value | the process starts with a different `LIGHTSTACK_ENDPOINT_SCHEME`, `LIGHTSTACK_ENDPOINT_HOST` or `glance.s3_endpoint` | the first token's catalog uses the new value |
| C5 | any `cache.enabled` value | a migration changes `services`/`endpoints` rows | the first token after it runs lists them |
| C6 | `cache.enabled: true` | any token is issued or validated | no key matching `*service_catalog*` is read or written |
| C7 | `cache.enabled: true`, Redis unreachable after start | a token is issued | the catalog is the same as with the cache off |

Verified by: C1, C2 and C4 (scheme) by `TestCatalogListsEndpointCreatedAfterIssue`,
`TestCatalogDropsDeletedEndpoint` and `TestCatalogFollowsSchemeChange` (their cache-on variant was RED
before the change and went with Keystone's cache parameter). C1 to C6 on a host with a real Redis (quickstart §3).
C6 also by construction: Keystone holds no cache. C3, C5 and C7 follow from C6 and are not tested
separately.

Unchanged and still covered by existing tests: per-caller host (`catalog_host_test.go`), scheme
(`TestBuildServiceCatalogScheme`), s3 handling, placement listing, hardcoded-catalog parity.
