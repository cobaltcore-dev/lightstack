# HOST_TESTING — SPEC-021 stub mode (a real console / noVNC)

**Feature**: `021-real-novnc-console` · **Branch**: `feature/534` · **Mode**: stub

Stub mode has no hypervisor and no guest, so there is nothing to display. What this
runbook proves is that O3K **says so** — immediately and in the standard shape —
instead of handing out a link that fails opaquely in the browser. That opaque
failure was finding F1.

The real-mode runbook (`HOST_TESTING_REAL_MODE.md`) is where the framebuffer gets
verified.

> **Every shell block runs from the repository root, on this feature's branch.** No
> block changes directory or checks out anything, so paths like `deployments/…` and
> `./test/…` resolve as written.
>
> Shell blocks are zsh-copy-paste-safe: no `!` inside double quotes, no `\` line
> continuations, no inline `#` comments.

---

## Prerequisites

Idempotent; safe to re-run.

```bash
sudo apt-get update
sudo apt-get install -y curl jq python3-openstackclient
docker --version
```

---

## Setup

One block, one paste. Idempotent — safe to re-run. Run it in the shell you will use
for S1–S7, since it exports the `OS_*` variables every later step needs.

```bash
export O3K_JWT_SECRET=dev-secret-not-for-production
export HORIZON_HTTP_PORT=8080
docker compose -f deployments/docker-compose-horizon.yml up -d

echo "--- waiting for O3K ---"
for i in $(seq 1 60); do curl -sf http://localhost:35357/readyz >/dev/null 2>&1 && echo "O3K ready" && break; sleep 1; done

grep -q 'spec021-hosttest' /etc/hosts || echo "127.0.0.1 o3k  # spec021-hosttest" | sudo tee -a /etc/hosts

export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3

echo "--- migration 065: console_auth_tokens ---"
docker compose -f deployments/docker-compose-horizon.yml exec -T postgres psql -U o3k -d o3k -c "\d console_auth_tokens"
echo "--- dead console_vnc columns remaining (want 0) ---"
docker compose -f deployments/docker-compose-horizon.yml exec -T postgres psql -U o3k -d o3k -c "\d instances" | grep -c console_vnc

echo "--- auth ---"
openstack token issue -f value -c id >/dev/null && echo "authenticated"

echo "--- image ---"
touch /tmp/spec021-cirros.qcow2
openstack image show cirros >/dev/null 2>&1 || openstack image create cirros --disk-format qcow2 --container-format bare --file /tmp/spec021-cirros.qcow2 >/dev/null
openstack image list -f value -c Name
```

**Desired outcome**, reading down the output:

- `O3K ready`
- the `console_auth_tokens` table, with `token`, `instance_id`, `project_id`, `host`,
  `port`, `created_at`, `expires_at`, two indexes and a `port > 0` check
- `0` dead `console_vnc` columns — both dropped by migration 065
- `authenticated`
- `cirros` listed

### Why those particular lines

**`HORIZON_HTTP_PORT=8080`** — rootless Podman cannot bind below 1024 and fails the
whole `up` with *"rootlessport cannot expose privileged port 80"*. Docker-as-root
users could omit it; setting it is harmless either way, so it is unconditional.
Every Horizon URL below uses `localhost:8080`.

**The `/etc/hosts` alias, and its position after `up -d`** — this stack sets
`O3K_ENDPOINT_HOST=o3k`, so the catalog advertises `http://o3k:<port>`: correct for
Horizon, which calls O3K server-side over the compose network, but unresolvable from
the host. Only Keystone escapes it, because `OS_AUTH_URL` is explicit; everything
else follows the catalog and fails with *"Failed to contact the endpoint at
http://o3k:9292"*. The compose file publishes every O3K port 1:1, so the alias makes
those URLs reach the published ports.

The ordering is load-bearing: Podman seeds each container's `/etc/hosts` from the
host's, so an alias present *before* `up -d` is copied into the Horizon container,
where `o3k` then resolves to Horizon itself and every backend call breaks. Teardown
removes the line for that reason.

