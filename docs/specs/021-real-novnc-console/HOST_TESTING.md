# HOST_TESTING — SPEC-021 stub mode (a real console / noVNC)

**Feature**: `021-real-novnc-console` · **Branch**: `feature/534` · **Mode**: stub

Stub mode has no hypervisor and no guest, so there is nothing to display. What this
runbook proves is that Lightstack **says so** — immediately and in the standard shape —
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

On a host that ran `HOST_TESTING_REAL_MODE.md`, run its **Teardown** first. Its Lightstack
pod uses host networking, so it holds the ports this stack publishes, and its database
container is named `lightstack-postgres`, like this stack's — `up` fails on either.

```bash
sudo apt-get update
sudo apt-get install -y curl jq python3-openstackclient
docker --version
```

---

## Setup

One block, one paste. Idempotent — safe to re-run. Run it in the shell you will use
for the test steps, since it exports the `OS_*` variables every later step needs.

```bash
export LIGHTSTACK_JWT_SECRET=dev-secret-not-for-production
export HORIZON_HTTP_PORT=8080
docker compose -f deployments/docker-compose-horizon.yml exec -T postgres psql -U lightstack -d lightstack -Atc "SELECT 1 FROM information_schema.columns WHERE table_name='console_auth_tokens' AND column_name='port'" 2>/dev/null | grep -q 1 && docker compose -f deployments/docker-compose-horizon.yml down -v
sudo sed -i "/spec021-hosttest/d" /etc/hosts
docker compose -f deployments/docker-compose-horizon.yml up -d --build

echo "--- waiting for Lightstack ---"
for i in $(seq 1 60); do curl -sf http://localhost:35357/readyz >/dev/null 2>&1 && echo "Lightstack ready" && break; sleep 1; done

grep -q 'spec021-hosttest' /etc/hosts || echo "127.0.0.1 lightstack  # spec021-hosttest" | sudo tee -a /etc/hosts

export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3

echo "--- migration 065: console_auth_tokens ---"
docker compose -f deployments/docker-compose-horizon.yml exec -T postgres psql -U lightstack -d lightstack -c "\d console_auth_tokens"
echo "--- dead console_vnc columns remaining (want 0) ---"
docker compose -f deployments/docker-compose-horizon.yml exec -T postgres psql -U lightstack -d lightstack -c "\d instances" | grep -c console_vnc

echo "--- auth ---"
openstack token issue -f value -c id >/dev/null && echo "authenticated"

echo "--- image ---"
touch /tmp/spec021-cirros.qcow2
openstack image show cirros >/dev/null 2>&1 || openstack image create cirros --disk-format qcow2 --container-format bare --file /tmp/spec021-cirros.qcow2 >/dev/null
openstack image list -f value -c Name
```

**Desired outcome**, reading down the output:

- `Lightstack ready`
- the `console_auth_tokens` table, with `token`, `instance_id`, `project_id`, `socket`,
  `created_at`, `expires_at`, two indexes and a `CHECK (socket ~~ '/%'::text)` (psql's
  spelling of `LIKE`) — a token names the guest's console socket, not a host and port
- `0` dead `console_vnc` columns — both dropped by migration 065
- `authenticated`
- `cirros` listed

### Why those particular lines

**`--build`** — the stack builds `lightstack:latest` from this checkout, but a plain `up -d`
builds only when no such image exists. An image left from an earlier run would be reused,
and the whole runbook would test old code.

**The `down -v` guard** — migration `065` was changed in place on this branch: tokens
now name a socket path instead of a host and port. A compose volume that survived a
run of an earlier build keeps the old table, and golang-migrate will not re-apply a
version it has already recorded. The guard recreates the stack only in that case;
otherwise it does nothing, including when the stack is not running.

**`HORIZON_HTTP_PORT=8080`** — rootless Podman cannot bind below 1024 and fails the
whole `up` with *"rootlessport cannot expose privileged port 80"*. Docker-as-root
users could omit it; setting it is harmless either way, so it is unconditional.
Every Horizon URL below uses `localhost:8080`.

