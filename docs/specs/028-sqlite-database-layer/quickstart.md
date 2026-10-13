# Quickstart: SQLite backend, part 1

How to run Lightstack on SQLite and verify each success criterion. Commands run from the repository root; stub mode unless noted. PostgreSQL comes from `make db-up` (URL in the `Makefile`: `postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable`). A non-root user must point `state_path` at a writable directory — the default `/var/lib/lightstack` needs root — as CI does (`.github/workflows/ci.yml`).

## 1. Run on SQLite (US1)

```bash
make build
mkdir -p /tmp/ls-quick && rm -rf /tmp/ls-quick/db
LIGHTSTACK_DB_URL=sqlite:///tmp/ls-quick/db/lightstack.db ./bin/lightstack --config config/lightstack.yaml
```

Expect: the directory and file are created, migrations run (baseline 073, then 074), and one `database ready` line appears — fields `backend:"sqlite"`, `journal_mode:"wal"`, `synchronous:"full"`, `foreign_keys:true`, ending `…,"time":"…","message":"database ready"}` (zerolog writes `message` last). In a second shell:

```bash
stat -c '%a %n' /tmp/ls-quick/db /tmp/ls-quick/db/lightstack.db*   # 750 for the directory, 600 for every file
./bin/lightstack healthcheck; echo "ready=$?"   # 0: /readyz on the identity port reached the database
source ~/.lightstack-env
openstack server list
```

## 2. Refused configurations (US3, SC-003)

```bash
# An empty database.url must be set in the config: an empty LIGHTSTACK_DB_URL override is ignored.
sed 's|^  url: .*|  url: ""|' config/lightstack.yaml > /tmp/ls-empty.yaml
./bin/lightstack --config /tmp/ls-empty.yaml; echo "exit=$?"
for u in 'mysql://x/y' 'host=db user=x' 'sqlite://relative.db' 'sqlite:///tmp/x.db?mode=ro'; do
  LIGHTSTACK_DB_URL="$u" ./bin/lightstack --config config/lightstack.yaml; echo "exit=$?"
done
LIGHTSTACK_DB_BACKEND=postgres LIGHTSTACK_DB_URL=sqlite:///tmp/ls-quick/db/lightstack.db ./bin/lightstack --config config/lightstack.yaml; echo "exit=$?"
LIGHTSTACK_DB_BACKEND=mysql ./bin/lightstack --config config/lightstack.yaml; echo "exit=$?"
```

Expect: each exits non-zero within a second with the message from [contracts/configuration-and-startup.md](contracts/configuration-and-startup.md); nothing new under `/tmp`. For the multi-node guard, set `neutron.vxlan_enabled: true` in a copy of the config.

## 3. Unit tests on both backends (SC-004)

```bash
make db-up
pkgs=$(go list ./... | grep -v /test/contract)   # as CI and `make test` do
LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' go test -count=1 $pkgs
LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/lightstack-test.db' go test -count=1 $pkgs
go test -count=1 ./internal/database -run 'TestSQLConstructs|TestTranslate|TestSQLiteSettings|TestSQLiteOpen'
```

`-count=1` matters: Go's test cache does not see the variable that `TestMain` reads. Each package gets its own SQLite file next to the given path.

## 4. Schema parity and the baseline (FR-026, FR-028)

```bash
LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' \
  go test -count=1 ./internal/database -run TestSchemaParity
# Regenerate the baseline only when moving it to a newer migration on main:
LIGHTSTACK_TEST_DB_URL='postgres://…' go test -count=1 ./internal/database -run TestGenerateSQLiteBaseline -update
git diff --diff-filter=MDR --name-only main -- migrations/   # only the four corrected down files of FR-025 (SC-002)
```

## 5. Concurrency (FR-039, SC-005/006/007/010)

```bash
LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/c.db' go test -v -count=1 ./internal/... -run 'Concurren|TwoMigrators|NoTransactionHeld|SweepInterrupted'
LIGHTSTACK_TEST_DB_URL='postgres://…' go test -count=1 ./internal/... -run 'Concurren|SweepInterrupted'
```

`-v` is needed because the mode × case table is printed with `t.Log`. The SQLite runs execute every
case with `deferred` and with `immediate` transactions; research.md records the chosen mode and busy wait.

## 6. Contract and quick suites against SQLite (SC-001)

With the server from step 1 running:

```bash
cd test/contract && LIGHTSTACK_DB_URL=sqlite:///tmp/ls-quick/db/lightstack.db go test -count=1 ./... ; cd -
./test/quick_test.sh
```

The 6 contract tests that seeded through `docker exec … psql` now seed through `LIGHTSTACK_DB_URL`, so the same command works against the Compose PostgreSQL with its URL.

## 7. Migrations tool (US5)

```bash
make migrate DB_URL=sqlite:///tmp/ls-mig/db/lightstack.db
LIGHTSTACK_DB_URL=sqlite:///tmp/ls-mig/db/lightstack.db go run ./cmd/lightstack-migrate status
LIGHTSTACK_DB_URL=sqlite:///tmp/ls-mig/db/lightstack.db go run ./cmd/lightstack-migrate goto 50   # refused, names baseline 073
make migrate                                                                                       # PostgreSQL, through lightstack-migrate
```

## 8. Quota refusal and reservations (FR-032 – FR-035a)

```bash
PROJECT=$(openstack token issue -f value -c project_id)                # ~/.lightstack-env sets no OS_PROJECT_ID
openstack image create --disk-format qcow2 --container-format bare cirros   # if none exists yet
openstack quota set --instances "$(openstack server list -f value | wc -l)" "$PROJECT"
openstack server create --flavor m1.tiny --image cirros quota-probe    # HTTP 403, "Quota exceeded for instances: …"
openstack volume create --size 1 --image cirros v-from-image           # completes; listed as "creating" while it runs
```

## 9. Real mode (quality gate)

On a real-mode host, repeat steps 1 and 8 with `libvirt_mode: real`, `networking_mode: iptables` and `cinder.storage_mode: local`, then once more on PostgreSQL — `deployments/docker-compose-kvm.yml` runs either backend (FR-042), and [HOST_TESTING.md](HOST_TESTING.md) is the runbook. Kill Lightstack while a large image-backed volume is created, restart it, and check the volume is `error` and its file is gone (FR-033).
