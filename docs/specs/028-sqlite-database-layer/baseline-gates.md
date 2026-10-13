# Pre-change gate baseline (T001)

Recorded on `feature/598` @ `278d106` (working tree: SPEC-028 documents only), 2026-10-12, on PostgreSQL 18.6 (`make db-up`).
This is the oracle for T022 and SC-002: after the type-only conversion every result below must be identical.

| Gate | Command | Result |
|---|---|---|
| Unit tests, PostgreSQL leg | `LIGHTSTACK_TEST_DB_URL=postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable go test -count=1 -p 1 $(go list ./... \| grep -v /test/contract)` | **1571 pass, 0 fail, 0 skip** (subtests counted) in 16 packages |
| Lint | `make lint` | **0 issues** |
| Contract suite | stub binary on a fresh database, `go test -count=1 ./test/contract/...` | **324 pass, 0 fail, 12 skip** (top-level tests) |
| Quick integration suite | `test/quick_test.sh` | **17 passed, 0 failed** |
| Horizon UI probe | `make test-horizon-ui` | **85 passed, 0 failed** |

The stub binary ran on the host (`config/lightstack.yaml` with `state_path` redirected to a scratch directory, `LIGHTSTACK_JWT_SECRET`, `LIGHTSTACK_ENDPOINT_HOST=localhost`, `LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER=true`, as Docker Compose sets them), against a database reset with `make db-down && make db-up`.

## Unit tests per package

| Package | Pass | Fail | Skip |
|---|---|---|---|
| `cmd/lightstack` | 17 | 0 | 0 |
| `internal/cinder` | 90 | 0 | 0 |
| `internal/common` | 191 | 0 | 0 |
| `internal/console` | 27 | 0 | 0 |
| `internal/glance` | 34 | 0 | 0 |
| `internal/health` | 16 | 0 | 0 |
| `internal/keystone` | 232 | 0 | 0 |
| `internal/metadata` | 35 | 0 | 0 |
| `internal/middleware` | 25 | 0 | 0 |
| `internal/neutron` | 287 | 0 | 0 |
| `internal/nova` | 458 | 0 | 0 |
| `internal/teardown` | 9 | 0 | 0 |
| `pkg/hypervisor` | 73 | 0 | 0 |
| `pkg/networking` | 60 | 0 | 0 |
| `pkg/paths` | 2 | 0 | 0 |
| `pkg/storage` | 15 | 0 | 0 |

## Contract tests skipped at baseline

- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_CreateResource_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_DeleteResource_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_ErrorHandling_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_Filtering_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_GetResource_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_ListResources_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_Pagination_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestTemplate_UpdateResource_Contract`
- `github.com/cobaltcore-dev/lightstack/test/contract.TestWS0_ImageNormalizesToRaw`
- `nova.TestNovaActionsByName_Contract`
- `nova.TestNovaCreateRefusesForeignNetwork_Contract`
- `nova.TestNovaFlavorAccessControl_Contract`

## Final gates (T104, 2026-10-12)

Same commands as above, on the finished branch; SQLite runs on a fresh file, PostgreSQL on a fresh
database. The contract runs set `LIGHTSTACK_DB_URL` (the SQLite URL on the SQLite leg), so the five
seeded contract tests run; without it they skip and the suite reports 17 skips, not the 12 below.

| Gate | PostgreSQL | SQLite |
|---|---|---|
| Unit tests | **1774 pass, 0 fail, 5 skip** — the three SQLite transaction-mode matrices, the baseline generator (`-update` only), the lock test's helper process | **1752 pass, 0 fail, 17 skip** (with `-race`) — the PostgreSQL-only tests, `TestSchemaParity` (needs PostgreSQL), the generator, the helper process |
| `make lint` | 0 issues | — |
| Contract suite | **325 pass, 0 fail, 12 skip** | **325 pass, 0 fail, 12 skip** |
| `test/quick_test.sh` | 17 passed, 0 failed | 17 passed, 0 failed |
| `make test-horizon-ui` | 85 passed, 0 failed | 85 passed, 0 failed |
| Migration round trip (`TestMigrationsRoundTrip`) | pass — up, down through 001, up; 074 down and up | pass — through the baseline's down file |

Against the baseline: the unit counts grew by the new tests and nothing that passed fails; the
contract suite gained `TestNovaServerCreateOverQuotaIs403_Contract` with the same 12 skips. The
behaviour differences are the corrections of FR-035a (quota refusal `403`) and FR-036a (`400` for a
resource still `creating`).
