# Host testing guide — SPEC-034 stop caching the service catalog (stub mode, one host)

Validates cloud-storage#581 on one host with the shared cache **on**, against a real Redis. With
`cache.enabled: true`, an endpoint created or deleted through the Identity API, a service changed or deleted, and a
restart with a new scheme or object-storage endpoint must all show in the very next token's catalog (SC-001,
SC-002). Redis must hold no catalog key while flavors still use it (SC-003, SC-006, FR-009). The bundled Compose
stacks must start no Redis (SC-005), and the docs must not describe a catalog cache (FR-011).

Run all commands from the repo root, top to bottom: §0, §1, §2, §3. Shell **T1** runs everything. The server runs
in the background from the branch's own build, on a throwaway SQLite file under `/tmp/ls581` with stub
services. Redis runs in a throwaway container on `127.0.0.1:56381`. Neither `make db-up`'s PostgreSQL nor
`/var/lib/lightstack` is read or changed. To start over, run §3 first.

| Validates | Proved in |
|---|---|
| FR-001, FR-003, US1 AS1, contract C1: a created endpoint in the next issued **and** validated token | 2.1 |
| FR-003, US1 AS2, contract C2: a deleted endpoint gone from the next token | 2.2 |
| FR-003, US1 AS3, contract C3: a service renamed, then deleted | 2.3 |
| FR-006, FR-007, FR-009, SC-003, SC-006, contract C6: no catalog key in Redis; flavors still cached | 2.4 |
| FR-002, contract C7: Redis unreachable, catalog unchanged | 2.5 |
| FR-004, US2 AS1 and AS2, SC-002, contract C4: a restart with a new scheme and object-storage endpoint | 2.6 |
| FR-010, US3, SC-005: the Compose stacks start no Redis and still come up | 2.7 |
| FR-007, FR-011, US4, SC-003: no catalog cache in the source, docs or config | 2.8 |

Not host-tested: US2 AS3 (a migration that changes catalog rows) and AS4 (a rolling update across nodes). Both
follow from contract C6, and this is a single node.

Two client commands don't work against Lightstack, for reasons outside this feature, so the steps avoid them.
`--region RegionOne` fails because `/v3/regions` is not routed. `openstack endpoint delete` first sends
`GET /v3/endpoints/{id}`, which answers `405`, so 2.2 sends the `DELETE` with `curl`.

---

## 0. Prerequisites

One block; it needs no root. It needs:
- `go` 1.26 or later (`go.mod`): builds the branch's binary.
- Docker or Podman, either, with `compose` (`docker compose`, or `podman compose` backed by podman-compose or
  docker-compose): runs Redis in §1 and the SQLite Compose stack in 2.7.
- `openstack` (python-openstackclient), `curl`, `jq`: the client side.
- `ss`, `sed`, `grep`, `awk`: the checks.

```bash
missing=0
for t in go openstack curl jq ss sed grep awk; do command -v $t >/dev/null || { echo "MISSING: $t"; missing=1; }; done
command -v docker >/dev/null || command -v podman >/dev/null || { echo "MISSING: docker or podman"; missing=1; }
ENGINE=$(command -v docker || command -v podman)
[ -n "$ENGINE" ] && { $ENGINE compose version >/dev/null 2>&1 || { echo "MISSING: $ENGINE compose"; missing=1; }; }
go version; openstack --version; jq --version; [ -n "$ENGINE" ] && $ENGINE --version
[ "$missing" = 0 ] && echo "PREREQS OK" || echo "PREREQS FAILED: install the missing tools above, then re-run"
```

**Pass =** `PREREQS OK`, no `MISSING:` line.

Install anything reported missing yourself before going on; this runbook installs nothing.

---

## 1. Setup steps

**Shell T1** — keep it for the whole run.

The block refuses to run outside the repo root, while Lightstack's ports or `56381` are taken, or while a container
named `ls581-redis` or `lightstack` exists. It records the engine's image IDs, so §3 can remove exactly the images
the run adds. It builds the branch into `/tmp/ls581/lightstack`. It writes `/tmp/ls581/lightstack.yaml` from
`config/lightstack.yaml` with SQLite at `/tmp/ls581/lightstack.db`, `state_path: /tmp/ls581/state`, stub
block and image storage, and `cache.enabled: true` pointed at the Redis it starts. It writes `/tmp/ls581.env`,
which every later block sources: the client's `OS_*` settings, plus `ls_start` / `ls_stop` (the server in the
background, its PID in `/tmp/ls581/server.pid`, its log in `/tmp/ls581/server.log`) and `ls_redis` (`redis-cli`
in the container). Then it starts the server and issues a token.

