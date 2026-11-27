# Tasks: Stop Caching the Service Catalog

**Input**: `docs/specs/034-drop-catalog-cache/` — plan.md, spec.md, research.md (R1–R7),
data-model.md, contracts/catalog-freshness.md, quickstart.md
**Branch**: `feature/581` (based on `2c9c792`) · **Ticket**: cobaltcore-dev/cloud-storage#581

**Tests**: required. Constitution Article III is non-negotiable, and spec FR-012 specifies the test
and its RED. The RED is automated against an in-process Redis (miniredis). That variant is removed
together with Keystone's cache parameter (spec Clarifications, research R2).

**Story map**: US1 and US2 are delivered by the same code change. US1 carries it, and US2 adds its
own RED check (the scheme) and verification on top. US3 (compose) and US4 (docs) are independent
of each other and of the code.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: parallel-safe: different files, no dependency on an incomplete task
- **[Story]**: US1–US4 from spec.md

---

## Phase 1: Setup

- [X] T001 Baseline on `2c9c792` with a fresh test database (`make db-up`, then `LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'`): run `go test ./internal/keystone/ ./cmd/lightstack/ ./internal/middleware/ -count=1`, then the same with `LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db'`. Record any failures in this file under *Notes* so later runs can tell pre-existing failures from new ones.
- [X] T002 Add the test-only stand-in: `go get github.com/alicebob/miniredis/v2@v2.40.0` (brings `github.com/yuin/gopher-lua`). Only `go.mod`/`go.sum` change. It is imported from a `_test.go` file only (T004).

---

## Phase 2: Foundational (test harness)

- [X] T003 In `internal/keystone/auth_catalog_test.go`, add the helpers the freshness test uses, reusing the package's existing ones (`seedRGWCompatFixtures`, `passwordAuthBody`, `postTokens`, `decodeTokenBody`, `getToken`, `decodeValidationBody`, `postJSON` in `auth_rgw_compat_test.go`):
  - `newCatalogRouter(t *testing.T, c *cache.Cache) *gin.Engine`. A copy of `newRGWCompatRouter(false)` (`auth_rgw_compat_test.go:41`) that passes `c` to `NewAuthService` and `NewService`. It exists only until T013.
  - `catalogURLs(cat) []string`. Every endpoint URL of every entry whose `Type` is the test service type.
  - `createProbeEndpoint(t, r, url) (serviceID, endpointID string)`. `POST /v3/services` (a unique type such as `catalog-freshness-<uuid8>`), then `POST /v3/endpoints` (`interface: public`, `region: RegionOne`), both with `X-Test-Roles: admin` and `X-Test-User: rgwCompatSeededAdminID`. Asserts `201` and registers `t.Cleanup` deleting both rows straight from `testPool`, so a failed run leaves nothing behind for parallel packages.

**Checkpoint**: the package compiles (`go vet ./internal/keystone/`).

---

## Phase 3: User Story 1: an endpoint change reaches the next token (P1) 🎯 MVP

**Goal**: endpoints created or deleted through the Identity API appear in, or vanish from, the next issued *and* validated token's catalog, with the cache on or off (FR-001 to FR-003, FR-006 to FR-008; contract C1, C2, C6).
**Independent test**: `go test ./internal/keystone/ -run 'TestCatalogListsEndpointCreatedAfterIssue|TestCatalogDropsDeletedEndpoint' -count=1 -v` on both backends.

### Tests (RED first)

