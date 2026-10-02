# Quickstart: verify the database-pool injection

**Feature**: `027-database-pool-injection` | **Date**: 2026-10-01

How to check SC-001…SC-005 on this branch. Every block is meant to be pasted into zsh from the repository root.

## 0. Test database

The DB-backed unit tests need a PostgreSQL at the URL the Makefile uses:

```bash
make db-up
export LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'
```

`make db-up` and the compose stack (step 3) both create a container named `lightstack-postgres` on port 5432. Run `make db-down` before step 3.

## 1. Acceptance search (SC-001)

```bash
git grep -n 'database\.DB\b' -- '*.go'
```

Expected: no output. Before the change it lists 874 lines.

## 2. Unit tests and lint (SC-003, SC-004)

```bash
go build ./...
go vet ./...
go test -race -count=1 -p 1 $(go list ./... | grep -v /test/contract)
make lint
```

Expected: all green and `0 issues`. Each package's result matches the baseline recorded before the change.

The Keystone demonstrator runs on its own, on a pool it opens itself:

```bash
go test -count=1 -run TestServiceRunsOnItsOwnPool -v ./internal/keystone/
```

Expected: `PASS`. Without `LIGHTSTACK_TEST_DB_URL` it reports `SKIP`.

Without a test database, the DB-backed tests skip rather than fail (US2 scenario 3):

```bash
env -u LIGHTSTACK_TEST_DB_URL go test -count=1 -p 1 -v $(go list ./... | grep -v /test/contract) 2>&1 | grep -c -- '--- SKIP'
```

Expected: every package passes. The count is exactly one higher than before the change, and the extra skip is the demonstrator.

## 3. Behavior against a running stack (SC-003)

```bash
make db-down
docker compose -f deployments/docker-compose.yml up -d --build
make test-contract
./test/quick_test.sh
make test-horizon-ui
```

Expected: the contract suite's passed/failed/skipped counts equal the baseline. The CLI suite and the Horizon probe pass.

Then check that startup still fails fast against an unreachable database (US1 scenario 2). Run the binary directly, because compose won't start the container while `postgres` is unhealthy (`depends_on: service_healthy`):

```bash
go build -o /tmp/lightstack-failfast ./cmd/lightstack
LIGHTSTACK_DB_URL='postgres://lightstack:secret@127.0.0.1:1/lightstack?sslmode=disable' timeout 20 /tmp/lightstack-failfast --config config/lightstack.yaml
echo "exit=$?"
```

Expected: `Failed to connect to database: failed to ping database: …` and `exit=1`, as before the change.

And readiness against a database that disappears (US1 scenario 3):

```bash
curl -s -w '\n%{http_code}\n' http://localhost:35357/readyz
docker compose -f deployments/docker-compose.yml stop postgres
curl -s -w '\n%{http_code}\n' http://localhost:35357/readyz
docker compose -f deployments/docker-compose.yml start postgres
```

Expected: `200` while the database is up, and `503` with reason `database unavailable` once it is stopped.

And a clean shutdown (US1 scenario 5):

```bash
docker compose -f deployments/docker-compose.yml stop lightstack
docker compose -f deployments/docker-compose.yml logs --tail 30 lightstack
docker inspect -f '{{.State.ExitCode}}' lightstack
docker compose -f deployments/docker-compose.yml start lightstack
```

Expected: the log runs from `Shutting down servers...` to `Lightstack stopped` with no error about a closed pool, and the exit code is `0`.

## 4. Real mode (constitution: both modes)

These run on the real-mode host, from [`HOST_TESTING_REAL_MODE.md`](HOST_TESTING_REAL_MODE.md). [`HOST_TESTING.md`](HOST_TESTING.md) replays §1–§3 and §5 as a stub-mode runbook. The real-mode checks are a server create with a network, a volume attach and detach, a delete, and `lightstack cleanup --dry-run`. Each behaves exactly as before the change.

## 5. Documentation (SC-005)

```bash
git grep -n 'database\.DB' -- CLAUDE.md docs/ARCHITECTURE.md docs/TROUBLESHOOTING.md docs/STUB_PLACEHOLDERS.md docs/WHATS_LEFT.md
```

Expected: no output. `CLAUDE.md`'s DI paragraph names a real constructor.