```bash
ENGINE=$(command -v docker || command -v podman)
if [ ! -f config/lightstack.yaml ]; then
  echo "STOP: run this from the repo root"
elif ss -lnt | grep -qE ':(35357|8774|8775|8776|8778|9292|9696|56381) '; then
  echo "STOP: a port this run needs is taken (ss -lnt shows which); stop what listens there, often a Compose stack, then re-run this block"
elif $ENGINE ps -a --format '{{.Names}}' | grep -qxE 'ls581-redis|lightstack'; then
  echo "STOP: a container named ls581-redis or lightstack exists; run §3, or remove it, then re-run this block"
else
  rm -rf /tmp/ls581; mkdir -p /tmp/ls581/state
  $ENGINE images -q --no-trunc | sort -u > /tmp/ls581-images-before
  go build -o /tmp/ls581/lightstack ./cmd/lightstack
  sed -e '1i state_path: /tmp/ls581/state' -e 's|^  url: "postgres://.*|  url: "sqlite:///tmp/ls581/lightstack.db"|' -e 's|storage_mode: local.*|storage_mode: stub|' -e '/^cache:/,/^rate_limiting:/ s|^  enabled: false.*|  enabled: true|' -e '/^cache:/,/^rate_limiting:/ s|^  redis_url: .*|  redis_url: "redis://127.0.0.1:56381/0"|' config/lightstack.yaml > /tmp/ls581/lightstack.yaml
  grep -nE '^state_path|sqlite:|storage_mode:|^  enabled: true|56381' /tmp/ls581/lightstack.yaml
  $ENGINE run -d --rm --name ls581-redis -p 127.0.0.1:56381:6379 docker.io/library/redis:7-alpine
  cat > /tmp/ls581.env <<'ENV'
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
ls_start() {
  ss -lnt | grep -q ':35357 ' && { echo "STOP: port 35357 is taken; run ls_stop first"; return 1; }
  nohup sh -c 'echo $$ > /tmp/ls581/server.pid; exec "$@"' sh env "$@" /tmp/ls581/lightstack -config /tmp/ls581/lightstack.yaml -migrations "$REPO/migrations" >> /tmp/ls581/server.log 2>&1 &
  for i in $(seq 1 60); do curl -sf -o /dev/null http://localhost:35357/v3 && { echo "server up, pid $(cat /tmp/ls581/server.pid)"; return 0; }; sleep 1; done
  echo "STOP: the server did not come up; read /tmp/ls581/server.log"; return 1
}
ls_stop() {
  [ -f /tmp/ls581/server.pid ] && kill "$(cat /tmp/ls581/server.pid)" 2>/dev/null
  for i in $(seq 1 60); do ss -lnt | grep -q ':35357 ' || break; sleep 1; done
  rm -f /tmp/ls581/server.pid
}
ls_redis() { [ -n "$ENGINE" ] && $ENGINE exec ls581-redis redis-cli "$@"; }
ENV
  printf 'export %s=%s\n' ENGINE "$ENGINE" REPO "$PWD" >> /tmp/ls581.env
  source /tmp/ls581.env
  ls_start && openstack token issue -f value -c project_id && ls_redis ping
fi
```

**Pass =** The `grep` shows exactly `state_path: /tmp/ls581/state`, `url: "sqlite:///tmp/ls581/lightstack.db"`,
two `storage_mode: stub` lines, `enabled: true` and `redis_url: "redis://127.0.0.1:56381/0"`. Then come a container
ID, `server up, pid <n>`, the project ID `00000000-0000-0000-0000-000000000002` and `PONG`. The log shows
`Redis cache enabled (prefix: lightstack:, …)`: `grep 'Redis cache' /tmp/ls581/server.log`.

---

## 2. Test steps

All in **T1**. Every block sources `/tmp/ls581.env`, so it also works in a fresh shell.

### 2.1 A created endpoint is in the next issued and validated token (FR-001, FR-003, C1)

A token is issued first: with the old code, that warmed the cache. Then a service and an endpoint are created
through the Identity API. The next issued token's catalog is read through `openstack catalog show`, which issues a
new token. The first token is then validated (`GET /v3/auth/tokens`), and its catalog read from the response.

```bash
source /tmp/ls581.env
T0=$(openstack token issue -f value -c id)
openstack service create --name probe581 probe581 -f value -c id
openstack endpoint create probe581 public http://localhost:8999/v1 -f value -c id
openstack catalog show probe581 -f value -c endpoints
curl -s -H "X-Auth-Token: $T0" -H "X-Subject-Token: $T0" $OS_AUTH_URL/auth/tokens | jq -r '.token.catalog[] | select(.type == "probe581") | .endpoints[].url'
```