- [X] T004 [US1] In `internal/keystone/auth_catalog_test.go`, add `TestCatalogFollowsIdentityAPIWrites` with `requireDB(t)`, `seedRGWCompatFixtures(t)` and `t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "example.com")`. Without that setting today's code would not cache the `httptest` host, and the RED would pass for the wrong reason (research R3). Two subtests share one body `run(t, c *cache.Cache)`:
  - `cache-off` runs with `c = nil`.
  - `cache-on` runs with `c` from `cache.NewCache(cache.Config{RedisURL: "redis://" + miniredis.RunT(t).Addr(), Enabled: true, KeyPrefix: "lightstack:", DefaultTTL: time.Hour})`.
  - The body: (1) issue a token via `postTokens(passwordAuthBody(rgwCompatProjectID))` to warm the cache; (2) `createProbeEndpoint`; (3) issue a token and assert the URL is in its catalog; validate the step-1 token via `getToken` and assert the same; (4) `DELETE /v3/endpoints/{id}` (admin headers, `204`), issue a token, assert the URL is absent.
  - Failure messages name the step and say "stale catalog".
- [X] T005 [US1] Show RED on `2c9c792` plus T002 to T004, on PostgreSQL: `cache-on` fails at steps (3) and (4) and `cache-off` passes. Paste the failing lines under *Notes*. If `cache-on` passes, check T004's host setting before going further.

### Implementation (GREEN)

- [X] T006 [US1] `internal/keystone/auth.go` `BuildServiceCatalog` (`:896`): drop the `cacheInstance *cache.Cache` parameter. Delete the FR-031a comment block and the `cacheable`/`Get` block (`:900-915`) and the `TrySet` block (`:1008-1011`). Reword the partial-read comment (`:990-992`) to say a partial catalog would hand this token missing endpoints, with no mention of caching, and drop "uncached" from the log line at `:994`.
- [X] T007 [US1] `internal/keystone/auth.go`: delete `catalogCacheable` and its doc comment (`:1032-1049`), so `catalogHost`'s comment (`:1016-1031`) sits on `catalogHost` again. Re-read that comment and keep it.
- [X] T008 [US1] `internal/keystone/auth.go`: remove the `cache` field (`:43`), the `cacheInstance` parameter of `NewAuthService` (`:52`) and its assignment (`:57`). Update the three callers (`:460`, `:669`, `:836`) to `BuildServiceCatalog(s.pool, <project>, requestHost, s.glanceS3Endpoint)`. Drop the `pkg/cache` import; `time` stays.
- [X] T009 [US1] `internal/keystone/handlers.go`: remove the unused `cache` field (`:22`), the `cacheInstance` parameter of `NewService` (`:26`) and its assignment (`:30`), and the `pkg/cache` import if it is no longer used.
- [X] T010 [US1] `cmd/lightstack/main.go:146,149`: call `keystone.NewAuthService(pool, cfg.Keystone.JWTSecret, cfg.Keystone.TokenTTL)` and `keystone.NewService(pool, authService)`. `cacheInstance` still goes to Nova, Neutron and Glance unchanged (FR-009).
- [X] T011 [P] [US1] Update the remaining test callers of the three signatures, which are the compiler errors after T006 to T010: `cmd/lightstack/node_route_test.go`, `internal/middleware/auth_test.go`, `internal/neutron/node_call_test.go`, and in `internal/keystone/` `auth_rgw_compat_test.go`, `identity_notfound_test.go`, `identity_surface_test.go`, `pool_injection_test.go`, `project_nullable_columns_test.go`, `routes_wiring_test.go`, `s3tokens_disabled_test.go`, `scope_test.go`. Drop the trailing `nil` cache argument only.
- [X] T012 [US1] `internal/keystone/auth_catalog_test.go`: delete `TestCatalogCacheableOnlyForTheConfiguredHost` (`:267`) and drop the `nil` cache argument from the remaining `BuildServiceCatalog` calls. In `internal/keystone/catalog_host_test.go`, drop it from every call and rename `TestCatalogCacheKeyIncludesHost` (`:160`) to `TestCatalogIsBuiltForEachCallersHost`. Rewrite its comment (`:155-159`) and its second failure message to state the per-caller-host requirement without a cache.
- [X] T013 [US1] `internal/keystone/auth_catalog_test.go`: remove the `cache-on` subtest and `newCatalogRouter`. `TestCatalogFollowsIdentityAPIWrites` runs the body once against `newRGWCompatRouter(false)`. Remove the `miniredis` and `pkg/cache` imports. Run `go mod tidy` and confirm `grep -n miniredis go.mod go.sum` prints nothing (FR-013, research R2).
- [X] T014 [US1] GREEN on both backends: `go test ./internal/keystone/ ./cmd/lightstack/ ./internal/middleware/ ./internal/neutron/ -count=1`. `TestCatalogFollowsIdentityAPIWrites` passes, and nothing fails that T001 did not record. Then `grep -rn 'service_catalog\|catalogCacheable' --include='*.go' .` prints nothing (SC-003, FR-007).

