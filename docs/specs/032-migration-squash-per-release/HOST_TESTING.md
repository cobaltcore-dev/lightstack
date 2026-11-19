# Host testing guide — SPEC-032 migration squash and per-release migrations (stub mode)

End-to-end host validation of [cloud-storage#596](https://github.com/cobaltcore-dev/cloud-storage/issues/596):
the 83 migrations are squashed into one baseline, `001`, per backend; a database from before the squash is
refused with the way out; schema changes are fragments that assemble into one open release migration, which
a release freezes. This runs the real binaries and a real PostgreSQL against the success criteria.

**Topology.** One host. Lightstack built from this checkout runs in **stub mode** (`config/lightstack.yaml`,
with only `state_path` redirected) against a throwaway **PostgreSQL 18 container on port 55496** or a
**SQLite file** under `/tmp/ls596-host`. A database from before the squash is stood in for by its version
record alone: a `schema_migrations` table at 59 and no `applied_migrations`, which is all the refusal reads.
**Run all commands from the repo root.** Setup writes `/tmp/ls596-host/env`, and every later block sources it,
so each block works in a fresh shell. A block that needs the server starts it in the background and stops it
at its end: one shell is enough. The steps build on each other, so run them in order.

| Validates | Spec | Section |
|---|---|---|
| The baseline builds what the 83 migrations built, on both backends | US1, SC-001 | §2.1 |
| A new PostgreSQL database starts at `001` and serves | US1, FR-006, SC-006, SC-007 | §2.2 |
| A missing SQLite file reports `NO DATABASE`; a new one starts at `001` and serves | US1, FR-022 | §2.3 |
| A pre-squash database is refused by the server and by `lightstack-migrate`, naming the way out | US2, FR-011, SC-003 | §2.4 |
| Fragments assemble into the open release migration, logged as unreleased | US3, FR-015, FR-018, SC-006 | §2.5 |
| A changed fragment makes the database stale, refused until recreated | US4, FR-019, FR-020, SC-006 | §2.6 |
| A release freezes its migration, and the database carries on | US5, FR-021, FR-023, FR-024, SC-005 | §2.7 |
| A hand-numbered file is refused; merges in either order; immutability | FR-025, FR-026, SC-002, SC-003 | §2.8 |

---

## 0. Prerequisites

Required tools. The runbook installs nothing:

- **Go 1.27.3 or newer** (`go.mod` says `go 1.27.3`): builds both binaries and runs the database tests in §2.1
  and §2.8.
- **make**: `make build`, `make migration-fragment`.
- **git**: the immutability test in §2.8 builds temporary repositories.
- **Docker or Podman**: either one runs the PostgreSQL container, and the steps use whichever is present.
- **curl**: `test/quick_test.sh` talks HTTP with it; the check below also uses it to make sure nothing
  answers on port 35357 yet.
- **bash**, **timeout**, **seq**: the test scripts, bounding a start that must be refused, wait loops.

Lightstack's ports (35357, 8774, 9696, 8776, 9292, 8775) and 55496 must be free: stop any other Lightstack
first.

```bash
missing=0
for t in go make git curl bash timeout seq; do command -v $t >/dev/null || { echo "MISSING: $t"; missing=1; }; done
command -v docker >/dev/null || command -v podman >/dev/null || { echo "MISSING: docker or podman"; missing=1; }
curl -s -o /dev/null --max-time 2 http://localhost:35357/ && { echo "BUSY: something already answers on port 35357"; missing=1; }
go version
docker --version 2>/dev/null || podman --version
[ "$missing" = 0 ] && echo "PREREQS OK" || echo "PREREQS FAILED: install the missing tools above, then re-run"
```

**Pass =** `PREREQS OK`, no `MISSING:` or `BUSY:` lines, and Go reports `go1.27.3` or newer. Install anything
reported missing yourself, or stop whatever holds port 35357, then re-run the block.

---

## 1. Setup steps

One block does the setup:
- `make build` builds `bin/lightstack` and `bin/lightstack-migrate`.
- It writes the configuration (`state_path` moved to `/tmp/ls596-host/state`) and a scratch copy of
  `migrations/` for the fragment steps, so the checkout's own `migrations/next/` stays untouched.
- It writes the env file with the JWT secret, the endpoint host and three helpers:
  - `ls_start URL [MIGRATIONS]` starts a server in the background and waits until it is ready. The server
    writes its own PID to `/tmp/ls596-host/server.pid`;
  - `ls_stop` stops exactly that process;
  - `lsm URL COMMAND [MIGRATIONS]` runs `lightstack-migrate`.
- It starts PostgreSQL and creates the three databases the steps use. `old` gets the version record of a
  database from before the squash.

```bash
mkdir -p /tmp/ls596-host/state
make build
sed -e 's|^.*state_path: .*|state_path: /tmp/ls596-host/state|' config/lightstack.yaml > /tmp/ls596-host/lightstack.yaml
cp -r migrations /tmp/ls596-host/migrations
cat > /tmp/ls596-host/env <<'ENV'
export LSH=/tmp/ls596-host
export ENGINE=$(command -v docker >/dev/null 2>&1 && echo docker || echo podman)
export PG=postgres://lightstack:secret@localhost:55496
export LIGHTSTACK_JWT_SECRET=host-test-only LIGHTSTACK_ENDPOINT_HOST=localhost
ls_start() {
  LIGHTSTACK_DB_URL="$1" nohup sh -c 'echo $$ > /tmp/ls596-host/server.pid; exec "$@"' sh ./bin/lightstack --config ${LSH}/lightstack.yaml --migrations ${2:-migrations} > ${LSH}/server.log 2>&1 &
  for i in $(seq 30); do ./bin/lightstack healthcheck >/dev/null 2>&1 && echo SERVER UP && return 0; sleep 2; done
  echo SERVER NOT UP; tail -n 5 ${LSH}/server.log; return 1
}
ls_stop() {
  [ -f ${LSH}/server.pid ] || return 0
  kill $(cat ${LSH}/server.pid) 2>/dev/null
  for i in $(seq 15); do kill -0 $(cat ${LSH}/server.pid) 2>/dev/null || break; sleep 1; done
  rm -f ${LSH}/server.pid
}
lsm() { LIGHTSTACK_DB_URL="$1" ./bin/lightstack-migrate $2 -config ${LSH}/lightstack.yaml -migrations ${3:-migrations}; }
ENV
. /tmp/ls596-host/env
$ENGINE run -d --name ls596-host-pg -e POSTGRES_USER=lightstack -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=lightstack -p 55496:5432 docker.io/library/postgres:18
until $ENGINE exec ls596-host-pg pg_isready -h 127.0.0.1 -U lightstack -q; do sleep 1; done
for db in fresh old frag; do $ENGINE exec ls596-host-pg psql -q -U lightstack -d lightstack -c "CREATE DATABASE $db"; done
$ENGINE exec ls596-host-pg psql -q -U lightstack -d old -c 'CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)' -c 'INSERT INTO schema_migrations VALUES (59, false)'
ls bin/lightstack bin/lightstack-migrate ${LSH}/env >/dev/null && echo SETUP OK
```

**Pass =** the container ID prints, and the block ends with `SETUP OK`.

---

## 2. Test steps

### 2.1 The baseline builds what the history built

The equivalence tests apply the 83 pre-squash migrations and the baseline to scratch databases and compare
tables, columns, keys, indexes, sequences and seed rows. The round trip goes up, down and up again; the NOT
NULL test proves the five `project_id` columns refuse a missing project on SQLite as on PostgreSQL.

```bash
. /tmp/ls596-host/env
LIGHTSTACK_TEST_DB_URL="${PG}/lightstack?sslmode=disable" go test -count=1 -v ./internal/database -run 'TestBaselineEquivalence|TestSchemaParity|TestMigrationsRoundTrip|TestBaselineProjectIDNotNull' 2>&1 | grep -E '^(--- |ok|FAIL)'
mkdir -p ${LSH}/sqlite-tests
LIGHTSTACK_TEST_DB_URL="sqlite://${LSH}/sqlite-tests/t.db" go test -count=1 -v ./internal/database -run 'TestBaselineEquivalence|TestMigrationsRoundTrip|TestBaselineProjectIDNotNull' 2>&1 | grep -E '^(--- |ok|FAIL)'
```

**Expect:** PostgreSQL: `--- PASS` for all five: `TestBaselineEquivalencePostgres`, `TestBaselineEquivalenceSQLite`,
`TestBaselineProjectIDNotNull`, `TestMigrationsRoundTrip` and `TestSchemaParity`, then `ok`. SQLite:
`--- SKIP: TestBaselineEquivalencePostgres` (it needs PostgreSQL), `--- PASS` for the other three, then `ok`. No
`FAIL`.

### 2.2 A new PostgreSQL database starts at 001 and serves

```bash
. /tmp/ls596-host/env
lsm "${PG}/fresh?sslmode=disable" status
ls_start "${PG}/fresh?sslmode=disable"
lsm "${PG}/fresh?sslmode=disable" status
./test/quick_test.sh | tail -n 4
ls_stop
```

**Expect:**
1. Before the start: `Current Version: 0`, `Migration: none (empty database)`, `Status: Clean`.
2. `SERVER UP`.
3. After the start: `Current Version: 1`, `Migration: baseline`, `Status: Clean`.
4. The quick test ends `Total Passed: 17`, `Total Failed: 0`, `✓ All tests passed!`.

The JSON `Migration files validated` lines are the folder check every command runs first.

### 2.3 SQLite: a missing file, then a new one that starts at 001 and serves

```bash
. /tmp/ls596-host/env
lsm "sqlite://${LSH}/sqlite/lightstack.db" status
echo "status exit $?"
ls -d ${LSH}/sqlite 2>/dev/null || echo "status created nothing"
ls_start "sqlite://${LSH}/sqlite/lightstack.db"
lsm "sqlite://${LSH}/sqlite/lightstack.db" status
./test/quick_test.sh | tail -n 4
ls_stop
```

**Expect:**
1. `Status: NO DATABASE`, then `SQLite database /tmp/ls596-host/sqlite/lightstack.db does not exist: up and the server
   create it at their first run. …`, then `status exit 1` and `status created nothing`.
2. Then `SERVER UP`, and status reports `Current Version: 1`, `Migration: baseline`, `Status: Clean`.
3. The quick test ends `Total Passed: 17`, `Total Failed: 0`.

### 2.4 A pre-squash database is refused, naming the way out

`old` records version 59 and has no `applied_migrations`, as a database a Lightstack from before the squash
migrated. The server must refuse it at start, and `status` must report it, both without touching it.

```bash
. /tmp/ls596-host/env
LIGHTSTACK_DB_URL="${PG}/old?sslmode=disable" timeout 30 ./bin/lightstack --config ${LSH}/lightstack.yaml --migrations migrations > ${LSH}/refused.log 2>&1
echo "server exit $?"
grep -o 'Failed to run migrations: the database is at migration version 59, from before the migration squash' ${LSH}/refused.log
lsm "${PG}/old?sslmode=disable" status
echo "status exit $?"
$ENGINE exec ls596-host-pg psql -q -U lightstack -d old -tAc 'SELECT version, dirty FROM schema_migrations'
```

**Expect:**
1. `server exit 1`, then the grep prints `Failed to run migrations: the database is at migration version 59, from before
   the migration squash`. It never says "newer Lightstack".
2. Status prints `Current Version: 59`, `Status: PRE-SQUASH` and the message `… Recreate the database. See
   docs/UPGRADING.md, "The migration squash"`, then `status exit 1`.
3. The record is untouched: `59|f`.

### 2.5 A fragment becomes the open release migration

`make migration-fragment` runs against the scratch copy of the folder (`-C`), as it would against `migrations/`.

```bash
. /tmp/ls596-host/env
make -s -C ${LSH} -f "$PWD/Makefile" migration-fragment TICKET=596 NAME=hosttest-demo
up=$(ls ${LSH}/migrations/next/*-596-hosttest-demo.up.sql)
printf 'ALTER TABLE volumes ADD COLUMN hosttest_demo TEXT;\n' > $up
printf 'ALTER TABLE volumes DROP COLUMN hosttest_demo;\n' > ${up%.up.sql}.down.sql
ls_start "${PG}/frag?sslmode=disable" ${LSH}/migrations
grep -o 'applied the open release migration 002 (unreleased)' ${LSH}/server.log
ls_stop
lsm "${PG}/frag?sslmode=disable" status ${LSH}/migrations
```

**Expect:**
1. Two paths, `…/next/<UTC time>-596-hosttest-demo.up.sql` and `.down.sql`, then `SERVER UP`.
2. The grep prints `applied the open release migration 002 (unreleased)` (the server's warning, FR-018).
3. Status: `Current Version: 2`, `Migration: open release migration 002 (unreleased)`, `Status: Clean`.

### 2.6 A changed fragment makes the database stale

```bash
. /tmp/ls596-host/env
up=$(ls ${LSH}/migrations/next/*-596-hosttest-demo.up.sql)
printf 'ALTER TABLE volumes ADD COLUMN hosttest_demo TEXT;\nALTER TABLE volumes ADD COLUMN hosttest_more TEXT;\n' > $up
printf 'ALTER TABLE volumes DROP COLUMN hosttest_more;\nALTER TABLE volumes DROP COLUMN hosttest_demo;\n' > ${up%.up.sql}.down.sql
LIGHTSTACK_DB_URL="${PG}/frag?sslmode=disable" timeout 30 ./bin/lightstack --config ${LSH}/lightstack.yaml --migrations ${LSH}/migrations > ${LSH}/stale.log 2>&1
echo "server exit $?"
grep -o 'applied an earlier state of the open release migration 002 (unreleased)' ${LSH}/stale.log
lsm "${PG}/frag?sslmode=disable" status ${LSH}/migrations
$ENGINE exec ls596-host-pg psql -q -U lightstack -d lightstack -c 'DROP DATABASE frag' -c 'CREATE DATABASE frag'
ls_start "${PG}/frag?sslmode=disable" ${LSH}/migrations
ls_stop
```

**Expect:**
1. `server exit 1`, and the grep prints `applied an earlier state of the open release migration 002 (unreleased)`.
2. Status: `Current Version: 2`, `Migration: open release migration 002 (unreleased)`, `Status: STALE`, and the
   message ending `Recreate the database`.
3. After the database is recreated, `SERVER UP`: it applies the changed fragment cleanly.

### 2.7 A release freezes its migration, and the database carries on

`freeze` runs from a directory with no `config/`: it needs neither a configuration nor a database.

```bash
. /tmp/ls596-host/env
B=$PWD/bin
( cd ${LSH} && $B/lightstack-migrate freeze -migrations migrations v0.7.0-hosttest )
ls ${LSH}/migrations/002_*
ls -A ${LSH}/migrations/next
lsm "${PG}/frag?sslmode=disable" status ${LSH}/migrations
ls_start "${PG}/frag?sslmode=disable" ${LSH}/migrations
ls_stop
( cd ${LSH} && $B/lightstack-migrate freeze -migrations migrations v0.7.0-hosttest )
echo "freeze exit $?"
```

**Expect:**
1. `froze 2 fragment files into release migration 002 (v0.7.0-hosttest): 002_v0.7.0-hosttest.down.sql,
   002_v0.7.0-hosttest.up.sql`.
2. Both files are listed, and `next/` holds only `.gitkeep`.
3. Status: `Current Version: 2`, `Migration: release v0.7.0-hosttest`, `Status: Clean`. The database that applied the
   open state is not stale (FR-021), and `SERVER UP` follows.
4. The second freeze: `Freeze failed: release v0.7.0-hosttest already has migration 002_v0.7.0-hosttest.up.sql` and
   `freeze exit 1`.

### 2.8 Folder rules, merges in either order and immutability

```bash
. /tmp/ls596-host/env
cp -r migrations ${LSH}/numbered
printf 'SELECT 1;\n' > ${LSH}/numbered/002_add_demo.up.sql
printf 'SELECT 1;\n' > ${LSH}/numbered/002_add_demo.down.sql
lsm "${PG}/fresh?sslmode=disable" status ${LSH}/numbered
echo "status exit $?"
LIGHTSTACK_TEST_DB_URL="${PG}/lightstack?sslmode=disable" go test -count=1 ./internal/database -run 'TestMigrationsFolder|TestValidateMigrations|TestFragmentsMergeInEitherOrder|TestFragmentsApplyInOrder' 2>&1 | tail -n 1
bash test/migrations_immutable_test.sh | tail -n 1
bash test/deploy_single_node_test.sh | tail -n 1
```

**Expect:**
1. `Migration validation failed: 002_add_demo.down.sql: a numbered migration other than the baseline or a release
   migration; schema changes go into fragments (migrations/next/)` and `status exit 1`.
2. `ok  github.com/cobaltcore-dev/lightstack/internal/database`. These are the merge of two parallel fragments in both
   orders (SC-002) and the per-fragment replay CI runs.
3. The two scripts print `passed: 6, failed: 0` and `passed: 4, failed: 0`. They cover the immutability check (a
   changed baseline or release migration fails) and the deploy script's build and migration steps.

---

## 3. Teardown steps

Stops any server left running, removes the PostgreSQL container and everything under `/tmp/ls596-host`. The
binaries `make build` wrote to `bin/` are ignored by git; `make clean` removes them too, if you want.

```bash
. /tmp/ls596-host/env 2>/dev/null
[ -f /tmp/ls596-host/server.pid ] && kill $(cat /tmp/ls596-host/server.pid) 2>/dev/null
${ENGINE:-docker} rm -f ls596-host-pg 2>/dev/null || true
rm -rf /tmp/ls596-host
echo TEARDOWN DONE
```

**Pass =** `TEARDOWN DONE`; `docker ps -a` (or `podman ps -a`) no longer lists `ls596-host-pg`.

---

## Troubleshooting

- **`PREREQS FAILED` with `BUSY: … 35357`**: another Lightstack is running (Compose, systemd, `make run`). Stop it.
  Every server here binds the same ports.
- **`Error: … name "ls596-host-pg" is already in use`, or port 55496 taken**: a previous run was not torn down.
  Run §3, then §1 again.
- **`SERVER NOT UP`**: the last lines of `/tmp/ls596-host/server.log` print below it. Read more with
  `tail -n 50 /tmp/ls596-host/server.log`.
- **Re-running §2.5 to §2.7**: they change the scratch copy of `migrations/` and the `frag` database. Run §3 and
  §1 again first.
- **Job-control lines** such as `[1] 12345` and `[1] + terminated nohup …` are zsh reporting the background server.
  They are expected.