**Expect:** the service ID, the endpoint ID, then
`[{'interface': 'public', 'region': '', 'url': 'http://localhost:8999/v1'}]`, then `http://localhost:8999/v1`
from the validated first token. Before the fix the catalog said `service probe581 not found` and the validation
printed nothing, for up to 24 hours.

### 2.2 A deleted endpoint is gone from the next token (FR-003, C2)

The first `catalog show` runs after the create, so a kept catalog would now hold the endpoint. The `DELETE` goes
out with `curl` (see the intro), and the next token's catalog is read again.

```bash
source /tmp/ls581.env
openstack catalog show probe581 -f value -c endpoints
T=$(openstack token issue -f value -c id)
E=$(openstack endpoint list --service probe581 -f value -c ID)
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "X-Auth-Token: $T" $OS_AUTH_URL/endpoints/$E
openstack catalog show probe581 -f value -c endpoints
```

**Expect:** `[{'interface': 'public', 'region': '', 'url': 'http://localhost:8999/v1'}]`, then `204`, then `[]`.
Before the fix the last line still listed the endpoint.

### 2.3 A service renamed, then deleted, in the next token (FR-003, C3)

```bash
source /tmp/ls581.env
S=$(openstack service list -f value -c ID -c Type | awk '$2 == "probe581" {print $1}')
openstack service set --name probe581-renamed $S
openstack catalog show probe581 -f value -c name
openstack service delete $S
openstack catalog show probe581 -f value -c name
```

**Expect:** `probe581-renamed`, then `service probe581 not found` (followed by the client's
`No recognized column names` note). Both come from the next token's catalog.

### 2.4 Redis holds no catalog key; flavors are still cached (FR-007, FR-009, SC-003, SC-006, C6)

Only single-resource reads use the cache (a flavor shown by ID, likewise images and networks); lists never did. So
the flavor is shown by ID, a token is issued, and Redis is searched.

```bash
source /tmp/ls581.env
F=$(openstack flavor list -f value -c ID | head -1)
openstack flavor show $F -f value -c name
openstack token issue -f value -c expires
ls_redis --scan --pattern 'lightstack:flavor:*'
ls_redis --scan --pattern '*service_catalog*' | wc -l
```

**Expect:** a flavor name (`m1.tiny`), an expiry time, `lightstack:flavor:<the flavor's id>`, then `0`. Before the
fix the last line was `1`: `lightstack:service_catalog:00000000-0000-0000-0000-000000000002:localhost`.

### 2.5 Redis unreachable, catalog unchanged (FR-002, C7)

Redis is stopped while the server runs, the catalog is listed, and a fresh Redis is started for the next steps.

```bash
source /tmp/ls581.env
[ -n "$ENGINE" ] && $ENGINE stop ls581-redis
openstack catalog list -f value -c Name | sort | tr '\n' ' '; echo
[ -n "$ENGINE" ] && $ENGINE run -d --rm --name ls581-redis -p 127.0.0.1:56381:6379 docker.io/library/redis:7-alpine
```

**Expect:** `ls581-redis`, then `cinder cinderv3 glance keystone neutron nova placement`, the same as with Redis up,
then a container ID.

### 2.6 A restart with a new scheme and object-storage endpoint shows at once (FR-004, SC-002, C4)

The first line counts `https://` URLs under the running server. Redis is kept across the restart (the old code's
entries would have survived it). The server comes back with `LIGHTSTACK_ENDPOINT_SCHEME=https` and
`LIGHTSTACK_GLANCE_S3_ENDPOINT=http://rgw.example:7480`. The first catalog after the restart is then read three
ways: how many non-`s3` URLs are **not** `https://` (out of how many), the `s3` entry, and Redis.

```bash
source /tmp/ls581.env
openstack catalog list -f json | jq -r '[.[].Endpoints[]?.url] | "\(map(select(startswith("https://"))) | length) of \(length) https"'
ls_stop
ls_start LIGHTSTACK_ENDPOINT_SCHEME=https LIGHTSTACK_GLANCE_S3_ENDPOINT=http://rgw.example:7480
openstack catalog list -f json | jq -r '[.[] | select(.Type == "s3" | not) | .Endpoints[]?.url] | "\(map(select(startswith("https://") | not)) | length) of \(length) not https"'
openstack catalog list -f json | jq -r '.[] | select(.Type == "s3") | .Endpoints[]?.url'
ls_redis --scan --pattern '*service_catalog*' | wc -l
```

**Expect:** `0 of 7 https` (all http before), `server up, pid <n>`, then `0 of 7 not https`, then
`http://rgw.example:7480`, then `0`. Before the fix the restart changed nothing: every URL stayed `http://`, and
the `s3` entry stayed absent.

