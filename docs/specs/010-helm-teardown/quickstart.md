# Quickstart: Clean teardown & pristine redeploy

**Feature**: SPEC-010 | **Refs**: spec.md (US1/US2/US3), SC-001/002/003/004

How an operator tears a release down with **no state leakage** and redeploys onto a pristine baseline. Single-node (the validated scope); multi-node host purge is tracked in [#52](https://github.com/cobaltcore-dev/lightstack/issues/52).

> ⚠️ **`dataRetentionPolicy: delete` is irreversible.** It destroys this release's libvirt VMs, all host network state, all node-local storage under the host storage path, and release PVCs. It does **not** touch the **database** (operator-owned — read to enumerate, never reset) or networked backends (Ceph RBD / S3). The default `retain` keeps everything.

## 1. Default uninstall (retain) — unchanged

```bash
helm uninstall my-lightstack -n lightstack
```
Removes only the release's Kubernetes workloads/Service/Secret/Ingress. Database, node-local `hostPath` data, and backends are left intact (SPEC-008 SC-006). Nothing new here.

## 2. Destructive teardown (delete)

Enable the purge and supply a kubectl image (required when `delete`):

```bash
helm upgrade my-lightstack charts/lightstack -n lightstack --reuse-values \
  --set dataRetentionPolicy=delete \
  --set teardown.kubectlImage=registry.example.com/kubectl:1.31

helm uninstall my-lightstack -n lightstack        # blocks on the pre-delete hooks
```

What happens, in order (you can watch the hook Jobs):
1. **Quiesce** — the Deployment scales to 0; pods terminate (VMs survive — they're host processes). Release PVCs are deleted.
2. **Purge** (blocking) — `lightstack cleanup` reads the operator-owned DB read-only to enumerate, then: destroys + undefines the libvirt domains; removes **all host network state** (egress SNAT, floating-IP DNAT + the `/32` on the uplink, ingress security-group filters, `lightstack-SG-*` chains, metadata DNAT, dnsmasq, host bridges, router + tenant namespaces); and `RemoveAll`s **every node-local tree** under the host storage path (`instances/`, `cloud-init/`, `images/`, `volumes/`, `dhcp/`). The **database is never written** — it is operator-owned.
3. Only after the purge **succeeds** does Helm remove the release. If it fails, **uninstall aborts** with the Job's error (FR-001) — inspect:
   ```bash
   kubectl logs -n lightstack job/my-lightstack-lightstack-teardown-purge
   ```

## 3. Immediate redeploy — pristine (SC-002)

```bash
helm install my-lightstack charts/lightstack -n lightstack -f my-values.yaml   # same name/node/DB
helm test my-lightstack -n lightstack                                   # identity check passes
```
The control plane comes up healthy with no name/port collisions and no leftover Kubernetes objects or host state (SC-002).

> **Resource pristineness needs a clean database.** Teardown leaves the operator-owned DB intact, so the redeployed services still *list* the prior VMs/networks while their host disks and domains are gone. For a truly empty `openstack server list`, reset the database yourself before reinstalling (it is the operator's lifecycle, not teardown's):
> ```bash
> psql "$LIGHTSTACK_DB_URL" -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'   # Lightstack re-migrates on next start
> ```

## 4. Verify zero residue (SC-001)

On the node and in the cluster, all should be empty for the release:
```bash
kubectl get all,pvc -n lightstack -l app.kubernetes.io/instance=my-lightstack    # nothing
sudo virsh list --all                                              # no release domains
sudo ip netns list | grep -E 'light-ns-|qrouter-'                  # nothing
ip -br link show type bridge | grep '^br-'                         # no lightstack bridges
sudo iptables -S | grep 'lightstack-'                                     # no lightstack chains
sudo ls /var/lib/lightstack/{instances,volumes,images,cloud-init,dhcp}    # removed (RemoveAll'd)
```
(The **database** is intentionally **not** in this list — it is operator-owned and deliberately retained.)

## 5. Escape hatch — cleanup is impossible (C5)

If the node is gone or the purge cannot complete and you must remove the release anyway:
```bash
helm uninstall my-lightstack -n lightstack --no-hooks    # skips the purge; removes the release
```
You are then responsible for any orphaned host state. (`helm uninstall` takes no value overrides, so this — not a value — is the override.)

## Notes & limits

- **Multi-node**: in DaemonSet mode the purge only covers the node the hook lands on; full multi-node host purge is **deferred to #52**. Don't rely on `delete` for multi-node cleanup yet.
- **Networked backends**: RBD/S3 volumes/images are **not** deleted (operator-owned, C4). Reclaim them yourself if desired.
- **Stub mode**: `delete` removes workloads/PVCs and runs `lightstack cleanup` as a host no-op (nothing to purge); the DB is read-only enumerated, never reset.
- **Idempotent**: re-running a teardown (retry, partial prior run) is safe — already-absent artifacts are treated as success.