**Checkpoint**: US1 done; the code change is complete.

---

## Phase 4: User Story 2: a deployment change reaches the first token after restart (P1)

**Goal**: a changed scheme, host or s3 endpoint, or a migrated catalog row, is in the first catalog served afterwards (FR-004, FR-005; contract C4, C5).
**Independent test**: `TestCatalogFollowsSchemeChange`, and quickstart §3's restart.
**Depends on**: the RED tasks T004 and T005 for its RED; T006 to T013 for its GREEN.

- [X] T015 [US2] Do this together with T004, before T005 runs. Add step (5) to the shared body in `internal/keystone/auth_catalog_test.go`: `t.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", "https")`, issue a token, and assert every URL of every non-`s3` entry starts with `https://`. The scheme is read on each call (`internal/common/scheme.go:14`), so this stands in for a restart (research R3). T005's RED must show `cache-on` failing at step (5) too, and T014's GREEN must pass it.
- [X] T016 [US2] Confirm the other restart inputs need no code. Host and s3 endpoint are already covered without a cache: `TestCatalogHostPrecedence` and `TestCatalogEchoesRequestHost` in `catalog_host_test.go`, and `TestBuildServiceCatalogS3EndpointFromConfig` in `auth_catalog_test.go`. Migrated rows are read like any other row (`TestBuildServiceCatalogListsPlacement`). Run those four tests on both backends after T014 and record the result under *Notes*. Add no new test unless one of them fails.

**Checkpoint**: US1 and US2 done.

---

## Phase 5: User Story 3: the sample deployments do what they say (P2)

**Goal**: the compose stacks set no ignored cache variable and start no unused Redis (FR-010).
**Independent test**: quickstart §2.

- [X] T017 [P] [US3] `deployments/docker-compose.yml`: delete the `redis` service with its comment (`:27-42`), the `redis:` condition under `lightstack.depends_on` (`:54-55`, `postgres` stays), `LIGHTSTACK_REDIS_URL` and `LIGHTSTACK_CACHE_ENABLED` (`:65-66`), and `redis_data` under the top-level `volumes:` (`:127-128`; the other volumes stay).
- [X] T018 [P] [US3] `deployments/docker-compose-sqlite.yml`: the same removals: the `redis` service with its comment (`:18-33`); the whole `depends_on:` block of `lightstack` (`:42-44`), because `redis` is its only entry; `LIGHTSTACK_REDIS_URL` and `LIGHTSTACK_CACHE_ENABLED` (`:63-64`); `redis_data` (`:89-90`; `lightstack_state` stays). Leave every other compose file alone; none of them has Redis (research R5).
- [X] T019 [US3] Run `grep -nE 'redis|CACHE_ENABLED' deployments/docker-compose.yml deployments/docker-compose-sqlite.yml` and confirm it prints nothing. Then `docker compose -f <file> config -q` for both. Then bring each stack up per quickstart §2: every service is healthy and `openstack token issue` succeeds. Tear down with `down -v`.

**Checkpoint**: US3 done.

---

## Phase 6: User Story 4: the documentation matches the behaviour (P3)

**Goal**: no operator doc describes a catalog cache (FR-011).
**Independent test**: `grep -n -i 'catalog' docs/CONFIGURATION.md docs/UPGRADING.md | grep -i cach` shows only the new UPGRADING section.