### 2.7 The Compose stacks start no Redis and still come up (FR-010, SC-005)

This step stops the §1 server, because the stack takes the same ports. It checks both Compose files for Redis and
validates the PostgreSQL stack's file. It does not start that stack: its PostgreSQL is named `lightstack-postgres`
on `5432`, the same as `make db-up`'s. It brings the SQLite stack up (it builds the image, which takes a few
minutes), issues a token against it and takes it down again.

```bash
source /tmp/ls581.env
grep -nE 'redis|CACHE_ENABLED' deployments/docker-compose.yml deployments/docker-compose-sqlite.yml | wc -l
$ENGINE compose -f deployments/docker-compose.yml config -q && echo "postgres stack file OK"
ls_stop
$ENGINE compose -p ls581 -f deployments/docker-compose-sqlite.yml up -d --build
for i in $(seq 1 90); do curl -sf -o /dev/null http://localhost:35357/v3 && break; sleep 2; done
$ENGINE ps --format '{{.Names}}' | grep -cx lightstack-redis
openstack token issue -f value -c project_id
$ENGINE compose -p ls581 -f deployments/docker-compose-sqlite.yml down -v
```

**Expect:** `0`, `postgres stack file OK`, the build and start output, then `0` (no `lightstack-redis` container),
then `00000000-0000-0000-0000-000000000002`, then the stack's removal. Before the change the stack started
`lightstack-redis` and set two variables the binary never read.

### 2.8 No catalog cache in the source, docs or config (FR-007, FR-011, SC-003)

```bash
grep -rn service_catalog --include='*.go' . | wc -l
grep -n service_catalog config/lightstack.yaml | wc -l
grep -n -i catalog docs/CONFIGURATION.md docs/UPGRADING.md | grep -i cach
grep -n 'keystone.NewAuthService\|keystone.NewService' docs/ARCHITECTURE.md
```

**Expect:** `0`, `0`, then only the "**not cached** (SPEC-034)" line of `docs/CONFIGURATION.md` and the
`UPGRADING.md` lines of "The service catalog is no longer cached". Then
`keystone.NewAuthService(db, jwtSecret, ttl)` and `keystone.NewService(db, authService)`, neither with `cache`.

---

## 3. Teardown steps

Stops the server, takes down the SQLite stack if it still runs, removes the Redis container and every image the run
added (compared with the list §1 recorded), checks that the ports are free, and deletes the run's files. It may
run again: what is already gone is skipped.

```bash
[ -f /tmp/ls581.env ] && source /tmp/ls581.env
ENGINE=${ENGINE:-$(command -v docker || command -v podman)}
[ -f /tmp/ls581/server.pid ] && kill "$(cat /tmp/ls581/server.pid)" 2>/dev/null
[ -n "$ENGINE" ] && $ENGINE compose -p ls581 -f deployments/docker-compose-sqlite.yml down -v >/dev/null 2>&1
[ -n "$ENGINE" ] && $ENGINE rm -f ls581-redis >/dev/null 2>&1
[ -n "$ENGINE" ] && [ -s /tmp/ls581-images-before ] && $ENGINE images -q --no-trunc | sort -u | grep -vxF -f /tmp/ls581-images-before | xargs -r $ENGINE rmi -f
sleep 2; ss -lnt | grep -cE ':(35357|8774|8775|8776|8778|9292|9696|56381) '
rm -rf /tmp/ls581; rm -f /tmp/ls581.env /tmp/ls581-images-before
```

**Pass =** the image removals, if any, then `0`: no port the run used is still taken.

---

## Troubleshooting

- **`server did not come up`**: read `/tmp/ls581/server.log`. `Failed to initialize cache` means Redis on
  `127.0.0.1:56381` is not answering (`ls_redis ping`). `bind: address already in use` means another Lightstack
  holds a port: find it with `ss -lntp`, and never stop it with `pkill -f`, which matches any command line
  containing the path.
- **Every `openstack` command answers `401`**: `OS_USER_DOMAIN_NAME` must be `Default`. Lightstack matches the
  domain name case-sensitively, so `default` fails.
- **2.1 prints `No Region found for RegionOne`**: a `--region` crept into the endpoint create; drop it (see the
  intro).
- **2.6's second line is not `0`**: an `openstack` client caches nothing between commands, so a stale catalog here
  is the server's. Check `ps -p $(cat /tmp/ls581/server.pid) -o args=` shows the `/tmp/ls581/lightstack` binary.
  `env` passes the variables but does not show in `args`, so compare `/proc/$(cat /tmp/ls581/server.pid)/environ`
  as well.
- **2.7 `compose` fails on Podman with no provider**: `podman compose` needs podman-compose or docker-compose
  installed; §0 checks for it.
