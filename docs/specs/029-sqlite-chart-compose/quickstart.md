# Quickstart: SQLite via the Helm chart and Docker Compose

**Feature**: `029-sqlite-chart-compose` · verifies SC-001 … SC-007

Prerequisites: a single-node k3s (for the chart path) or Docker + Docker Compose (for the Compose path), and the built `lightstack` image. The SQLite hostPath must be a **local POSIX filesystem** (not NFS/CephFS).

## A. Helm chart on k3s (stub mode)

```bash
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')

helm install ls charts/lightstack \
  --set image.repository=<registry>/lightstack \
  --set secrets.jwtSecret=dev-jwt \
  --set secrets.adminPassword=dev-pw \
  --set database.url='sqlite:///var/lib/lightstack/db/lightstack.db' \
  --set nodeSelector."kubernetes\.io/hostname"=$NODE
```

Verify:

```bash
kubectl get pods -l app.kubernetes.io/instance=ls          # one pod, Running; no postgres
kubectl get pvc  -l app.kubernetes.io/instance=ls          # none
kubectl get deploy ls-lightstack -o jsonpath='{.spec.strategy.type}'   # Recreate
kubectl exec deploy/ls-lightstack -- ls /var/lib/lightstack/db          # lightstack.db (+ -wal/-shm)
```

Survives a restart and an upgrade (SC-004):

```bash
kubectl delete pod -l app.kubernetes.io/instance=ls        # Recreate brings it back on the same db
helm upgrade ls charts/lightstack --reuse-values            # data survives
```

## B. Render-time refusals (SC-002, SC-003)

Each of these fails `helm template` with a message naming the problem:

```bash
# path outside the storage root
helm template charts/lightstack --set database.url='sqlite:///tmp/x.db' --set nodeSelector."kubernetes\.io/hostname"=n ...
# no node pin
helm template charts/lightstack --set database.url='sqlite:///var/lib/lightstack/db/lightstack.db' ...
# multi-node
helm template charts/lightstack --set database.url='sqlite:///var/lib/lightstack/db/lightstack.db' --set deploymentMode=multi-node ...
# replicas > 1
helm template charts/lightstack --set database.url='sqlite:///var/lib/lightstack/db/lightstack.db' --set replicaCount=2 ...
# sqlite url + Secret
helm template charts/lightstack --set database.url='sqlite:///var/lib/lightstack/db/lightstack.db' --set database.existingSecret=db ...
```

A SQLite URL hidden in a Secret (SC-003): give `database.existingSecret` a Secret holding a `sqlite://` URL while `database.url` is a `postgres://` URL — the chart renders `LIGHTSTACK_DB_BACKEND=postgres`, and the pod crash-loops with the binary's scheme-mismatch message.

## C. Teardown (SC-005, SC-007)

```bash
# delete: purges VMs and removes db/ last
helm uninstall ls            # with dataRetentionPolicy=delete (set at install)
kubectl debug node/$NODE -it --image=busybox -- ls /host/var/lib/lightstack/db   # gone

# retain (default): db/ stays, a reinstall resumes
helm uninstall ls            # with dataRetentionPolicy=retain
# reinstall on the same node → opens the existing database
```

Purge-scheduling parity (SC-007) is asserted by helm-unittest:

```bash
helm unittest charts/lightstack -f tests/teardown_test.yaml
```

## D. Docker Compose (SQLite)

```bash
docker compose -f deployments/docker-compose-sqlite.yml up -d
docker compose -f deployments/docker-compose-sqlite.yml ps      # no postgres service
source ~/.lightstack-env && openstack token issue               # works
docker compose -f deployments/docker-compose-sqlite.yml down    # db persists on the named volume
docker compose -f deployments/docker-compose-sqlite.yml up -d   # resumes from the same db
```

The default PostgreSQL stack is unchanged (SC-006):

```bash
docker compose -f deployments/docker-compose.yml up -d          # still runs postgres
```

## E. Unit gates (SC-006)

```bash
helm unittest charts/lightstack          # SQLite render + PostgreSQL-unchanged, both pass
go test ./internal/teardown/...          # db/ purged last, after the connection closes
```