- [X] T020 [P] [US4] `docs/CONFIGURATION.md`: delete the subsection "`LIGHTSTACK_ENDPOINT_HOST` also decides what the service catalog caches" (`:196-215`) and the bullet "The catalog cache is keyed on the host as well as the project" (`:235-238`). Re-read the surrounding host-precedence text so it still flows. Add a sentence that the catalog is read from the database on every token issue and validation.
- [X] T021 [P] [US4] `docs/UPGRADING.md`: in "Placement is listed in the service catalog", replace the sentence about the 24-hour cached copy (`:535-536`) with "placement appears in the next token". Add a section modelled on "Redis cache keys are now scoped to the caller" (`:228`): the service catalog is no longer cached; endpoint writes, scheme, host and s3-endpoint changes and catalog migrations show in the next token; `lightstack:service_catalog:*` keys a previous version wrote are never read and expire within 24 h; the optional `redis-cli --scan --pattern 'lightstack:service_catalog:*' | xargs -r redis-cli del`, adjusted for `cache.key_prefix`. Say that the cache still serves flavors, images and networks (research R6, R7).

**Checkpoint**: all stories done.

---

## Phase 7: Polish and gates

- [X] T022 `make fmt` and `make lint` clean. `make test` on PostgreSQL and on SQLite (quickstart §1), with no failure beyond those T001 recorded.
- [X] T023 Contract suites per `test/contract/README.md`, with no new failure against the known-failures list.
- [X] T024 Host check with a real Redis, cache on (quickstart §3), on the branch build, plus the "before" run on `2c9c792`. Before: the endpoint and the scheme show stale. After: both show at once, and `redis-cli --scan --pattern '*service_catalog*'` prints nothing. Record both under *Notes*. This covers acceptance criteria 2 and 3 of the ticket.
- [X] T025 Confirm the other caches are untouched: with the cache on (quickstart §3 setup), `openstack flavor show <id>`, then `redis-cli --scan --pattern '*'` lists `lightstack:flavor:<id>` (FR-009). Only single-resource reads are cached (`nova/handlers.go:3131`, `glance/images.go:473`, `neutron/network.go:1032`); lists are not.
- [ ] T026 Branch review against cloud-storage#581.

---

## Dependencies

```text
T001 ─► T002 ─► T003 ─► T004+T015 ─► T005 (RED) ─► T006 ─► T007 ─► T008 ─► T009 ─► T010 ─► T011 ─► T012 ─► T013 ─► T014 ─► T016
                                                                                                                        │
T017, T018 ─► T019   (US3: independent of everything above)                                                             ├─► T022 ─► T023 ─► T024 ─► T025 ─► T026
T020, T021           (US4: independent of everything above)                                                             │
```

- T006 to T010 are sequential, because T006, T007 and T008 edit `auth.go` and the build stays broken until T011. Treat T006 to T012 as one compile unit and run the tests only at T014.
- US3 and US4 can start any time, even before T001.

## Parallel examples

- **US3 alongside US1**: T017 and T018 (two compose files) while T006 to T010 run.
- **US4 alongside US1**: T020 and T021 (two docs) while T006 to T010 run.
- **Inside US1**: T011's eleven test files are independent of one another once T006 to T010 have set the signatures.

## Implementation strategy

1. **MVP = US1** (T001 to T014). The code change and its regression test, which stop the catalog from ever being stale.
2. **US2** adds its RED check inside the same test (T015, written before T005) and confirms the restart inputs (T016). There is no code of its own.
3. **US3 and US4** are small, independent cleanups, and either can land in any order.
4. **Gates** (T022 to T026) once all four are in.

Suggested commits, for the user to make: (a) T002 to T005 plus T015 as the RED commit (`test(keystone): …`); (b) T006 to T014 and T016 (`fix(keystone): stop caching the service catalog`); (c) T017 to T019 (`chore(deploy): …`); (d) T020 and T021 (`docs: …`). (a) keeps miniredis in `go.mod` until (b) removes it.

## Notes

