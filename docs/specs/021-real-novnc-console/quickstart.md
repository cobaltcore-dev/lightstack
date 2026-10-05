# Quickstart: a real console / noVNC (SPEC-021)

How to bring the console up and prove it works. The full host runbook comes later (`/cl.hosttest`); this is the short path plus the probes that isolate a failure to one side.

## Stub mode — the console is expected to refuse

Stub mode has no hypervisor and no guest, so there is nothing to display and no proxy is deployed.

```bash
docker compose -f deployments/docker-compose.yml up -d
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
openstack console url show <server>
```

**Expected**: a refusal naming stub mode (HTTP `501`), *not* a URL. A URL here means FR-027 regressed.

The serial fallback must still work:

```bash
openstack console log show <server>
```

## Real mode — the console shows a guest

Prerequisites: the SPEC-018 real harness — Lightstack on k3s in real mode via `charts/lightstack`, Horizon via `charts/horizon` (relocated by this feature), Ceph/RGW via Rook.

```bash
helm upgrade --install lightstack charts/lightstack --namespace lightstack --create-namespace \
  --set image.repository=<registry>/lightstack --set image.tag=<tag> \
  --set secrets.jwtSecret=<jwt-secret> --set secrets.adminPassword=<admin-password> \
  --set database.url='postgres://<user>:<password>@<db-host>:5432/lightstack?sslmode=disable' \
  --set deploymentMode=single-node \
  --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true \
  --set nova.libvirtMode=real \
  --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables \
  --set glance.storageMode=local --set cinder.storageMode=local \
  --set endpointHost=<node-ip> \
  --set nova.consoleProxyBaseUrl=http://<node-ip>:30680
```

`realMode.enabled=true` grants the pod libvirt, host networking and host storage, and it
is what deploys the console proxy — but it does not make the services real: each keeps
its own mode, and every one defaults to `stub`. A stub Glance stores no image bytes, so
the guest fails to boot with a checksum mismatch. `endpointHost` keeps the catalog from
advertising `localhost`, which Horizon would then call from inside its own pod. The
real-mode runbook's Setup (`HOST_TESTING_REAL_MODE.md`) is the verified version of this
command, with every flag explained.

`nova.consoleProxyBaseUrl` is an **origin only** — no path. The client page and token query are appended by Lightstack (FR-020), so `/vnc_auto.html` must not appear here; that page does not exist in noVNC 1.6.0.

Boot an instance, then:

```bash
openstack server create --flavor m1.small --image cirros --network <net> vm1
openstack console url show vm1
```

Open the URL. **Expected**: the cirros framebuffer, and typing reaches the guest — log in and run a command (SC-001, SC-002).

## Probes that isolate a failure

Console failures are ambiguous by nature — "Unable to load Console" is what F1 already looked like. Each probe below narrows the cause to one component.

### Is the console socket real?

```bash
sudo virsh dumpxml <domain> | grep -A2 "<graphics"
```

Must show `socket='/var/lib/lightstack/console-sockets/<instance-uuid>.sock'`, a matching `<listen type='socket' socket='…'/>`, and `sharePolicy='force-shared'`, with no `port=` at all. Two other results mean the domain was defined by an older build (see `docs/UPGRADING.md`), and the console is refused as misconfigured:
- a socket under `/var/lib/libvirt/qemu/domain-<id>-<name>/` (defined before 2026-10-01);
- a `port=` (defined before the socket console).

```bash
sudo stat -c '%U:%G %a' /var/lib/lightstack/console-sockets   # expect libvirt-qemu:kvm 750 (QEMU's user)
```

A root-owned directory means Lightstack could not set the owner at startup. Its log names the cause, and guest starts are refused until it is fixed (FR-017a).

### Did Lightstack write a projection entry?

```bash
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list     # --json for structured output
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens purge    # drop expired entries by hand
```

(The binary is at `/app/lightstack` in the image, not on `$PATH`. On a host install it is
just `lightstack console-tokens list`.)

