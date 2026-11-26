# Quickstart: verifying the squash and the per-release migrations

The walk-through follows the user stories in order. It assumes a PostgreSQL started with
`make db-up` (`postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable`) and a scratch
directory for SQLite files. Each step names the outcome to see.

```bash
export PG='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'
export SQ="sqlite:///tmp/ls-596/t.db"
mkdir -p /tmp/ls-596
```

## 1. The baseline equals the history (US1)

```bash
LIGHTSTACK_TEST_DB_URL="$PG" go test -count=1 ./internal/database -run 'TestBaselineEquivalence|TestSchemaParity|TestMigrationsRoundTrip'
LIGHTSTACK_TEST_DB_URL="$SQ" go test -count=1 ./internal/database -run 'TestBaselineEquivalence|TestMigrationsRoundTrip'
```

**Outcome**: all pass. The equivalence test reports the number of tables it compared (more than 50) and no
difference.

## 2. A fresh database starts at 001

```bash
make db-down && make db-up
LIGHTSTACK_DB_URL="$PG" go run ./cmd/lightstack-migrate up -migrations migrations
LIGHTSTACK_DB_URL="$PG" go run ./cmd/lightstack-migrate status -migrations migrations
```

**Outcome**: `Current Version: 1`, `Migration: baseline`, `Status: Clean`.

## 3. A pre-squash database is refused (US2)

Make the database look like one a pre-squash Lightstack migrated: golang-migrate's version table at `059`, and no
`applied_migrations`. Then point the new build at it:

```bash
make db-down && make db-up
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL); INSERT INTO schema_migrations VALUES (59, false);"
LIGHTSTACK_DB_URL="$PG" go run ./cmd/lightstack-migrate up -migrations migrations
LIGHTSTACK_DB_URL="$PG" go run ./cmd/lightstack-migrate status -migrations migrations
```

**Outcome**: `up` exits 1 with **"from before the migration squash"**, naming version 59 and the way out,
"Recreate the database". It does not say "written by a newer Lightstack". `status` prints `Status: PRE-SQUASH` with
the same message and exits 1. `make db-down && make db-up` recreates the database.

`TestDatabaseState` (`internal/database`) and `TestStartupRefusals` (`cmd/lightstack`) cover the same refusal on
both backends.

## 4. Fragments, the open release migration and the guard (US3, US4)

```bash
make migration-fragment TICKET=596 NAME=quickstart-demo
# Write into the new up file:   ALTER TABLE volumes ADD COLUMN demo TEXT;
# Write into the new down file: ALTER TABLE volumes DROP COLUMN demo;
LIGHTSTACK_DB_URL="$PG" go run ./cmd/lightstack-migrate up -migrations migrations
LIGHTSTACK_DB_URL="$PG" go run ./cmd/lightstack-migrate status -migrations migrations
```

**Outcome**: the log names the open release migration `002` as unreleased. `status` shows
`Migration: open release migration 002 (unreleased)`.

Now change the fragment, for example add a second column, and run `up` again.

**Outcome**: exit 1 with **"applied an earlier state of the open release migration"**. `make db-down && make
db-up`, then `up`: it applies cleanly.

## 5. Freeze a release (US5)

```bash
make migrations-freeze RELEASE=v0.7.0-quickstart
ls migrations/ migrations/next/
LIGHTSTACK_DB_URL="$PG" go run ./cmd/lightstack-migrate status -migrations migrations
```

**Outcome**:
- `002_v0.7.0-quickstart.up.sql` and `.down.sql` exist, and `next/` holds no `.sql` file.
- The database that applied the open `002` reports `Migration: release v0.7.0-quickstart` and starts without being
  recreated (FR-021).
- `make migrations-freeze RELEASE=v0.7.0-quickstart` again refuses, exit 1: `release v0.7.0-quickstart already has
  migration 002_v0.7.0-quickstart.up.sql`. A release that has been frozen once is never frozen again.

Revert the demo files afterwards. They must never reach `main`.

## 6. The CI checks, locally

```bash
go test -count=1 ./internal/database -run TestMigrationsFolder
```

**Outcome**: passes on the real folder. Then create `migrations/003_demo.up.sql` and its down file, and run it
again.

**Outcome**: it fails with **"schema changes go into fragments"**. Remove the files.

The immutability step is the `git diff` from [contracts/ci-and-scripts.md](contracts/ci-and-scripts.md). Run it
against `origin/main` after editing a line of `001_baseline.postgres.up.sql`.

**Outcome**: it lists the file. Revert the edit.

## 7. Gates

```bash
make fmt && make lint
LIGHTSTACK_TEST_DB_URL="$PG" go test -race ./...
LIGHTSTACK_TEST_DB_URL="$SQ" go test -race ./...
docker compose -f deployments/docker-compose.yml up -d && ./test/quick_test.sh
```

Run both contract suites as CI does, on PostgreSQL and on SQLite (`contract-tests`, `contract-tests-sqlite`).
