# Contract: documented restore and recovery procedures

**Feature**: `030-sqlite-backup-restore` (cloud-storage#600) | **Date**: 2026-10-28

These are **documented operator procedures** (FR-007, FR-009–FR-012), not code paths. They ship in `docs/KUBERNETES_DEPLOYMENT.md` (§ Backup, integrity check and restore) and `docs/DOCKER_DEPLOYMENT.md` (§ Backup and restore on SQLite); those are authoritative. The one code-enforced invariant they rely on is the newer-database refusal (see `cli-commands.md`). SQLite / real-mode only (FR-004a).

---

## Restore — the invariant that must not be skipped

Putting a backup in place without first removing the old database's `-wal`/`-shm` lets SQLite **replay the stale write-ahead log** onto the restored file, reinstating post-backup data (verified, research R10). Every documented restore therefore follows the same order:

1. **Stop** Lightstack (no process may hold the database).
2. **Delete** `lightstack.db-wal` and `lightstack.db-shm`.
3. **Put the backup in place** as `lightstack.db`.
4. **`lightstack-migrate check`** the restored file (exit 0 required).
5. **Start** Lightstack. An older backup is migrated forward on start; a newer backup is refused, naming both versions.

### Kubernetes (k3s) — node debug pod (FR-009)
The running container runs `/app/lightstack` directly (no shell), and a scaled-down Deployment leaves no pod mounting the node storage, so the file surgery uses a node debug pod:

```
kubectl scale deployment <release>-lightstack --replicas=0
kubectl wait --for=delete pod -l app.kubernetes.io/name=lightstack,app.kubernetes.io/instance=<release>
kubectl debug node/<node> -it --image=<lightstack-image> -- sh   # host fs under /host; give sh, or the image's CMD runs
  # in the debug pod:
  rm -f /host<hostStoragePath>/db/lightstack.db-wal /host<hostStoragePath>/db/lightstack.db-shm
  cp <backup> /host<hostStoragePath>/db/lightstack.db
  LIGHTSTACK_DB_URL=sqlite:///host<hostStoragePath>/db/lightstack.db /app/lightstack-migrate check -config /app/config/lightstack.yaml
kubectl delete pod <debug-pod>
kubectl scale deployment <release>-lightstack --replicas=<n>
```

### Docker Compose (FR-010) — the real-mode KVM stack
`deployments/docker-compose-kvm.yml` mounts the host state root at the same path; its config is `/opt/lightstack/config/lightstack.yaml`. Stub-mode SQLite stacks are not backed up (FR-004a).
```
docker compose -f deployments/docker-compose-kvm.yml stop lightstack
docker compose -f deployments/docker-compose-kvm.yml run --rm --entrypoint sh lightstack
  # in the one-off container (same mounts):
  rm -f /var/lib/lightstack/db/lightstack.db-wal /var/lib/lightstack/db/lightstack.db-shm
  cp /var/lib/lightstack/backups/<backup> /var/lib/lightstack/db/lightstack.db
  /app/lightstack-migrate check -config /opt/lightstack/config/lightstack.yaml
docker compose -f deployments/docker-compose-kvm.yml start lightstack
```

---

## Taking a backup (FR-011)

Backups are taken **while the service runs** — a plain exec, no downtime:

- **Kubernetes**: `kubectl exec deploy/<release>-lightstack -- /app/lightstack-migrate backup -config /app/config/lightstack.yaml`, then `kubectl cp <pod>:<hostStoragePath>/backups/<file> ./`.
- **Docker Compose** (KVM stack): `docker compose -f deployments/docker-compose-kvm.yml exec lightstack /app/lightstack-migrate backup -config /opt/lightstack/config/lightstack.yaml`; the file lands on the host under `<state_path>/backups`.

---

## Recovery beyond the latest backup (FR-012)

1. Restore the latest good backup by the procedure above.
2. If damage goes beyond it, salvage with the SQLite `.recover` command **off the node**, on a copy of the damaged file (with its `-wal`/`-shm`). Use Alpine's `sqlite3`: Debian's and Ubuntu's builds lack the `sqlite_dbpage` table `.recover` needs and leave an empty file (found in the host run).
   ```
   podman run --rm -v "$PWD":/w docker.io/library/alpine:3.24 sh -c 'apk add -q sqlite && sqlite3 /w/lightstack.db ".recover" | sqlite3 /w/recovered.db'
   ```
   The Lightstack image ships **no** `sqlite3` (research R13). Restore the recovered file by the procedure above; its `check` refuses a file with no Lightstack schema (FR-005a), such as the empty result of a failed `.recover`.

---

## Acceptance hooks
- The Kubernetes restore is exercised once on k3s during host validation; the restored database serves requests (FR-013).
- A test proves the correct order reads back the backup's data even when the source left `-wal`/`-shm` behind (SC-002).