**The `/etc/hosts` alias, and its position after `up -d`** — this stack sets
`LIGHTSTACK_ENDPOINT_HOST=lightstack`, so the catalog advertises `http://lightstack:<port>`: correct for
Horizon, which calls Lightstack server-side over the compose network, but unresolvable from
the host. Only Keystone escapes it, because `OS_AUTH_URL` is explicit; everything
else follows the catalog and fails with *"Failed to contact the endpoint at
http://lightstack:9292"*. The compose file publishes every Lightstack port 1:1 except
placement's `8778`, which only Horizon calls, so the alias makes those URLs reach the
published ports.

The ordering is load-bearing: Podman seeds each container's `/etc/hosts` from the
host's, so an alias present *before* `up -d` is copied into the Horizon container,
where `lightstack` then resolves to Horizon itself and every backend call breaks. Teardown
removes the line for that reason, and so does Setup before `up`: a re-run after a failed
`up` would otherwise find the line already there.

A stack created with the line in place fails the Horizon login with *"Unable to establish
connection to keystone endpoint"*. Recreating Horizon alone fixes it:

```bash
sudo sed -i "/spec021-hosttest/d" /etc/hosts
docker compose -f deployments/docker-compose-horizon.yml up -d --force-recreate --no-deps horizon
grep -q 'spec021-hosttest' /etc/hosts || echo "127.0.0.1 lightstack  # spec021-hosttest" | sudo tee -a /etc/hosts
```

`OS_*_ENDPOINT_OVERRIDE` is not a substitute. A known Lightstack defect lets
`common.BaseURL` rank `LIGHTSTACK_ENDPOINT_HOST` above the request Host header, so every
version document advertises `links[].href = http://lightstack:<port>`; keystoneauth follows
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

The binary lives at `/app/lightstack` in the image and is **not** on `$PATH` — a bare
`lightstack` gives `executable file not found`.

```bash
docker compose -f deployments/docker-compose-horizon.yml exec -T lightstack /app/lightstack console-tokens list; echo "exit $?"
docker compose -f deployments/docker-compose-horizon.yml exec -T lightstack /app/lightstack console-tokens list --json; echo "exit $?"
```

**Desired outcome**: `no live console tokens` (stub issues none), then exactly `[]`
for JSON — never `null`, so `jq length` works against an empty store. `exit 0`
both times.

```bash
docker compose -f deployments/docker-compose-horizon.yml exec -T lightstack /app/lightstack console-tokens wat; echo "exit $?"
```

**Desired outcome**: usage text on stderr, `exit 2`. Podman's compose wrapper passes the
code through and adds an `Error: executing … exit status 2` line of its own.

### S5 — The scripted probe

```bash
LIGHTSTACK_MODE=stub ./test/console_test.sh
```

**Desired outcome**: `failed: 0`. This script previously passed against a
completely broken console; it now asserts refusals and URL shape.

### S6 — No console proxy is deployed in stub

```bash
docker compose -f deployments/docker-compose-horizon.yml config --services
```

**Desired outcome**: `postgres`, `memcached`, `lightstack` and `horizon` — no `novnc`.
Stub mode refuses at the API, so the compose stack needs no proxy — the
dead-placeholder container that used to reproduce F1 is gone. List the services rather
than count matches: a compose file that does not parse prints no services either, and a
count would read that as success.

### S7 — Horizon shows the honest message

Horizon is already running from Setup, but it starts slower than Lightstack. Wait for it:

```bash
for i in $(seq 1 120); do curl -sf http://localhost:8080/dashboard/auth/login/ >/dev/null 2>&1 && echo ready && break; sleep 1; done
```

**Desired outcome**: `ready`.

Open `http://localhost:8080/dashboard`, log in as `admin` / `secret`, open the instance,
click **Console**.

**Desired outcome**: the tab reports the console is unavailable. In stub mode that
is the truth, not a bug — there is no guest. The **Console Log** tab still shows
output.

Then open **Admin → Compute → Hypervisors** and its third tab, **Resource Provider**. The
page opens on the **Hypervisor** tab, whose `lightstack-node-1` row is Nova's hypervisor,
not a resource provider.

