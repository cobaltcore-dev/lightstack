# Host testing guide — SQLite backup, integrity check and restore (k3s, real-mode storage)

Validates cloud-storage#600 (SPEC-030) by hand on the shipped image: `lightstack-migrate backup` and `check` run inside a live pod, the documented Kubernetes restore through a node debug pod, the `pg_dump` refusal, the newer-database refusal and the `.recover` path. It is the host run task T025 calls for, and it proves FR-013 and SC-001 … SC-005. The automated tests cover the rest, including the wrong-order restore (`TestRestoreProcedure`).

Topology: one Ubuntu host with single-node **k3s**. Setup installs k3s if it is absent, and Teardown removes it again only if this runbook installed it. The chart is installed with **`realMode.enabled: true`**: backups are a real-mode operation (FR-004a), because only real mode mounts the whole node storage root. Every **service stays in stub mode** (no libvirt, KVM or networking). This feature touches only the database file and the storage mounts. The libvirt socket mount that real mode renders points at an empty placeholder directory. Everything lives under a dedicated root, **`/var/lib/lightstack-hosttest`**, so a real `/var/lib/lightstack` on the host is never touched.

**Run all commands from the repo root.** Paths like `charts/lightstack` and `build/package/Dockerfile` are repo-relative. Host paths (`/var/lib/lightstack-hosttest`, `/tmp/…`, `/etc/rancher/…`) are not.

**One terminal.** Run §1 and §2 in the **same terminal (T1)**: §1 sets the environment and defines the `pf` port-forward helper that §2 uses. Every section is **one block**. Its stages are announced in the output by `### N.M …` lines, and the **Pass =** / **Expect:** lists below each block are keyed by those stage numbers.

**Validates:**

| Acceptance criterion (spec / ticket) | Stage |
|---|---|
| `backup` takes a consistent copy while Lightstack serves requests (FR-001, SC-001) | 2.1 |
| Default target `<root>/backups/lightstack-<UTC>.db`; file `0600`, directory `0750`; no `-wal`/`-shm` (FR-002, FR-003a) | 2.1 |
| `backup` refuses an existing target (FR-003) | 2.1 |
| Backup copied off the node with `kubectl cp` (FR-011) | 2.1 |
| `check` exits 0 on the live database and non-zero on a damaged one (FR-005, SC-003) | 2.2 |
| On a PostgreSQL URL, `backup` and `check` exit non-zero naming `pg_dump` (FR-006, SC-004) | 2.3 |
| The documented Kubernetes restore, run over a database that left `-wal`/`-shm` behind, starts, passes `check` and serves the backup's data (FR-007, FR-009, FR-013, SC-002, SC-005) | 2.4 |
| A database from a newer Lightstack is refused, naming both versions (FR-008) | 2.5 |
| Recovery with `sqlite3 .recover` off the node (Alpine's; Ubuntu's cannot) yields a database that passes `check` (FR-012) | 2.6 |
| `check` refuses an empty file: no Lightstack schema (FR-005a) | 2.6 |

---

## 0. Prerequisites

Assumes **Docker or Podman** is installed and usable (to build the image); this block only verifies it. **k3s** is not needed yet: stage 1.2 installs it if it is absent, so a `not installed yet` line here is expected on a fresh host.

