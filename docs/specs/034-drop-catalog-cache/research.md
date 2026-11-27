# Research: Stop Caching the Service Catalog

Measured against `feature/581` @ `2c9c792`. Line numbers are from that commit.

## R1 — Remove the catalog cache instead of fixing it

**Decision**: `BuildServiceCatalog` reads `services` ⋈ `endpoints` on every call. Keystone keeps no
cache.

**Rationale**: recorded on cloud-storage#581 (Decision, 2026-11-17). In short, the cache saves one
join over about 20 rows on paths that already run several queries. A hit costs a Redis round trip
plus a JSON decode, about the query's own cost on PostgreSQL and more on SQLite. It applies only
with `cache.enabled` and only for callers addressing the configured host (`catalogCacheable`,
`auth.go:1047`). Fixing it means invalidation on every catalog write and migration, and it would
still race in a rolling DaemonSet that shares one Redis.

**Consequence found while planning**: the catalog is the only thing Keystone uses the cache for.
`AuthService.cache` (`auth.go:43`) is read only at the three `BuildServiceCatalog` calls
(`:460` password auth, `:669` token auth, `:836` `BuildTokenResponse`, which validation uses).
`Service.cache` (`handlers.go:22`) is stored and never read. Both fields go, and so do the cache
parameters of `NewAuthService` (`auth.go:52`) and `NewService` (`handlers.go:26`). Leaving an
unused parameter in place would be a lie in the signature, and `unparam` would flag it.

**Alternatives considered**:
- *Cache raw rows, resolve on read, invalidate on the five writes and at startup.* Rejected per the
  ticket: needs a permanent Redis test harness and invalidation every future catalog write must
  remember, and still goes stale after direct SQL edits.
- *Add scheme and s3 endpoint to the key.* Fixes the restart cases only; endpoint writes still go
  unseen for 24 h.

## R2 — RED with an in-process Redis, then the test leaves

**Decision**: the RED change adds `github.com/alicebob/miniredis/v2` v2.40.0 (latest on the module
proxy) as a test dependency (it brings `github.com/yuin/gopher-lua` with it). A cache-on variant of
the regression test builds a real `*cache.Cache` through `cache.NewCache` pointed at
`miniredis.RunT(t).Addr()` and hands it to `NewAuthService`. It fails on today's code. The GREEN
change removes Keystone's cache parameter, so the variant has nothing left to pass the cache to. It
is deleted, and `go mod tidy` drops miniredis. The cache-off variant stays. (User decision, clarify session 2026-11-25.)

**Rationale**: miniredis speaks RESP over TCP, so `pkg/cache` and go-redis v9 run unmodified,
including `NewCache`'s `Ping`. No CI service, no container. After GREEN the property "the catalog
ignores the cache" is enforced by the compiler (no cache handle reaches Keystone) and by FR-007's
source search. Keeping a test that can no longer fail would only look like coverage.

**Alternatives considered**:
- *A real Redis in `make` or CI.* New test setup on both backends to prove one historical defect.
- *An interface over `pkg/cache` with a fake.* An abstraction added only for a test, which Article
  VIII forbids, and it would not exercise go-redis.
- *Moving the Keystone wiring out of `main()` so a process-level test could keep cache-on
  permanently.* A `main.go` refactor plus a permanent dependency to guard a mechanism that no longer
  exists. Rejected by the user.

## R3 — How the RED test reaches the cached path

The current code caches only when the resolved host equals `catalogHost("")`, which is
`LIGHTSTACK_ENDPOINT_HOST`, or `localhost` when that is unset (`auth.go:908`, `:1047`).
`httptest.NewRequest` addresses `example.com`. So the test sets `LIGHTSTACK_ENDPOINT_HOST=example.com`
with `t.Setenv`. Without it the RED variant would pass for the wrong reason: nothing cached.

Test shape (`internal/keystone/auth_catalog_test.go`, DB-backed): three scenarios, each with a fresh
router (and, in the RED step, a fresh cache), sharing these pieces:

- Router built like `newRGWCompatRouter` (`auth_rgw_compat_test.go:41`): `NewAuthService` plus
  `NewService(...).RegisterRoutes` with the test role header for admin calls. Fixtures from
  `seedRGWCompatFixtures`, which already gives a project, a role and a password-auth user, with
  cleanup.
- A probe: a service of a test-only type and one endpoint for it, created through `POST /v3/services`
  and `POST /v3/endpoints` as admin. Unique type, removed on cleanup, so parallel packages reading the
  seeded catalog are untouched (see the seeded-project flakiness the team has met before).

The scenarios:

1. **Created endpoint.** Issue a token (with a cache, this warms it), create the probe, issue again: the
   catalog lists the endpoint. Validate the first token (`GET /v3/auth/tokens`, `X-Subject-Token`): its
   catalog lists it too. This covers both issue and validation (FR-001).