**Desired outcome**: an empty table, not *"Unable to retrieve providers information."*
Horizon finds placement in the catalog (S11), and Lightstack's placement reports no
resource providers.

### S8 — A reboot needs a valid type

Upstream requires `reboot.type`, HARD or SOFT, and nothing else in the body. `$VM` and
`$TOKEN` are still set from S1.

```bash
curl -s -o /dev/null -w 'no type -> %{http_code}\n' -X POST "http://localhost:8774/v2.1/servers/$VM/action" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"reboot":{}}'
curl -s -o /dev/null -w 'unknown type -> %{http_code}\n' -X POST "http://localhost:8774/v2.1/servers/$VM/action" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"reboot":{"type":"FAST"}}'
curl -s -o /dev/null -w 'hard -> %{http_code}\n' -X POST "http://localhost:8774/v2.1/servers/$VM/action" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"reboot":{"type":"HARD"}}'
curl -s "http://localhost:8774/v2.1/servers/$VM" -H "X-Auth-Token: $TOKEN" | jq -r .server.status
sleep 2
curl -s "http://localhost:8774/v2.1/servers/$VM" -H "X-Auth-Token: $TOKEN" | jq -r .server.status
```

**Desired outcome**: `400`, `400`, `202`, then `HARD_REBOOT` — upstream's status for a
hard reboot in progress — and `ACTIVE` once the stub has finished. Both refused bodies
used to be accepted.

### S9 — The metadata service answers every caller as itself

Guests are identified by their own address. `X-Instance-ID`, which tests use to name an
instance from outside any guest, is believed only from loopback, so ask once from inside
the container and twice from the host:

```bash
docker compose -f deployments/docker-compose-horizon.yml exec -T lightstack curl -s -w '\n' -H "X-Instance-ID: $VM" http://127.0.0.1:8775/latest/meta-data/instance-id
echo "i-$(echo $VM | tr -d - | cut -c1-17)"
curl -s -o /dev/null -w 'named by header, from the host -> %{http_code}\n' -H "X-Instance-ID: $VM" http://localhost:8775/latest/meta-data/instance-id
curl -s -o /dev/null -w 'claiming loopback, from the host -> %{http_code}\n' -H "X-Instance-ID: $VM" -H "X-Forwarded-For: 127.0.0.1" http://localhost:8775/latest/meta-data/instance-id
```

**Desired outcome**: the first two lines match — the EC2-shaped id, `i-` and the first
17 hex digits of the server's UUID, where the bare UUID used to be. Then `404` twice: port
mapping delivers host requests from an address that is not loopback — the bridge gateway,
or under rootless Podman the container's own address, which its port forwarder connects
from — so the header is ignored, and a forwarded `127.0.0.1` does not make a caller
loopback. Before the fix, both answered for the named instance.

This stack leaves `metadata.trust_instance_header` off, as every deployment with guests
must. Only `deployments/docker-compose.yml` turns it on, for the contract tests.

### S10 — Logs record the real caller

s3tokens logs every rejection with the caller's address, which is how probing shows up:

```bash
curl -s -o /dev/null -w 's3tokens -> %{http_code}\n' -X POST http://localhost:35357/v3/s3tokens -H 'X-Forwarded-For: 203.0.113.9' -H 'Content-Type: application/json' -d '{}'
docker compose -f deployments/docker-compose-horizon.yml logs lightstack 2>&1 | grep 'signature validation denied' | tail -1
```

**Desired outcome**: `401`, and the log line's `client_ip` is where the request really
came from — the bridge gateway, or under rootless Podman the lightstack container's own
address — never `203.0.113.9`.

### S11 — Placement is in the catalog

```bash
openstack catalog list -f value -c Name -c Endpoints | grep placement
```

**Desired outcome**: `placement`, with one public endpoint at `http://lightstack:8778`
— this stack advertises the `lightstack` hostname. It used to be missing: only the
fallback catalog listed it. Horizon reaches the port over the compose network; the stack
does not publish it, so placement is not reachable from the host.

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
