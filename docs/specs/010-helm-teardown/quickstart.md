# Quickstart: Clean teardown & pristine redeploy

**Feature**: SPEC-010 | **Refs**: spec.md (US1/US2/US3), SC-001/002/003/004

How an operator tears a release down with **no state leakage** and redeploys onto a pristine baseline. Single-node (the validated scope); multi-node host purge is tracked in [#52](https://github.com/cobaltcore-dev/o3k/issues/52).

> ⚠️ **`dataRetentionPolicy: delete` is irreversible.** It destroys this release's libvirt VMs, networks, node-local storage, PVCs, **and resets the configured database's O3K schema**. It does **not** touch networked backends (Ceph RBD / S3) — those stay operator-owned. The default `retain` keeps everything.

## 1. Default uninstall (retain) — unchanged

```bash
helm uninstall my-o3k -n o3k
```
Removes only the release's Kubernetes workloads/Service/Secret/Ingress. Database, node-local `hostPath` data, and backends are left intact (SPEC-008 SC-006). Nothing new here.

## 2. Destructive teardown (delete)

Enable the purge and supply a kubectl image (required when `delete`):

```bash
helm upgrade my-o3k charts/o3k -n o3k --reuse-values \
  --set dataRetentionPolicy=delete \
  --set teardown.kubectlImage=registry.example.com/kubectl:1.31

helm uninstall my-o3k -n o3k        # blocks on the pre-delete hooks
```

What happens, in order (you can watch the hook Jobs):
1. **Quiesce** — the Deployment scales to 0; pods terminate (VMs survive — they're host processes). Release PVCs are deleted.
2. **Purge** (blocking) — `o3k cleanup` destroys/undefines libvirt domains, removes `light-ns-*`/`qrouter-*` namespaces + routes/bridges/TAP/iptables/VXLAN, deletes node-local volume/image files under the host storage path, then **resets the database schema** last.
3. Only after the purge **succeeds** does Helm remove the release. If it fails, **uninstall aborts** with the Job's error (FR-001) — inspect:
   ```bash
   kubectl logs -n o3k job/my-o3k-o3k-teardown-purge
   ```

## 3. Immediate redeploy — pristine (SC-002)

```bash
helm install my-o3k charts/o3k -n o3k -f my-values.yaml   # same name/node/DB
helm test my-o3k -n o3k                                   # identity check passes
openstack server list                                     # empty — no stale resources
```
No name collisions, no leftover namespaces, no resurrected networks (the DB was reset).

## 4. Verify zero residue (SC-001)

On the node and in the cluster, all should be empty for the release:
```bash
kubectl get all,pvc -n o3k -l app.kubernetes.io/instance=my-o3k   # nothing
sudo ip netns list | grep -E 'light-ns-|qrouter-'                  # nothing (for this release)
sudo virsh list --all                                              # no release domains
sudo ls /var/lib/o3k/volumes /var/lib/o3k/images                   # empty
```

## 5. Escape hatch — cleanup is impossible (C5)

If the node is gone or the purge cannot complete and you must remove the release anyway:
```bash
helm uninstall my-o3k -n o3k --no-hooks    # skips the purge; removes the release
```
You are then responsible for any orphaned host state. (`helm uninstall` takes no value overrides, so this — not a value — is the override.)

## Notes & limits

- **Multi-node**: in DaemonSet mode the purge only covers the node the hook lands on; full multi-node host purge is **deferred to #52**. Don't rely on `delete` for multi-node cleanup yet.
- **Networked backends**: RBD/S3 volumes/images are **not** deleted (operator-owned, C4). Reclaim them yourself if desired.
- **Stub mode**: `delete` still resets the DB and removes workloads/PVCs; there's no host state to purge.
- **Idempotent**: re-running a teardown (retry, partial prior run) is safe — already-absent artifacts are treated as success.
