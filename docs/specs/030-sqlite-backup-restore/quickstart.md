# Quickstart: SQLite backup, integrity check and restore

**Feature**: `030-sqlite-backup-restore` (cloud-storage#600) | **Date**: 2026-10-28

Verify the feature end to end on a single-node SQLite deployment. Assumes SPEC-028 + SPEC-029 (SQLite backend and chart) are in place.

## 0. Build

```bash
make build          # bin/lightstack
go build -o bin/lightstack-migrate ./cmd/lightstack-migrate
```

## 1. A SQLite database, serving

Point the config at SQLite and start the server (dev/stub is fine for the CLI checks):

```bash
# config/lightstack.yaml -> database.url: sqlite:///var/lib/lightstack/db/lightstack.db
#                           state_path:  /var/lib/lightstack
./bin/lightstack --config config/lightstack.yaml &
```

## 2. Online backup (FR-001/002/003/003a)

```bash
# default location, timestamped, while the server serves:
./bin/lightstack-migrate backup
# -> ✅ Backup written to /var/lib/lightstack/backups/lightstack-<UTC>.db

ls -l /var/lib/lightstack/backups/           # file is 0600
stat -c '%a %n' /var/lib/lightstack/backups  # dir is 750

# refuses to overwrite:
B=$(ls /var/lib/lightstack/backups/*.db | head -1)
./bin/lightstack-migrate backup "$B"; echo "exit=$?"   # non-zero, names the target
```

## 3. Integrity check (FR-005) — online

```bash
./bin/lightstack-migrate check; echo "exit=$?"          # exit 0 on the live DB, no downtime

# damaged file exits non-zero:
cp "$B" /tmp/bad.db
dd if=/dev/urandom of=/tmp/bad.db bs=4096 seek=5 count=3 conv=notrunc
LIGHTSTACK_DB_URL=sqlite:///tmp/bad.db ./bin/lightstack-migrate check; echo "exit=$?"   # non-zero, lists problems
```

## 4. PostgreSQL guard (FR-006)

```bash
LIGHTSTACK_DB_URL=postgres://lightstack:secret@localhost:5432/lightstack ./bin/lightstack-migrate backup; echo "exit=$?"  # non-zero, names pg_dump
LIGHTSTACK_DB_URL=postgres://lightstack:secret@localhost:5432/lightstack ./bin/lightstack-migrate check;  echo "exit=$?"  # non-zero, names pg_dump
```

## 5. Restore round-trip (SC-002) — the WAL pitfall

```bash
# write more data after the backup, then stop the server (leaving the DB as-is)
# ... create a resource via openstack CLI so the live DB diverges from the backup ...
kill %1    # stop lightstack

DB=/var/lib/lightstack/db/lightstack.db
rm -f "$DB-wal" "$DB-shm"     # MANDATORY — else the stale WAL is replayed
cp "$B" "$DB"
./bin/lightstack-migrate check && echo "restored OK"
./bin/lightstack --config config/lightstack.yaml &   # starts; serves the backup's data
```

Skipping the `rm -f …-wal …-shm` step reinstates the post-backup data — that is the bug the step prevents.

## 6. Newer-backup refusal (FR-008)

```bash
# simulate a database created by a newer Lightstack: force it one version ahead
./bin/lightstack-migrate force $(( $(./bin/lightstack-migrate status | awk '/Current Version/{print $3}') + 1 ))
./bin/lightstack --config config/lightstack.yaml; echo "exit=$?"   # refuses: "the database is at migration version N, but this Lightstack knows versions up to M"
```

## 7. Kubernetes restore (FR-013) — on k3s

Follow `contracts/restore-recovery.md`: `kubectl exec … backup` + `kubectl cp` to take a backup; then `kubectl scale --replicas=0`, `kubectl debug node/<node>` (host fs under `/host`) to delete `-wal`/`-shm`, copy the backup to `<hostStoragePath>/db/lightstack.db`, run `/app/lightstack-migrate check`, delete the debug pod, and scale back up. Confirm the restored database serves requests.

## Done when
- A backup is taken with the server serving; the file is `0600`, the dir `0750`; a second backup to the same path is refused.
- `check` exits 0 live and non-zero on a damaged file.
- `backup` and `check` on a PostgreSQL URL exit non-zero naming `pg_dump`.
- A backup restored after deleting `-wal`/`-shm` starts, passes `check`, and serves the backup's data.
- A database forced ahead of the binary is refused at start.
- The k3s restore via a node debug pod serves requests.
