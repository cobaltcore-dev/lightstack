# Host testing guide — database-pool injection (stub mode)

This runbook validates SPEC-027 (cloud-storage#554) in stub mode: the process-wide database handle is gone (SC-001, SC-005), and nothing observable changed (FR-005, FR-010, SC-003, SC-004). It replays the gates recorded while implementing, so each **Expect** line carries the result the pre-change tree gave at `44ed3d1`. The real-mode paths are covered separately by [`HOST_TESTING_REAL_MODE.md`](HOST_TESTING_REAL_MODE.md).

Topology: one Ubuntu host running the compose stack, which is PostgreSQL, Redis and Lightstack built from this branch. The unit tests use a separate `lightstack_test` database on the same PostgreSQL, the way CI does.

**Run all commands from the repo root.** Every shell block is zsh-copy-paste-safe.

**Validates**

| Criterion | Section |
|---|---|
| SC-001: no reference to the process-wide handle is left | 2.1 |
| SC-003: unit tests with and without a test database, and lint | 2.2 |
| SC-004: the Keystone demonstrator passes on its own | 2.3 |
| SC-003: contract suite, CLI suite and Horizon probe match the baseline | 2.4 |
| US1 scenario 2: startup fails fast against an unreachable database | 2.5 |
| US1 scenario 3: readiness follows the database | 2.6 |
| US1 scenario 4: `cleanup --dry-run` with the database up and down | 2.7 |
| US1 scenario 5: clean shutdown | 2.8 |
| SC-005: current docs no longer show the handle | 2.9 |

**Shell T1** runs everything. Keep it open from §1.2 on, because it carries `TESTDB`.

## 0. Prerequisites

The container engine (Docker, or Podman with `podman-docker` and a compose provider) and Go are preconditions. This block only checks them. It installs the lightweight CLI tools the tests call: `jq`, `curl`, and the OpenStack client that `test/quick_test.sh` drives.

**T1:**
```bash
sudo apt-get update
sudo apt-get install -y jq curl make
command -v openstack >/dev/null || sudo apt-get install -y python3-openstackclient
command -v docker >/dev/null || command -v podman >/dev/null || echo "install docker or podman first"
docker compose version >/dev/null 2>&1 || echo "docker compose is not available"
command -v go >/dev/null || echo "install Go first"
echo "versions:"; { docker --version || podman --version; }; docker compose version; go version; openstack --version; jq --version; make --version | head -1
```

**Pass =** every tool prints a version, and nothing prints `install … first` or `not available`.

## 1. Setup steps

### 1.1 T1 — build and start the stack from this branch

`make db-up` uses the same `lightstack-postgres` container name and port 5432, so it must not be running.

**T1:**
```bash
make db-down
docker compose -f deployments/docker-compose.yml up -d --build
for i in $(seq 1 60); do curl -sf http://localhost:35357/readyz >/dev/null && break; sleep 2; done
curl -s -w ' %{http_code}\n' http://localhost:35357/readyz
docker compose -f deployments/docker-compose.yml logs lightstack | grep -E 'Database connection established|Lightstack started'
```

**Pass =** `/readyz` answers `ok 200`, and the log shows `Database connection established` and `Lightstack started successfully`.

### 1.2 T1 — create the unit-test database

**T1:**
```bash
docker compose -f deployments/docker-compose.yml exec -T postgres dropdb -U lightstack --if-exists lightstack_test
docker compose -f deployments/docker-compose.yml exec -T postgres createdb -U lightstack lightstack_test
export TESTDB='postgres://lightstack:secret@localhost:5432/lightstack_test?sslmode=disable'
echo "$TESTDB"
```

**Pass =** `createdb` returns without error and `TESTDB` is set. Keep this shell.

## 2. Test steps

### 2.1 T1 — the handle is gone

**T1:**
```bash
git grep -n 'database\.DB\b' -- '*.go' | wc -l
git grep -n -E 'DB = pool|^var DB ' -- internal/database | wc -l
go build ./...
go vet ./...
```

**Expect:** both counts are `0` (the first was `874` before the change), and the build and vet print nothing.

### 2.2 T1 — unit tests with and without a test database, and lint

`-p 1` keeps packages from racing on the seeded project, so the results are comparable run to run.

**T1:**
```bash
LIGHTSTACK_TEST_DB_URL="$TESTDB" go test -race -count=1 -p 1 $(go list ./... | grep -v /test/contract) 2>&1 | grep -E '^(ok|FAIL|---)'
env -u LIGHTSTACK_TEST_DB_URL go test -count=1 -p 1 -v $(go list ./... | grep -v /test/contract) > /tmp/spec027-nodb.txt 2>&1
grep -c -E '^(FAIL|--- FAIL)' /tmp/spec027-nodb.txt
grep -c -- '--- SKIP' /tmp/spec027-nodb.txt
make lint
```

**Expect:**
- **With the test database:** every package prints `ok` (15 with tests), and nothing prints `FAIL` — the same as before the change.
- **Without it:** `0` failures and **`127`** skips. That's `126` before the change, plus the new demonstrator, which skips without a database.
- **Lint:** `0 issues.`

### 2.3 T1 — the Keystone demonstrator on its own pool

`TestServiceRunsOnItsOwnPool` opens its own pool and never touches the package's `testPool`.

**T1:**
```bash
LIGHTSTACK_TEST_DB_URL="$TESTDB" go test -count=1 -run TestServiceRunsOnItsOwnPool -v ./internal/keystone/ 2>&1 | grep -E -- '--- (PASS|FAIL|SKIP)|^ok'
```

**Expect:** `--- PASS: TestServiceRunsOnItsOwnPool`.

### 2.4 T1 — contract suite, CLI suite and Horizon probe

**T1:**
```bash
make test-contract > /tmp/spec027-contract.txt 2>&1
echo "PASS=$(grep -c -- '--- PASS:' /tmp/spec027-contract.txt) FAIL=$(grep -c -- '--- FAIL:' /tmp/spec027-contract.txt) SKIP=$(grep -c -- '--- SKIP:' /tmp/spec027-contract.txt)"
./test/quick_test.sh 2>&1 | tail -6
make test-horizon-ui 2>&1 | tail -4
```

**Expect:** `PASS=302 FAIL=0 SKIP=10`, the CLI suite's `Total Passed: 17` / `Total Failed: 0`, and the probe's `Passed: 54   Failed: 0`. All of these equal the baseline.

### 2.5 T1 — startup fails fast against an unreachable database

Run the binary directly. Compose won't start the container while `postgres` is unhealthy (`depends_on: service_healthy`), so it can't show this case.

**T1:**
```bash
go build -o /tmp/spec027-lightstack ./cmd/lightstack
LIGHTSTACK_DB_URL='postgres://lightstack:secret@127.0.0.1:1/lightstack?sslmode=disable' timeout 20 /tmp/spec027-lightstack --config config/lightstack.yaml 2>&1 | grep -o 'Failed to connect to database: failed to ping database'
LIGHTSTACK_DB_URL='postgres://lightstack:secret@127.0.0.1:1/lightstack?sslmode=disable' timeout 20 /tmp/spec027-lightstack --config config/lightstack.yaml >/dev/null 2>&1
echo "exit=$?"
```

**Expect:** `Failed to connect to database: failed to ping database` and `exit=1`, the same as the pre-change binary.

### 2.6 T1 — readiness follows the database

**T1:**
```bash
curl -s -w ' %{http_code}\n' http://localhost:35357/readyz
docker compose -f deployments/docker-compose.yml stop postgres
sleep 2
curl -s -w ' %{http_code}\n' http://localhost:35357/readyz
docker compose -f deployments/docker-compose.yml start postgres
for i in $(seq 1 60); do curl -sf http://localhost:35357/readyz >/dev/null && break; sleep 2; done
curl -s -w ' %{http_code}\n' http://localhost:35357/readyz
```

**Expect:** `ok 200`, then `database unavailable 503` while PostgreSQL is stopped, then `ok 200` again.

### 2.7 T1 — `cleanup --dry-run` with the database up and down

The cleanup job opens a pool of its own. Dry-run enumerates and logs, and deletes nothing.

**T1:**
```bash
docker compose -f deployments/docker-compose.yml exec -T lightstack /app/lightstack cleanup --config /app/config/lightstack.yaml --dry-run > /tmp/spec027-cleanup-up.txt 2>&1
echo "db up exit=$?"
grep -c 'class summary' /tmp/spec027-cleanup-up.txt
docker compose -f deployments/docker-compose.yml stop postgres
docker compose -f deployments/docker-compose.yml exec -T lightstack /app/lightstack cleanup --config /app/config/lightstack.yaml --dry-run > /tmp/spec027-cleanup-down.txt 2>&1
echo "db down exit=$?"
grep -c 'database unreachable' /tmp/spec027-cleanup-down.txt
docker compose -f deployments/docker-compose.yml start postgres
for i in $(seq 1 60); do curl -sf http://localhost:35357/readyz >/dev/null && break; sleep 2; done
```

**Expect:** `db up exit=0` with `3` class summaries (compute, network, storage). Then `db down exit=0`, with `1` `database unreachable` line: the host-only fallback.

### 2.8 T1 — clean shutdown

**T1:**
```bash
docker compose -f deployments/docker-compose.yml stop lightstack
docker compose -f deployments/docker-compose.yml logs --tail 30 lightstack | grep -E 'Shutting down servers|background goroutines stopped|Lightstack stopped|panic|closed pool'
docker inspect -f '{{.State.ExitCode}}' lightstack
docker compose -f deployments/docker-compose.yml start lightstack
for i in $(seq 1 60); do curl -sf http://localhost:35357/readyz >/dev/null && break; sleep 2; done
```

**Expect:**
- the log shows `Shutting down servers...`, then `Nova background goroutines stopped`, then `Cinder background goroutines stopped`, then `Lightstack stopped`;
- no `panic` and no `closed pool` lines;
- exit code `0`, and the stack comes back up afterwards.

### 2.9 T1 — the docs describe the pattern as built

**T1:**
```bash
git grep -n 'database\.DB' -- CLAUDE.md docs/ARCHITECTURE.md docs/TROUBLESHOOTING.md docs/STUB_PLACEHOLDERS.md docs/WHATS_LEFT.md | wc -l
grep -c 'nova.NewService(pool' CLAUDE.md
```

**Expect:** `0`, then `1`. The DI paragraph names a real constructor.

## 3. Teardown steps

The stack's PostgreSQL data lives in a compose volume. `down` keeps it. Add `-v` only on a host where that data is throwaway.

**T1:**
```bash
docker compose -f deployments/docker-compose.yml down
rm -f /tmp/spec027-nodb.txt /tmp/spec027-contract.txt /tmp/spec027-cleanup-up.txt /tmp/spec027-cleanup-down.txt /tmp/spec027-lightstack
unset TESTDB
```

**Pass =** `docker ps` no longer lists `lightstack`, `lightstack-postgres` or `lightstack-redis`.

## 4. Collect results (paste back)

Run this at the end of §2, before teardown.

**T1:**
```bash
echo "== handle refs: $(git grep -n 'database\.DB\b' -- '*.go' | wc -l)"
echo "== no-DB skips: $(grep -c -- '--- SKIP' /tmp/spec027-nodb.txt)"
echo "== contract: PASS=$(grep -c -- '--- PASS:' /tmp/spec027-contract.txt) FAIL=$(grep -c -- '--- FAIL:' /tmp/spec027-contract.txt) SKIP=$(grep -c -- '--- SKIP:' /tmp/spec027-contract.txt)"
echo "== cleanup up/down: $(grep -c 'class summary' /tmp/spec027-cleanup-up.txt) / $(grep -c 'database unreachable' /tmp/spec027-cleanup-down.txt)"
```

## Troubleshooting

- **Port 5432 already in use**: a `make db-up` container or another stack holds it. `make db-down`, then `docker compose -f deployments/docker-compose.yml down`.
- **A package fails only without `-p 1`**: the known seeded-project race between parallel packages. It's unrelated to this feature; rerun with `-p 1`.
- **Lint reports paths under a deleted worktree**: `go tool golangci-lint cache clean`, then rerun `make lint`.
- **Did this fail before the change too?** Rebuild the stack from the pre-change tree in a scratch clone (`git clone --shared . /tmp/spec027-base`, then `git -C /tmp/spec027-base checkout 44ed3d1`) and repeat the step there.