One entry per live token, each naming its guest's socket from the check above. If the list is empty right after a successful `console url show`, the failure is Lightstack-side. If entries accumulate past their expiry, the reconciler is not running — expiry is **not** enforced without it, since websockify performs no expiry check.

The underlying files remain visible if the CLI itself is suspect:

```bash
sudo ls -l /var/lib/lightstack/console-tokens/
sudo cat /var/lib/lightstack/console-tokens/*
```

Each file holds one line, `<token>: unix_socket:<path>`.

### Can the proxy read them, and reach the socket?

```bash
kubectl exec deploy/lightstack-lightstack-console-proxy -- ls -l /var/lib/lightstack/console-tokens/
kubectl exec deploy/lightstack-lightstack-console-proxy -- ls -l /var/lib/lightstack/console-sockets/
kubectl get deploy lightstack-lightstack-console-proxy -o jsonpath='{.spec.template.spec.volumes[*].hostPath.path}'; echo
```

A permission denial on the first refuses **every** token, which looks exactly like a token bug: mode/UID must line up. The second must list one `<instance-uuid>.sock` per running guest; the proxy keeps `DAC_OVERRIDE` because the directory belongs to QEMU's user with mode 0750. The third must print exactly the two directories, `console-tokens` and `console-sockets`, and nothing under `/var/lib/libvirt` (SC-013).

### Does the proxy resolve the token?

```bash
kubectl logs deploy/lightstack-lightstack-console-proxy | tail -20
```

Success looks like:

```text
Path: '/?token=9f8c…'
connecting to unix socket: /var/lib/lightstack/console-sockets/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.sock
```

A refusal looks like `Token '…' not found` — meaning the entry is absent, stale, or unreadable. No guest is contacted in that case, which is the intended fail-closed behaviour. `Failed to connect … No such file or directory` after a `connecting to` line means the guest is not running, because QEMU removes its socket on shutdown. `Permission denied` means the proxy lost `DAC_OVERRIDE`.

### Is the client page there?

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://<node-ip>:30680/vnc_lite.html   # expect 200
curl -s -o /dev/null -w '%{http_code}\n' http://<node-ip>:30680/vnc_auto.html   # expect 404
```

The second returning 404 is correct, and is why the documented example URLs changed.

## Security checks (SC-005 — all five must fail closed)

```bash
# 1. another instance's token — must not reach vm2
# 2. an expired token (wait TTL + reconcile interval, ~10m30s at defaults)
# 3. an invented token
# 4. a cross-project request without the admin role  → 404
# 5. reuse after the instance was stopped (or powered itself off) and restarted
openstack server stop vm1 && openstack server start vm1
# the pre-stop token for vm1 must be refused: the API stop/start revoked it
# 5b. the same, outside the API: within one reconcile interval the token must be gone,
#     because the domain ID changed (lightstack console-tokens list shows it)
sudo virsh destroy <domain> && sudo virsh start <domain>
```

Checks 5 and 5b are the ones worth running deliberately. The console socket is per instance, so a stale link can never reach another guest. The lifecycle revoke removes it at once, and for restarts outside the API the reconciler removes it within one sweep, because the domain ID changed.

## Guest console is not directly reachable (FR-017)

```bash
sudo ss -ltnp | grep -c qemu    # must print 0 — no guest console listens on TCP
```

If qemu holds a TCP port, that guest's console is reachable without a token and a port it frees can be handed to another guest — the whole security model is void for it.

## Concurrency (SC-008)

Open two browsers on the same instance: **both** must stay connected and show the guest (`sharePolicy='force-shared'`). With libvirt's default `allow-exclusive` the first viewer is silently evicted — which is the bug this setting prevents.

Open two on *different* instances: each shows only its own guest.

## Gates

```bash
make fmt && make lint && make test
make chart-test              # both charts lint + template; helm-unittest specs
./test/console_test.sh       # rewritten — the old version passes against a broken console
./test/ws0_console_test.sh   # serial log unregressed
```

Binary size should be unchanged: this feature adds **no** Go dependency, because the relay is not ours.