- **T001 (2026-11-25, `2c9c792`)**: `go test ./internal/keystone/ ./cmd/lightstack/ ./internal/middleware/ ./internal/neutron/ -count=1` all `ok` on PostgreSQL (`make db-up`) and on SQLite (`/tmp/ls-test/t.db`). No pre-existing failures.
- **T004 deviation**: the shared body became three scenarios, each with a fresh router and cache (`created endpoint`, `deleted endpoint`, `changed scheme`). In one sequence, the cache warmed at step 1 never held the new endpoint, so "deleted endpoint absent" passed for the wrong reason. The delete scenario now warms the cache *after* creating the endpoint. `catalogURLs` takes a `tokenCatalog` alias, the anonymous catalog type both response structs share.
- **T005 RED (2026-11-25, both backends, identical)**: `cache-on` fails in all three scenarios; `cache-off` passes in all three.
  ```
  issued token's catalog lists [] for catalog-freshness-95b0ea87; want http://example.com:8999/catalog-freshness-95b0ea87 (stale catalog)
  validated token's catalog lists [] for catalog-freshness-95b0ea87; want http://example.com:8999/catalog-freshness-95b0ea87 (stale catalog)
  deleted endpoint still in the catalog: [http://example.com:8999/catalog-freshness-74ce0b72] (stale catalog)
  compute endpoint http://example.com:8774/v2.1 is not https after the scheme change (stale catalog)   … and every other non-s3 entry
  --- FAIL: TestCatalogFollowsIdentityAPIWrites/cache-on/{changed_scheme,created_endpoint,deleted_endpoint}
  --- PASS: TestCatalogFollowsIdentityAPIWrites/cache-off/{changed_scheme,created_endpoint,deleted_endpoint}
  ```
  The RED state is kept as a patch at `/tmp/581-red.patch` (go.mod, go.sum, the test) for a separate RED commit.
- **T006–T013 (2026-11-25)**: `BuildServiceCatalog(pool, projectID, requestHost, s3Endpoint string)`. `NewAuthService(pool, jwtSecret, tokenTTL)`, `NewService(pool, authService)`. T011 touched 13 test files (30 call lines). With one router left, T013 inlined the scenario wrapper; each scenario uses `newRGWCompatRouter(false)`. Two stale test comments fixed besides (`auth_catalog_test.go` "nil cache => fresh"; the T081 header in `catalog_host_test.go`). `go mod tidy` returns `go.mod`/`go.sum` to HEAD.
- **T014 GREEN (2026-11-25, both backends)**: `go test ./internal/keystone/ ./cmd/lightstack/ ./internal/middleware/ ./internal/neutron/ -count=1` all `ok`. `TestCatalogFollowsIdentityAPIWrites` passes in all three scenarios. `grep -rn 'service_catalog\|catalogCacheable' --include='*.go' .` prints nothing.
- **T016 (2026-11-25, both backends)**: `TestCatalogHostPrecedence`, `TestCatalogEchoesRequestHost`, `TestBuildServiceCatalogS3EndpointFromConfig`, `TestBuildServiceCatalogListsPlacement` PASS. No new test needed.
- **T019 (2026-11-25, podman-compose)**: both files `config -q` OK; the grep for `redis|CACHE_ENABLED` prints nothing. The SQLite stack (`-p ls581`) came up healthy with no Redis container; `openstack token issue` and `catalog list` OK. The PostgreSQL stack ran under a throwaway override (postgres renamed `ls581-postgres`, port 55481), because its `container_name: lightstack-postgres` and `5432` are the developer's `make db-up` database. It came up healthy and token issue worked. Both stacks were torn down with `down -v`, and the image and override removed.
- **T020/T021 (2026-11-25)**: `CONFIGURATION.md` loses the "decides what the service catalog caches" subsection and the cache-key bullet. "Two consequences" becomes "One consequence". A "not cached" paragraph follows the forwarded-host bullet. `UPGRADING.md`: the placement sentence now says "next token". The new section "The service catalog is no longer cached" sits after "Redis cache keys are now scoped to the caller": what went stale, leftover keys and the optional cleanup, and the compose stacks no longer pretending to enable the cache.
- **T022 (2026-11-25)**: `make fmt` no change; `make lint` 0 issues. `make test` on SQLite (fresh `/tmp/ls-test`): all packages `ok`. On PostgreSQL (`make db-up`, the long-lived DB): `internal/nova` failed 4 tests in the parallel run and passes with `-p 1`, the known seeded-project race. `internal/glance` `TestTaskListAndGetAreOwnerScoped` fails because of `pending` import tasks dated 2026-11-24 left in that database's `tasks` table. It **fails identically on `2c9c792`** (scratch clone, same DB) and passes on SQLite: pre-existing database state, not this change. The database was not modified.
- **T024 (2026-11-25)**: real Redis (throwaway container), `cache.enabled: true`, fresh SQLite per run, host `localhost` (the configured host, so the old code caches). The same script ran against both binaries:

  | Check | before (`2c9c792`) | after |
  |---|---|---|
  | endpoint created → `catalog show` | `service probe581 not found` (stale) | lists `http://localhost:8999/v1` |
  | endpoint deleted (cache warmed after the create) → `catalog show` | still lists it (stale) | `[]` |
  | Redis `*service_catalog*` | `lightstack:service_catalog:…0002:localhost` | none |
  | restart with `LIGHTSTACK_ENDPOINT_SCHEME=https`, Redis kept | 0 of 8 URLs https | 7 of 7 https |

  (8 vs 7 entries: the stale catalog still holds the probe service, which the new code deletes at cleanup.) Found on the way, pre-existing and outside this feature: `/v3/regions` is not routed (404), so `openstack endpoint create --region RegionOne`, as in the ticket's *How to test*, fails. `GET /v3/endpoints/{id}` is not routed (405), so `openstack endpoint delete` fails before sending its `DELETE`. The quickstart now uses commands that work. The harness's first run was invalid: a process from the previous start still held `:35357`. The script now refuses to start on a bound port.