2. **Deleted endpoint.** Create the probe *first*, issue a token, then `DELETE /v3/endpoints/{id}` and
   issue again: the endpoint is gone. The order matters. Run after scenario 1's warm-up instead, the
   cache would never have held the endpoint, and "absent after the delete" would pass for the wrong
   reason. The first build did exactly that; see tasks.md Notes, T004.
3. **Changed scheme.** Issue a token, `t.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", "https")`, issue again:
   every non-s3 URL is `https://`. The scheme is read on each call (`internal/common/scheme.go:14`), so
   an env change in the test is a faithful stand-in for a restart with a new scheme (US2). The `s3`
   entry is the configured RGW URL, served as-is (`auth.go:977`), so it is excluded.

RED evidence: all three scenarios fail in the cache-on variant on `2c9c792`; the cache-off variant
passes all three. The cache-off variant has no RED of its own. Its job is the regression guard, and the
RED for the behaviour comes from the cache-on variant. Both outcomes are recorded in tasks.md. After the
branch review the scenarios are three top-level tests: `TestCatalogListsEndpointCreatedAfterIssue`,
`TestCatalogDropsDeletedEndpoint`, `TestCatalogFollowsSchemeChange`.

## R4 — What else goes, and what stays

- **Goes**: the FR-031a block comment and the `cacheable` logic in `BuildServiceCatalog`
  (`auth.go:900-915`, `:1008-1011`); `catalogCacheable` and its comment (`:1032-1049`);
  `TestCatalogCacheableOnlyForTheConfiguredHost` (`auth_catalog_test.go:267`); the
  `pkg/cache` import in `auth.go` (`time` stays: `tokenTTL` uses it).
- **Moves back into place**: `catalogHost`'s doc comment (`:1016-1031`) currently sits above
  `catalogCacheable`, separated from its function. Deleting `catalogCacheable` reattaches it. Check
  that its FR-031b paragraph still reads true; it does, because it is about substitution.
- **Stays, reworded**: the partial-read fallback (`:990-995`) still must not serve a partial
  catalog. The reason is now "this token would get a catalog with endpoints missing", not "it would
  be cached for 24 h". Log message drops "uncached".
- **Stays, renamed**: `TestCatalogCacheKeyIncludesHost` (`catalog_host_test.go:160`) asserts two
  callers each get their own host, still a correctness property with no cache involved. Rename it to
  `TestCatalogIsBuiltForEachCallersHost` and rewrite its comment without the cache.
- **Stays unchanged**: `pkg/cache`; Nova, Neutron, Glance cache use; the `cache:` block in
  `config/lightstack.yaml` and the chart (`values.yaml:247-251`). The chart's "single shared Redis
  (cross-node invalidation)" comment is still true for flavors, images and networks, which do
  invalidate (`nova/flavors.go:85,134`, `glance/images.go:667,792,1503`, `neutron/network.go:1189`).

## R5 — Compose stacks

`deployments/docker-compose.yml` and `deployments/docker-compose-sqlite.yml` each lose: the `redis`
service, the `redis:` entry under `lightstack.depends_on`, `LIGHTSTACK_REDIS_URL`,
`LIGHTSTACK_CACHE_ENABLED`, and the `redis_data` volume. `docker-compose-kvm.yml`, `-horizon.yml`,
`.test.yml` and `.rgw-test.yml` contain no Redis and are untouched. Verify by bringing each changed
stack up and issuing a token (quickstart §2).

`test/benchmark/README.md:132-137` tells the reader to `redis-cli ping` when *cache benchmarks*
fail. Those benchmarks are not about the catalog, and that Redis is the reader's own, so it stays.

## R6 — Documentation

- `docs/CONFIGURATION.md:196-215`: the whole subsection "`LIGHTSTACK_ENDPOINT_HOST` also decides
  what the service catalog caches" goes. `LIGHTSTACK_ENDPOINT_HOST` no longer has that second role.
- `docs/CONFIGURATION.md:235-238`: the bullet "The catalog cache is keyed on the host as well as the
  project" goes.
- `docs/UPGRADING.md:535-536`: the sentence that placement appears once a cached catalog expires
  goes. Placement now appears on the next token.
- `docs/UPGRADING.md`: a new section, modelled on "Redis cache keys are now scoped to the caller"
  (`:228`). It says the service catalog is no longer cached, so endpoint writes, scheme or host
  changes and catalog migrations show in the next token. `lightstack:service_catalog:*` entries
  left by the previous version are never read and expire within 24 h. It gives the optional
  `redis-cli --scan --pattern 'lightstack:service_catalog:*' | xargs -r redis-cli del`, with the
  `key_prefix` caveat.
- `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md:555` describes Horizon's own Django session cache. It
  stays.

## R7 — Entries an earlier version left in Redis

**Decision**: ignore them. Nothing reads them, and their TTL is at most 24 h.

**Alternatives considered**: a startup `TryDeletePattern("service_catalog:*")`. It would need
Keystone to hold the cache again just to delete keys nothing reads, and it is permanent code for a
one-off upgrade. The UPGRADING command covers operators who want them gone.
