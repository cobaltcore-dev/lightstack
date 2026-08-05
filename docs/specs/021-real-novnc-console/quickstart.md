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

Prerequisites: the SPEC-018 real harness — O3K on k3s in real mode via `charts/o3k`, Horizon via `charts/horizon` (relocated by this feature), Ceph/RGW via Rook.

```bash
helm upgrade --install o3k charts/o3k \
  --set deploymentMode=single-node \
  --set nova.libvirtMode=real \
  --set consoleProxy.enabled=true \
  --set nova.consoleProxyBaseUrl=http://<node-ip>:30680
```

`nova.consoleProxyBaseUrl` is an **origin only** — no path. The client page and token query are appended by O3K (FR-020), so `/vnc_auto.html` must not appear here; that page does not exist in noVNC 1.6.0.

Boot an instance, then:

```bash
openstack server create --flavor m1.small --image cirros --network <net> vm1
openstack console url show vm1
```

Open the URL. **Expected**: the cirros framebuffer, and typing reaches the guest — log in and run a command (SC-001, SC-002).

## Probes that isolate a failure

Console failures are ambiguous by nature — "Unable to load Console" is what F1 already looked like. Each probe below narrows the cause to one component.

### Is the port real?

```bash
sudo virsh dumpxml <domain> | grep -A2 "<graphics"
```

Must show `port='59xx'` (resolved, not `-1`), `listen='127.0.0.1'`, and `sharePolicy='force-shared'`. A `-1` here means `DomainGetXMLDesc` was called with the wrong flags — flag `0` returns the running definition, `DomainXMLInactive` returns the unresolved one.

### Did O3K write a projection entry?

```bash
kubectl -n o3k exec deploy/o3k-o3k -- /app/o3k console-tokens list     # --json for structured output
kubectl -n o3k exec deploy/o3k-o3k -- /app/o3k console-tokens purge    # drop expired entries by hand
```

(The binary is at `/app/o3k` in the image, not on `$PATH`. On a host install it is
just `o3k console-tokens list`.)

One entry per live token, each resolving to `127.0.0.1:<port>`. If the list is empty right after a successful `console url show`, the failure is O3K-side. If entries accumulate past their expiry, the reconciler is not running — expiry is **not** enforced without it, since websockify performs no expiry check.

The underlying files remain visible if the CLI itself is suspect:

```bash
sudo ls -l /var/lib/o3k/console-tokens/
sudo cat /var/lib/o3k/console-tokens/*    # <token>: 127.0.0.1:<port>
```

### Can the proxy read them?

```bash
kubectl exec deploy/o3k-o3k-console-proxy -- ls -l /var/lib/o3k/console-tokens/
```

A permission denial here refuses **every** token, which looks exactly like a token bug. The pod drops ALL capabilities and may run non-root, so mode/UID must line up.

### Does the proxy resolve the token?

```bash
kubectl logs deploy/o3k-o3k-console-proxy | tail -20
```

Success looks like:

```text
Path: '/?token=9f8c…'
connecting to: 127.0.0.1:5901
```

A refusal looks like `Token '…' not found` — meaning the entry is absent, stale, or unreadable. No guest is contacted in that case, which is the intended fail-closed behaviour.

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
# 5. reuse after the instance was stopped and a second instance took its port
openstack server stop vm1 && openstack server create ... vm2
# the pre-stop token for vm1 must be refused, never showing vm2
```

Check 5 is the one worth running deliberately: libvirt assigns console ports sequentially, so without revocation on stop/start a stale token can surface **another tenant's** framebuffer.

## Guest console is not directly reachable (FR-017)

From off-node:

```bash
nc -vz <node-ip> 5901    # must fail — the guest binds loopback only
```

If this connects, the token is decorative and the whole security model is void.

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