- **T025 (2026-11-25)**: `openstack flavor show <id>` with the cache on wrote `lightstack:flavor:00000000-0000-0000-0000-000000000010`; a token issue in the same run wrote no catalog key.
- **T023 (2026-11-25)**: the contract suite ran against fresh SQLite servers from `2c9c792` and from the branch, in stub mode with Glance `storage_mode: stub`: **341 pass, 12 skip, 1 fail on both, identical**. The failure, `TestGlanceDownloadImageData_Contract`, comes from stub Glance storage returning zero bytes, a harness choice. A first attempt failed every test with 401 because `test/contract/README.md` says `OS_USER_DOMAIN_NAME=default`, and Lightstack matches the domain name case-sensitively (`default` → 401, `Default` → 201). That is pre-existing and outside this feature.
- **Branch review fixes (2026-11-25, `2026-11-25-review.md`, all ten findings)**:
  - SRC-1: the three scenarios are now `TestCatalogListsEndpointCreatedAfterIssue`, `TestCatalogDropsDeletedEndpoint`
    and `TestCatalogFollowsSchemeChange`, on the helpers `catalogFreshnessEnv`, `issueCatalogToken` and
    `newCatalogProbe`.
  - SRC-2: `createdID` plus `cleanupIdentityRow`.
  - SRC-3: `tokenCatalog` is a defined type used by both response structs.
  - SRC-5: `t.Setenv` for scheme and host.
  - SRC-4 deviates from the suggested test. With `failedRows` (zero rows) it passes even with the `rows.Err()`
    branch deleted, because the empty-catalog fallback serves the same hardcoded catalog. Checked by
    `-overlay` mutation: the suggested test passed against the mutant. `TestBuildServiceCatalogDiscardsAPartialRead`
    uses `partialReadDB`, one good row and then an iteration error, and fails against the mutant.
  - MD-1 to MD-5 and OTHER-1 as suggested.