`OS_*_ENDPOINT_OVERRIDE` is not a substitute. A known O3K defect lets
`common.BaseURL` rank `O3K_ENDPOINT_HOST` above the request Host header, so every
version document advertises `links[].href = http://o3k:<port>`; keystoneauth follows
that self-link during discovery and discards the override.

**The image** — seed data ships flavors but no images, and `server create` fails with
`No Image found for cirros` without one. Stub mode never reads image data, so an
empty file suffices.

---

## Test steps

### S1 — The console API refuses, and names the reason

```bash
openstack server create --flavor m1.tiny --image cirros spec021-stub-vm --wait
openstack console url show spec021-stub-vm
```

**Desired outcome**: the command fails rather than printing a URL, and the error
mentions stub mode. A URL here is a regression — it is exactly what F1 looked like.

Confirm the status code directly:

```bash
TOKEN=$(openstack token issue -f value -c id)
VM=$(openstack server show spec021-stub-vm -f value -c id)
curl -s -o /dev/stderr -w 'HTTP %{http_code}\n' -X POST "http://localhost:8774/v2.1/servers/$VM/remote-consoles" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"remote_console":{"protocol":"vnc","type":"novnc"}}'
```

**Desired outcome**: `HTTP 501`, body naming stub mode. Not 200, not 500.

### S2 — Unsupported console types are refused, not faked

```bash
for t in xvpvnc spice-html5 rdp-html5; do curl -s -o /dev/null -w "$t -> %{http_code}\n" -X POST "http://localhost:8774/v2.1/servers/$VM/remote-consoles" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d "{\"remote_console\":{\"protocol\":\"vnc\",\"type\":\"$t\"}}"; done
```

**Desired outcome**: all three print `400`. Before SPEC-021 each returned `200`
with a `localhost` URL that led nowhere.

### S3 — The serial fallback still works

```bash
openstack console log show spec021-stub-vm | tail -5
```

**Desired outcome**: console output is returned. This is the documented workaround
that kept working throughout the F1 outage; breaking it would be a regression.

### S4 — The token CLI exists and is honest

The binary lives at `/app/o3k` in the image and is **not** on `$PATH` — a bare
`o3k` gives `executable file not found`.

```bash
docker compose -f deployments/docker-compose-horizon.yml exec -T o3k /app/o3k console-tokens list
docker compose -f deployments/docker-compose-horizon.yml exec -T o3k /app/o3k console-tokens list --json
```

**Desired outcome**: `no live console tokens` (stub issues none), then exactly `[]`
for JSON — never `null`, so `jq length` works against an empty store. Exit code 0
both times.

```bash
docker compose -f deployments/docker-compose-horizon.yml exec -T o3k /app/o3k console-tokens wat
```

**Desired outcome**: usage text on stderr, exit code `2`.

### S5 — The scripted probe

```bash
O3K_MODE=stub ./test/console_test.sh
```

**Desired outcome**: `failed: 0`. This script previously passed against a
completely broken console; it now asserts refusals and URL shape.

### S6 — No console proxy is deployed in stub

```bash
docker compose -f deployments/docker-compose-horizon.yml config --services | grep -c novnc
```

**Desired outcome**: `0`. Stub mode refuses at the API, so the compose stack needs
no proxy — the dead-placeholder container that used to reproduce F1 is gone.

### S7 — Horizon shows the honest message

Horizon is already running from Setup, but it starts slower than O3K. Wait for it:

```bash
for i in $(seq 1 120); do curl -sf http://localhost:8080/dashboard/auth/login/ >/dev/null 2>&1 && echo ready && break; sleep 1; done
```

**Desired outcome**: `ready`.

Open `http://localhost:8080/dashboard`, log in as `admin` / `secret`, open the instance,
click **Console**.

**Desired outcome**: the tab reports the console is unavailable. In stub mode that
is the truth, not a bug — there is no guest. The **Console Log** tab still shows
output.

---

## Teardown

```bash
openstack server delete spec021-stub-vm
openstack image delete cirros
rm -f /tmp/spec021-cirros.qcow2
docker compose -f deployments/docker-compose-horizon.yml down -v
sudo sed -i "/spec021-hosttest/d" /etc/hosts
```

Removing the `/etc/hosts` line matters: leaving it behind poisons the next run,
since Podman copies it into the Horizon container.