The block installs only the lightweight CLIs the tests invoke: `curl`, the OpenStack client, `sqlite3` (2.6 shows that this distribution build cannot run `.recover`, then runs Alpine's in a container) and `helm`. Re-running it is a no-op.

```bash
sudo apt-get update -qq && sudo apt-get install -y curl python3-openstackclient sqlite3
command -v helm >/dev/null || curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
echo "container engine:" && { docker --version 2>/dev/null || podman --version 2>/dev/null || echo "MISSING: install docker or podman first"; }
echo "k3s:" && { command -v k3s >/dev/null && k3s --version | head -1; } || echo "not installed yet (1.2 installs it)"
echo "versions:" && helm version --short && openstack --version 2>&1 | head -1 && sqlite3 --version | cut -d' ' -f1
```

**Pass =** a container engine, `helm`, `openstack` and `sqlite3` each print a version. k3s prints its version or `not installed yet (1.2 installs it)`. A `MISSING` line for the container engine means stop and install one first.

---

## 1. Setup steps

One block, five stages. `sudo -v` asks for your password once, up front. Every stage is safe to re-run, so after a failure you can fix the cause and paste the block again.

- **1.1 Client environment.** OpenStack credentials are the chart's: `admin` with the `adminPassword` set in 1.4 (`secret`), project `default`, domain `Default`. Keystone is reached through the port-forward on `localhost:35357` that 1.5 starts. `S` is the node storage root used throughout.
- **1.2 k3s.** Installs single-node k3s with the official installer if it is absent, leaving the system kubeconfig at its default `0600` and copying it to a user-owned `~/.kube/ls-hosttest.yaml` (mode `600`) so `kubectl` needs no `sudo` without making the cluster-admin credentials world-readable. The installer adds a system service (`k3s`) and its own containerd. When this stage does the install it leaves a marker at `/var/tmp/ls-hosttest-installed-k3s`, and Teardown removes k3s only if that marker exists. A k3s that was already here is used as is and never removed. Then it waits for the node to be `Ready` and reads its name.
- **1.3 Image.** Builds the branch's image, so the binaries under test are the shipped ones, and imports it into k3s's containerd under a `localhost/` name that both Docker and Podman produce. The first build takes a few minutes.
- **1.4 Chart.** Writes the values and installs release `ls`, pinned to the node. Real mode mounts all of `$S` (so `$S/backups` is on the node) and pre-creates its subdirectories. The libvirt socket directory real mode mounts is an empty placeholder, created here. The console proxy is off (stub services have no guests). `endpointHost: localhost` keeps the service catalog reachable through the port-forward. `dataRetentionPolicy` stays at the default, `retain`. The `rollout restart` makes a re-run pick up a rebuilt image, which keeps the same tag.
- **1.5 Port-forward and a first resource.** `pf` (re)starts a background `kubectl port-forward` and waits for Keystone. It is needed again after every pod recreation, because a forward dies with its pod. The network `before-backup` is the marker the restore in 2.4 must bring back.

```bash
sudo -v
echo "### 1.1 client environment"
export KUBECONFIG=$HOME/.kube/ls-hosttest.yaml
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
export S=/var/lib/lightstack-hosttest
export CFG=/app/config/lightstack.yaml
echo "root=$S"
echo "### 1.2 k3s, installed only if absent"
command -v k3s >/dev/null || { curl -sfL https://get.k3s.io | sudo sh -s - && sudo touch /var/tmp/ls-hosttest-installed-k3s; }
for i in $(seq 1 30); do sudo k3s kubectl get node >/dev/null 2>&1 && break; sleep 2; done
sudo k3s kubectl wait --for=condition=Ready node --all --timeout=120s
mkdir -p "$HOME/.kube" && sudo install -o "$USER" -m 600 /etc/rancher/k3s/k3s.yaml "$KUBECONFIG"
export NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
kubectl get nodes
test -f /var/tmp/ls-hosttest-installed-k3s && echo "k3s installed by this runbook (teardown will remove it)" || echo "k3s was already present (teardown leaves it)"
echo "### 1.3 build the image and import it into k3s"
CE=docker; command -v docker >/dev/null 2>&1 || CE=podman
$CE build -t localhost/lightstack:hosttest -f build/package/Dockerfile .
$CE save localhost/lightstack:hosttest | sudo k3s ctr images import -
sudo k3s ctr images ls -q | grep localhost/lightstack:hosttest
echo "### 1.4 install the chart: SQLite, real-mode storage, stub services"
sudo mkdir -p /run/ls-hosttest-libvirt
cat > /tmp/ls-hosttest-values.yaml <<YAML
image:
  repository: localhost/lightstack
  tag: hosttest
  pullPolicy: Never
endpointHost: localhost
secrets:
  jwtSecret: hosttest-jwt
  adminPassword: secret
database:
  url: sqlite://$S/db/lightstack.db
nodeSelector:
  kubernetes.io/hostname: $NODE
realMode:
  enabled: true
  hostStoragePath: $S
  libvirtSocketPath: /run/ls-hosttest-libvirt/libvirt-sock
consoleProxy:
  enabled: false
YAML
helm upgrade --install ls charts/lightstack -f /tmp/ls-hosttest-values.yaml
kubectl rollout restart deployment/ls-lightstack
kubectl rollout status deployment/ls-lightstack --timeout=180s
kubectl logs deploy/ls-lightstack | grep -i 'database ready' | head -1
sudo ls -l $S/db
echo "### 1.5 port-forward helper and a first resource"
pf() { kill $(cat /tmp/ls-pf.pid 2>/dev/null) 2>/dev/null; sleep 1; kubectl port-forward svc/ls-lightstack 35357 8774 9696 8776 9292 > /tmp/ls-pf.log 2>&1 & echo $! > /tmp/ls-pf.pid; for i in $(seq 1 30); do curl -sf http://localhost:35357/v3 >/dev/null 2>&1 && break; sleep 1; done; }
pf
openstack token issue -f value -c id | head -c 12; echo " ...token issued"
openstack network show before-backup -f value -c id 2>/dev/null || openstack network create before-backup -f value -c id
openstack network list -f value -c Name | grep -x before-backup
```

**Pass =**
- **1.1** prints `root=/var/lib/lightstack-hosttest`.
- **1.2** `kubectl wait` reports `condition met`, `kubectl get nodes` lists exactly one `Ready` node, and the last line says who installed k3s.
- **1.3** the build succeeds and the stage ends with `localhost/lightstack:hosttest`.
- **1.4** the rollout completes. The `database ready` log line names `sqlite` and `/var/lib/lightstack-hosttest/db/lightstack.db`. `$S/db` holds `lightstack.db` (plus `-wal`/`-shm` while it runs).
- **1.5** a token prefix prints, then the `before-backup` id, then `before-backup` from the listing.

---

## 2. Test steps

One block, six stages, run once from top to bottom in **T1**, the shell from §1. A stage that fails does not stop the block: read its **Expect** below, and see Troubleshooting.

- **2.1 Backup while serving.** A background loop keeps creating networks through the API (writes, not just reads) while `backup` runs in the pod. The block waits on that loop by its own PID (a bare `wait` would also wait for the port-forward, forever). Then it checks the file on the node, refuses a second backup onto the same file, and copies the backup off the node with `kubectl cp`.
- **2.2 Integrity check.** `check` runs against the serving database. Then a deliberately damaged copy of the backup, three overwritten pages, is copied into the pod and checked with the same binary.
- **2.3 PostgreSQL refusal.** The pod declares `LIGHTSTACK_DB_BACKEND=sqlite`, which would refuse a PostgreSQL URL before `backup` ever ran. `env -u` removes it for these commands, so the refusal tested is the one `backup` and `check` make themselves. The URL points at a closed port: both must refuse without connecting.
- **2.4 Restore over a database that left `-wal`/`-shm` behind.** This follows `docs/KUBERNETES_DEPLOYMENT.md` § Restore. It writes data **after** the backup (`after-backup`), which the restore must undo. Then it makes the stop look unclean. A graceful scale-down checkpoints the log and removes `-wal`, so the block copies the live `-wal`/`-shm` aside while Lightstack serves and puts them back after it stops: the state a killed pod leaves. The documented restore commands then run in a **node debug pod** from the Lightstack image, with the node's filesystem under `/host`. The docs show the interactive form (`-it … -- sh`, then the commands at its prompt). Here the same commands are the debug pod's command (`-- sh -c '…'`), so the step needs no second terminal; the pod, image, `/host` mount and binary are the same. The block waits for the pod to finish, prints its log, removes it, starts Lightstack, and reads the data back.
- **2.5 Newer backup refused.** This takes a copy of the backup inside the pod, stamps it one migration version ahead of this image (`force` asks for confirmation, so `yes` is piped in), and runs `lightstack-migrate up` on it. That is the same `MigrateUp` the server runs at startup, so the running deployment is not disturbed.
- **2.6 Recovery off the node, and the empty-file gate.** This host stands in for the operator's workstation, so `.recover` runs here, not in the image, which ships none. First the block shows that Ubuntu's own `sqlite3` cannot run it. Then it salvages the damaged copy from 2.2 with Alpine's `sqlite3` in a container, as the docs prescribe, and checks the result with the image's `check`. Last, it checks a 0-byte file, which an unguarded check would pass, since an empty file is a valid empty SQLite database.

```bash
echo "### 2.1 backup while serving, defaults, no overwrite, copy off the node"
( for i in $(seq 1 15); do openstack network create load-$i -f value -c id >/dev/null 2>&1 || echo REQ_FAIL; done; echo LOAD_DONE ) > /tmp/ls-load.log 2>&1 &
LOADPID=$!
sleep 2
kubectl exec deploy/ls-lightstack -- /app/lightstack-migrate backup -config $CFG | tee /tmp/ls-backup.out
wait $LOADPID
echo "load: $(grep -c REQ_FAIL /tmp/ls-load.log) failed requests, $(grep -c LOAD_DONE /tmp/ls-load.log) loop finished"
export B=$(sed -n 's/.*written to //p' /tmp/ls-backup.out)
echo "backup=$B"
sudo ls -la $S/backups
sudo stat -c '%a %n' $S/backups $B
kubectl exec deploy/ls-lightstack -- /app/lightstack-migrate backup -config $CFG $B; echo "second backup exit=$?"
POD=$(kubectl get pod -l app.kubernetes.io/name=lightstack,app.kubernetes.io/instance=ls -o jsonpath='{.items[0].metadata.name}')
kubectl cp $POD:$B /tmp/ls-hosttest-backup.db && ls -l /tmp/ls-hosttest-backup.db
echo "### 2.2 integrity check: live database, then a damaged copy"
kubectl exec deploy/ls-lightstack -- /app/lightstack-migrate check -config $CFG; echo "live check exit=$?"
openstack network list -f value -c Name | grep -c . | xargs echo "API still answering, networks:"
cp /tmp/ls-hosttest-backup.db /tmp/ls-bad.db && dd if=/dev/urandom of=/tmp/ls-bad.db bs=4096 seek=5 count=3 conv=notrunc status=none
kubectl cp /tmp/ls-bad.db $POD:/tmp/bad.db
kubectl exec deploy/ls-lightstack -- env LIGHTSTACK_DB_URL=sqlite:///tmp/bad.db /app/lightstack-migrate check -config $CFG; echo "damaged check exit=$?"
kubectl exec deploy/ls-lightstack -- rm -f /tmp/bad.db /tmp/bad.db-wal /tmp/bad.db-shm
echo "### 2.3 PostgreSQL refusal"
kubectl exec deploy/ls-lightstack -- env -u LIGHTSTACK_DB_BACKEND LIGHTSTACK_DB_URL=postgres://u:p@127.0.0.1:1/x /app/lightstack-migrate backup -config $CFG; echo "pg backup exit=$?"
kubectl exec deploy/ls-lightstack -- env -u LIGHTSTACK_DB_BACKEND LIGHTSTACK_DB_URL=postgres://u:p@127.0.0.1:1/x /app/lightstack-migrate check -config $CFG; echo "pg check exit=$?"
echo "### 2.4 restore through a node debug pod, over a database that left -wal/-shm behind"
openstack network create after-backup -f value -c id
openstack network list -f value -c Name | grep -xE 'before-backup|after-backup'
sudo cp $S/db/lightstack.db-wal /tmp/ls-stale-wal && sudo cp $S/db/lightstack.db-shm /tmp/ls-stale-shm
kill $(cat /tmp/ls-pf.pid) 2>/dev/null
kubectl scale deployment ls-lightstack --replicas=0
kubectl wait --for=delete pod -l app.kubernetes.io/name=lightstack,app.kubernetes.io/instance=ls --timeout=120s
sudo cp /tmp/ls-stale-wal $S/db/lightstack.db-wal && sudo cp /tmp/ls-stale-shm $S/db/lightstack.db-shm
sudo ls -l $S/db
kubectl get pods -o name | grep node-debugger | xargs -r kubectl delete
kubectl debug node/$NODE --image=localhost/lightstack:hosttest --image-pull-policy=Never -- sh -c 'H=/host/var/lib/lightstack-hosttest; rm -f $H/db/lightstack.db-wal $H/db/lightstack.db-shm; BK=$(ls -t $H/backups/lightstack-*.db | head -1); echo "restoring $BK"; cp "$BK" $H/db/lightstack.db; chmod 600 $H/db/lightstack.db; LIGHTSTACK_DB_URL=sqlite://$H/db/lightstack.db /app/lightstack-migrate check -config /app/config/lightstack.yaml; echo "restore check exit=$?"; ls -l $H/db'
sleep 3
DBG=$(kubectl get pods -o name | grep node-debugger | head -1)
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded $DBG --timeout=120s
kubectl logs $DBG
kubectl delete $DBG
kubectl scale deployment ls-lightstack --replicas=1
kubectl rollout status deployment/ls-lightstack --timeout=180s
pf
openstack network list -f value -c Name | grep -xE 'before-backup|after-backup'
openstack network list -f value -c Name | grep -qx after-backup && echo "FAIL: post-backup data came back" || echo "PASS: the backup's data, nothing written after it"
kubectl exec deploy/ls-lightstack -- /app/lightstack-migrate check -config $CFG
echo "### 2.5 a backup from a newer Lightstack is refused"
kubectl exec deploy/ls-lightstack -- sh -c 'cp $(ls -t /var/lib/lightstack-hosttest/backups/lightstack-*.db | head -1) /tmp/ahead.db'
V=$(kubectl exec deploy/ls-lightstack -- env LIGHTSTACK_DB_URL=sqlite:///tmp/ahead.db /app/lightstack-migrate status -config $CFG | awk '/Current Version/{print $3}'); echo "image is at version $V"
echo yes | kubectl exec -i deploy/ls-lightstack -- env LIGHTSTACK_DB_URL=sqlite:///tmp/ahead.db /app/lightstack-migrate force -config $CFG $((V+1))
kubectl exec deploy/ls-lightstack -- env LIGHTSTACK_DB_URL=sqlite:///tmp/ahead.db /app/lightstack-migrate up -config $CFG; echo "newer up exit=$?"
kubectl exec deploy/ls-lightstack -- rm -f /tmp/ahead.db /tmp/ahead.db-wal /tmp/ahead.db-shm /tmp/ahead.db.migrate-lock
echo "### 2.6 recovery off the node with Alpine's sqlite3 .recover, and an empty file refused"
CE=docker; command -v docker >/dev/null 2>&1 || CE=podman
sudo rm -rf /tmp/ls-recover && mkdir -p /tmp/ls-recover && cp /tmp/ls-bad.db /tmp/ls-recover/lightstack.db
sqlite3 /tmp/ls-recover/lightstack.db ".recover" > /dev/null 2> /tmp/ls-recover/ubuntu.err; echo "Ubuntu sqlite3 .recover says: $(head -1 /tmp/ls-recover/ubuntu.err)"
$CE run --rm -v /tmp/ls-recover:/w docker.io/library/alpine:3.24 sh -c 'umask 077 && apk add -q --no-cache sqlite && sqlite3 /w/lightstack.db ".recover" | sqlite3 /w/recovered.db && chown "$(stat -c %u:%g /w/lightstack.db)" /w/recovered.db'
ls -l /tmp/ls-recover/recovered.db
: > /tmp/ls-recover/empty.db
POD=$(kubectl get pod -l app.kubernetes.io/name=lightstack,app.kubernetes.io/instance=ls -o jsonpath='{.items[0].metadata.name}')
kubectl cp /tmp/ls-recover/recovered.db $POD:/tmp/recovered.db
kubectl cp /tmp/ls-recover/empty.db $POD:/tmp/empty.db
kubectl exec deploy/ls-lightstack -- env LIGHTSTACK_DB_URL=sqlite:///tmp/recovered.db /app/lightstack-migrate check -config $CFG; echo "recovered check exit=$?"
kubectl exec deploy/ls-lightstack -- env LIGHTSTACK_DB_URL=sqlite:///tmp/empty.db /app/lightstack-migrate check -config $CFG; echo "empty check exit=$?"
kubectl exec deploy/ls-lightstack -- sh -c 'rm -f /tmp/recovered.db /tmp/recovered.db-wal /tmp/recovered.db-shm /tmp/empty.db /tmp/empty.db-wal /tmp/empty.db-shm'
```

**Expect:**
- **2.1**
  - `✅ Backup written to /var/lib/lightstack-hosttest/backups/lightstack-<YYYYMMDDThhmmssZ>.db`, a UTC timestamp.
  - `load: 0 failed requests, 1 loop finished`. The API kept accepting writes during the backup.
  - `$S/backups` holds that **one** file, with **no** `-wal`/`-shm` beside it. `stat` prints `750 …/backups` and `600 …/lightstack-….db`.
  - The second backup prints `Backup failed: backup target … already exists; each backup needs a new file`, then `command terminated with exit code 1`, then `second backup exit=1`.
  - `/tmp/ls-hosttest-backup.db` exists on the host with a size above 1 MB (the full schema).
- **2.2**
  - `✅ Integrity check passed (schema version 74)` and `live check exit=0`, with the API still answering (the network count prints).
  - For the damaged copy: `Check failed: …` followed by either problem lines (`Tree … page …`, `row … missing from index …`) or `database disk image is malformed`, then `command terminated with exit code 1`, then `damaged check exit=1`.
- **2.3** `Backup failed: backup is only for SQLite; database.url selects postgres — use pg_dump` with `pg backup exit=1`, and `Check failed: check is only for SQLite; database.url selects postgres — use pg_dump` with `pg check exit=1`. Neither attempts a connection (no `connection refused`).
- **2.4**
  - Before the stop, both `before-backup` and `after-backup` are listed. After it, `kubectl wait` reports the pod deleted, and `$S/db` holds `lightstack.db`, `lightstack.db-wal` and `lightstack.db-shm`: the crash shape the procedure must handle.
  - The debug pod reaches `condition met`. Its log shows `restoring /host/var/lib/lightstack-hosttest/backups/lightstack-….db`, `✅ Integrity check passed (schema version 74)`, `restore check exit=0`, and an `ls` with `lightstack.db` at `-rw-------` and **no** `lightstack.db-wal`. (A `Permission denied` in this log means the debug profile cannot write the host path; see Troubleshooting, and report it, since the docs would need the flag.)
  - After the restart, the listing shows **only** `before-backup`, followed by `PASS: the backup's data, nothing written after it` and `✅ Integrity check passed (schema version 74)`. The restored database serves requests (FR-013, SC-002, SC-005). (`load-*` networks created during the backup may or may not be present; either is correct.)
- **2.5** `image is at version 74` (or this branch's newest), then `✅ Version forced to 75`. The `up` prints `Migration failed: the database is at migration version 75, but this Lightstack knows versions up to 74: it was written by a newer Lightstack. Run that version, or restore a backup this one wrote`, then `newer up exit=1`. It must not print `no migration found for version …`.
- **2.6**
  - `Ubuntu sqlite3 .recover says: sql error: no such table: sqlite_dbpage (1)`: the distribution build cannot salvage, which is why the docs use Alpine's.
  - The Alpine run prints at most a line such as `defensive off` (plus any warnings about the damaged pages), and `/tmp/ls-recover/recovered.db` is over 1 MB.
  - `✅ Integrity check passed (schema version 74)` with `recovered check exit=0`: what `.recover` salvaged holds the whole schema of its version (best effort; it would then be restored with 2.4's procedure, not repeated here). If the damage cost a table `.recover` could not rebuild, `check` instead lists what is missing and exits 1. That is also correct: such a file must not be restored.
  - For the 0-byte file: `Check failed: /tmp/empty.db holds no Lightstack schema: it is empty, an incomplete recovery, or not a Lightstack database`, then `command terminated with exit code 1`, then `empty check exit=1`.

---

## 3. Teardown steps

One block, two stages, in any terminal.

- **3.1** removes everything the run created inside the cluster and on the host: the port-forward, the release (with the default `retain` nothing is purged, so the state root is removed by hand), any leftover debug pod, the dedicated state root, the libvirt placeholder, the imported and built images, and the temporary files.
- **3.2** removes k3s, **only if stage 1.2 installed it** (its marker exists), with k3s's own uninstall script: it stops the service and removes the cluster, its containerd images, `/etc/rancher/k3s` and the `kubectl`/`crictl` links it created (links only, so a `kubectl` binary that was already installed stays). A k3s that was on the host before this runbook is left alone.

```bash
sudo -v
echo "### 3.1 release, state root, images and temporary files"
kill $(cat /tmp/ls-pf.pid 2>/dev/null) 2>/dev/null || true
export KUBECONFIG=$HOME/.kube/ls-hosttest.yaml
helm uninstall ls 2>/dev/null || true
kubectl get pods -o name 2>/dev/null | grep node-debugger | xargs -r kubectl delete
kubectl wait --for=delete pod -l app.kubernetes.io/instance=ls --timeout=120s 2>/dev/null || true
sudo rm -rf /var/lib/lightstack-hosttest /run/ls-hosttest-libvirt
sudo k3s ctr images rm localhost/lightstack:hosttest 2>/dev/null || true
{ docker rmi localhost/lightstack:hosttest || podman rmi localhost/lightstack:hosttest; } 2>/dev/null || true
rm -f /tmp/ls-hosttest-values.yaml /tmp/ls-pf.pid /tmp/ls-pf.log /tmp/ls-load.log /tmp/ls-backup.out /tmp/ls-hosttest-backup.db /tmp/ls-bad.db /tmp/ls-recovered.db
sudo rm -rf /tmp/ls-stale-wal /tmp/ls-stale-shm /tmp/ls-recover
echo "remaining:" && { helm list -q 2>/dev/null | grep -x ls; sudo ls -d /var/lib/lightstack-hosttest 2>/dev/null; echo "(nothing above = clean)"; }
rm -f "$KUBECONFIG"
echo "### 3.2 k3s, removed only if 1.2 installed it"
test -f /var/tmp/ls-hosttest-installed-k3s && { sudo /usr/local/bin/k3s-uninstall.sh && sudo rm -f /var/tmp/ls-hosttest-installed-k3s && echo "k3s removed"; } || echo "k3s was not installed by this runbook: left in place"
command -v k3s >/dev/null && echo "k3s still present" || echo "k3s absent"
```

**Pass =**
- **3.1** only `(nothing above = clean)` prints after `remaining:`.
- **3.2** either `k3s removed` then `k3s absent` (this runbook installed it), or `left in place` then `k3s still present` (it was there before).

Optional, only if this host did not have them before: `sudo apt-get remove -y python3-openstackclient sqlite3`, and the base images the build and 2.6 pulled: `podman rmi docker.io/library/golang:1-alpine3.24 docker.io/library/alpine:3.24` (or `docker rmi …`).

---

## Troubleshooting

- **Pod stuck in `ContainerCreating` with a hostPath error for the libvirt directory**: `/run/ls-hosttest-libvirt` is missing (for example after a reboot, since `/run` is a tmpfs). Re-run `sudo mkdir -p /run/ls-hosttest-libvirt`.
- **`ErrImageNeverPull`** (Lightstack pod or debug pod): the image is not in k3s's containerd. Re-run §1 (it is safe to repeat), and check `sudo k3s ctr images ls -q | grep hosttest`.
- **Permission denied writing under `/host` in the 2.4 debug pod log**: the default debug profile cannot write the host path on this cluster. Re-run that `kubectl debug` line with `--profile=sysadmin` added, and **report it**: the restore docs would then need `--profile=sysadmin`.
- **2.4's `kubectl wait` for the debug pod times out**: run `kubectl describe $DBG`. `Pending` means it was not scheduled; a failed image pull points at the `ErrImageNeverPull` entry above.
- **`openstack` hangs or `connection refused` after a scale or rollout**: the port-forward died with the old pod. Run `pf` again (in T1).
- **`load: N failed requests` in 2.1 with N > 0**: check `/tmp/ls-load.log` and `kubectl logs deploy/ls-lightstack`. A backup must not interrupt writes, so this is a finding, not noise.
