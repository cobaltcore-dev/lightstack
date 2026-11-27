# Implementation Plan: Stop Caching the Service Catalog

**Branch**: `feature/581` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/034-drop-catalog-cache/spec.md` (cobaltcore-dev/cloud-storage#581)

## Summary

`BuildServiceCatalog` reads the catalog from the cache before resolving its addresses, writes the
resolved catalog back for 24 hours, and nothing ever removes it. The fix removes the cache from the
catalog entirely. Keystone then holds no cache at all, because the catalog was its only user. So
`NewAuthService`, `keystone.NewService` and `BuildServiceCatalog` each lose a parameter. The
host-cacheability rule and its test go too. The two compose stacks that pretend to enable the cache
stop doing so, and the docs stop describing a catalog cache.

The defect is proven RED by an automated test against today's code with an in-process Redis
stand-in (miniredis). That test goes in the same change that removes the cache parameter, because
"Keystone ignores the cache" then holds by construction. A cache-off regression test, which drives
endpoint writes through the Identity API and checks the next token's catalog, stays.

Mostly deletion. No migration, no API change, no new runtime dependency.

## Technical Context

**Language/Version**: Go 1.27.3 (`go.mod`)
**Primary Dependencies**: gin, `internal/database` (pgx/pgxpool, `modernc.org/sqlite`), `pkg/cache`
(go-redis v9). **Test-only, RED step only**: `github.com/alicebob/miniredis/v2` v2.40.0. It leaves
`go.mod` again in the GREEN change (R2).
**Storage**: PostgreSQL or SQLite. Reads `services` and `endpoints` only. No schema change.
**Testing**: `go test` against `LIGHTSTACK_TEST_DB_URL` on both backends (`internal/keystone`'s
`testPool` and `requireDB`); contract suites; one manual host check with a real Redis
(quickstart §3).
**Target Platform**: Linux server; stub mode is enough for every check here.
**Project Type**: single Go binary (web service)
**Performance Goals**: none new. The catalog is one join over about 20 rows, already run on every
cache miss and with the cache off.
**Constraints**: no runtime dependency, no configuration setting, no API change (FR-013).
**Scale/Scope**: 1 production file with logic (`internal/keystone/auth.go`), 2 constructors
(`auth.go`, `handlers.go`), 1 wiring file (`cmd/lightstack/main.go`), test call sites in 13 files,
2 compose files, 2 docs.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Article / gate | Verdict | Note |
|---|---|---|
| I. Flamingo API compatibility | PASS | Token and catalog responses are unchanged in shape. Only their freshness changes, toward upstream, where Keystone's catalog reflects endpoint writes at once. No divergence, so no register entry. |
| II. Synchronous operations | PASS | Removes state. Nothing new runs in the background. |
| III. Fail-fast | PASS | The catalog no longer depends on Redis, so an unreachable Redis can't slow token issue. |
| IV. Multi-mode | PASS | Mode-independent. Every test runs in stub mode. |
| V. Project isolation | PASS | Per-project substitution is unchanged. The cross-caller key hazard (FR-031a) disappears with the key. |
| VI. Single binary | PASS | — |
| Forbidden patterns | PASS | — |
| Test-first | PASS | RED is automated (R2). The cache-off regression test is written before the code change. Whether it fails before the change depends on the cache being off, so its RED is the cache-on variant (R2). |
| Migrations reversible, both backends | N/A | No migration. |
| Both backends exercised | PASS | No SQL changes, but the regression test runs on both (quickstart §1). |
| Simplicity / YAGNI | PASS | Deletes a mechanism instead of extending it. |

No violations. Complexity Tracking is empty.

**Post-design re-check (after Phase 1)**: unchanged, all PASS. The design adds no table, route,
setting or dependency.

## Project Structure

### Documentation (this feature)

```text
docs/specs/034-drop-catalog-cache/
├── spec.md
├── plan.md              # this file
├── research.md          # R1–R7
├── data-model.md        # no persisted change; what the catalog is built from
├── quickstart.md        # gates, RED/GREEN, host check with real Redis
├── contracts/
│   └── catalog-freshness.md
├── checklists/requirements.md
└── tasks.md             # next phase
```

### Source Code (repository root)

```text
internal/keystone/
├── auth.go                  # BuildServiceCatalog: drop cache Get/TrySet + parameter (:896-1013);
│                            #   delete catalogCacheable (:1032-1049);
│                            #   AuthService.cache + NewAuthService param (:43,52,57);
│                            #   callers :460, :669, :836
├── handlers.go              # Service.cache + NewService param (:22,26,30)
├── auth_catalog_test.go     # delete TestCatalogCacheableOnlyForTheConfiguredHost (:267); call sites;
│                            #   new TestCatalogListsEndpointCreatedAfterIssue, TestCatalogDropsDeletedEndpoint,
│                            #   TestCatalogFollowsSchemeChange, TestBuildServiceCatalogDiscardsAPartialRead
├── catalog_host_test.go     # call sites; rename TestCatalogCacheKeyIncludesHost (:160), reword its comment
├── auth_rgw_compat_test.go, identity_notfound_test.go, identity_surface_test.go,
│   pool_injection_test.go, project_nullable_columns_test.go, routes_wiring_test.go,
│   s3tokens_disabled_test.go, scope_test.go          # constructor calls only
cmd/lightstack/main.go       # :146, :149 drop cacheInstance from the two Keystone constructors
cmd/lightstack/node_route_test.go, internal/middleware/auth_test.go, internal/neutron/node_call_test.go
deployments/docker-compose.yml, deployments/docker-compose-sqlite.yml   # remove Redis wiring
docs/CONFIGURATION.md        # :196-215, :235-238
docs/UPGRADING.md            # :535-536 + new entry
go.mod / go.sum              # miniredis added in the RED change, removed in GREEN
```

**Structure Decision**: the existing single-binary layout. No new package or file except the test.
It goes in `auth_catalog_test.go`, next to the catalog's other database-backed tests.

## Change sequence

1. **RED**: add `TestCatalogFollowsIdentityAPIWrites` with two variants. The cache-off variant uses
   nil. The cache-on variant hands a miniredis-backed `*cache.Cache` to `NewAuthService`. Show the
   cache-on variant failing on all three of its checks: created endpoint, deleted endpoint, changed
   scheme (R2, R3).
2. **GREEN**: remove the catalog cache from `BuildServiceCatalog`, `AuthService`, `Service`, their
   callers and `main.go`. Delete the cache-on variant and miniredis (`go mod tidy`). Delete
   `catalogCacheable` and its test, rename `TestCatalogCacheKeyIncludesHost`, and reword the
   FR-031a comments and the partial-read comment (R4).
3. **Compose**: remove the Redis wiring from the two stacks and bring both up (R5).
4. **Docs**: `CONFIGURATION.md` and `UPGRADING.md` (R6).
5. **Gates**: fmt, lint, `make test` on both backends, contract suites, the host check with a real
   Redis (quickstart).

## Complexity Tracking

None.
