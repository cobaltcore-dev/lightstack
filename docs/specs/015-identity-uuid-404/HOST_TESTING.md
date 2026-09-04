# Host testing guide — SPEC-015 identity 404 for non-UUID ids

End-to-end validation of the [cloud-storage#515](https://github.com/cobaltcore-dev/cloud-storage/issues/515)
fix on a real host: lightstack's Keystone identity lookups by path id return **404** (not
**500**) for a non-UUID `{id}`, so the openstack CLI / Terraform **name→id
resolution** works again.

**Topology.** Dead simple — one Lightstack process + PostgreSQL, no libvirt/Ceph/clusters.
Keystone identity is DB-only, so **stub mode** (the default) exercises the fix fully;
no real-mode root needed. Run **two shells**:
- `# T1 - lightstack` — the Lightstack server process.
- `# T2 - client` — `curl` + the `openstack` CLI + `go test` + teardown.

| Validates | Where |
|---|---|
| FR-001/003 malformed path id → 404, never 500 | §3 |
| FR-004 malformed **and** valid-but-unknown return the identical Keystone-verbatim 404 | §3 |
| SC-004 no regression — existing ids still 200/204 | §3 |
| FR-002 whole identity surface (projects/users/roles/groups/domains/services/endpoints/credentials/app-creds) | §3 |
| FR-009 authorization precedence — 401/403 before the 404 | §4 |
| FR-008 / SC-002 CLI name→id resolution works | §5 |
| (optional) the automated DB-backed + integration tests | §6 |

---

## 0. Prerequisites (one-time)

```bash
# T2 - client
sudo apt install -y curl jq postgresql-client
go version                                   # Go 1.26 (builds the binary under test)
docker version                               # for `make db-up` (postgres:18)
openstack --version || pipx install python-openstackclient
```

---

## 1. Setup — build Lightstack *with the fix* and start it

```bash
# T1 - lightstack
cd /home/fnk/code/sap/ora/lightstack/bugfix/515
make db-up                                   # PostgreSQL on :5432 (lightstack/secret/lightstack)
make build                                   # → bin/lightstack  (must be this branch, so it contains #515)
./bin/lightstack --config config/lightstack.yaml           # foreground; auto-migrates + seeds admin/default on startup
# leave this running; logs stream here. Keystone listens on :35357.
```
**Pass** = the log shows the services starting and `keystone` listening on `:35357`
(no crash). Lightstack applies migrations and seeds the `admin` user (password `secret`) +
`default` project on first start.

> Running the *built binary from this branch* is the whole point — a released image
> would still have the 500 bug. Confirm with `git rev-parse --short HEAD` (a `wip…#515`
> commit) before `make build`.

---

## 2. Authenticate (admin)

```bash
# T2 - client
cat > ~/.lightstack-env <<'EOF'
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF
source ~/.lightstack-env
TOKEN=$(openstack token issue -f value -c id) && echo "auth OK"
H=(-H "X-Auth-Token: $TOKEN")
```
**Pass** = `auth OK` (a token was issued). If the seeded admin password differs on
your config, set `OS_PASSWORD` accordingly.

---

## 3. The fix — malformed & unknown ids → identical verbatim 404 (never 500)

Reproduce the ticket and sweep the surface. Every by-id lookup must be **404**:
```bash
# T2 - client
for res in projects users roles groups domains services credentials application_credentials; do
  m=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$OS_AUTH_URL/$res/not-a-uuid")
  u=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$OS_AUTH_URL/$res/00000000-0000-0000-0000-0000000000ff")
  printf '%-24s malformed=%s  unknown-uuid=%s\n' "$res" "$m" "$u"
done
```
**Pass** = every line reads `malformed=404  unknown-uuid=404` (before the fix, `malformed`
was `500`).

Both cases return the **identical, Keystone-verbatim** body:
```bash
# T2 - client
curl -s "${H[@]}" "$OS_AUTH_URL/projects/not-a-uuid" | jq .
# {"itemNotFound":{"code":404,"message":"Could not find project: not-a-uuid."}}
curl -s "${H[@]}" "$OS_AUTH_URL/projects/00000000-0000-0000-0000-0000000000ff" | jq -r '.itemNotFound.message'
# Could not find project: 00000000-0000-0000-0000-0000000000ff.
```
**Pass** = message is `Could not find <resource>: <id>.` in both (only the id differs).

Mutating verbs and a nested path behave the same; existing ids are unchanged:
```bash
# T2 - client
curl -s -o /dev/null -w 'PATCH malformed  -> %{http_code}\n' -X PATCH "${H[@]}" \
  -H 'Content-Type: application/json' -d '{"project":{"name":"x"}}' "$OS_AUTH_URL/projects/not-a-uuid"   # 404
curl -s -o /dev/null -w 'DELETE malformed -> %{http_code}\n' -X DELETE "${H[@]}" "$OS_AUTH_URL/projects/not-a-uuid"  # 404
V=00000000-0000-0000-0000-000000000001   # a well-formed (unknown) uuid for the untested segments
curl -s "${H[@]}" "$OS_AUTH_URL/projects/$V/users/not-a-uuid/roles/$V" | jq -r '.itemNotFound.message'   # Could not find user: not-a-uuid.
PID=$(openstack project list -f value -c ID | head -1)
curl -s -o /dev/null -w 'existing project -> %{http_code}\n' "${H[@]}" "$OS_AUTH_URL/projects/$PID"     # 200 (regression guard)
```
**Pass** = malformed PATCH/DELETE → 404, the nested path names the first malformed
segment (`user`), and an existing id still → 200.

---

## 4. Authorization precedes the 404 (FR-009)

```bash
# T2 - client
# unauthenticated + malformed id -> 401 (authN first)
curl -s -o /dev/null -w 'no-token      -> %{http_code}\n' "$OS_AUTH_URL/users/not-a-uuid"   # 401

# make a non-admin (member) user + token
BOB_UID=$(openstack user create --project default --password Bob-1 bob -f value -c id)
MEMBER_RID=$(openstack role list -f value -c ID -c Name | awk '$2=="member"{print $1}')
openstack role add --project default --user "$BOB_UID" "$MEMBER_RID"
BOB_TOKEN=$(env OS_USERNAME=bob OS_PASSWORD=Bob-1 openstack token issue -f value -c id)

# non-admin + malformed id on an admin-gated route -> 403 (authz first, NOT 404)
curl -s -o /dev/null -w 'non-admin DEL -> %{http_code}\n' -X DELETE -H "X-Auth-Token: $BOB_TOKEN" "$OS_AUTH_URL/users/not-a-uuid"  # 403
# admin + malformed id on the same route -> 404 (id validated after authz)
curl -s -o /dev/null -w 'admin DEL     -> %{http_code}\n' -X DELETE "${H[@]}" "$OS_AUTH_URL/users/not-a-uuid"                     # 404
```
**Pass** = `401` unauthenticated, `403` for the non-admin, `404` for the admin — the
malformed-id 404 never pre-empts a 401/403.

---

## 5. Name→id resolution (the user-visible payoff)

```bash
# T2 - client
openstack project show default        # resolves by NAME (probe /projects/default → 404 → fall back to ?name=)
openstack project show not-a-uuid; echo "exit=$?"   # clean NotFound, NOT "HttpException 500"
openstack role add --project default --user admin admin   # references project/role/user by name
```
**Pass** = `openstack project show default` prints the project (name resolution works);
`openstack project show not-a-uuid` fails as **NotFound** (exit non-zero) rather than a
500 `HttpException`. Before the fix, the by-id probe 500'd and aborted these flows.

---

## 6. Automated tests (optional)

```bash
# T2 - client  (repo root; a DB is up from §1)
export LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'
go test ./internal/common/... ./internal/keystone/...     # DB-backed identity 404 tests
./test/keystone_name_resolution_test.sh                   # needs §1 lightstack running + §2 env
```
**Pass** = both green. (The DB-backed tests migrate + create/clean their own rows on the
`LIGHTSTACK_TEST_DB_URL` database.)

---

## 7. Teardown

```bash
# T1 - lightstack
# Ctrl-C the lightstack process (or: pkill -f 'bin/lightstack')

# T2 - client
make db-down                              # stop + remove the PostgreSQL container (drops all test data, incl. user bob)
rm -f ~/.lightstack-env                          # optional
```
**Pass** = the Lightstack process is stopped and `docker ps` shows no `lightstack-postgres`. Because
`make db-down` removes the database, the §4 `bob` user and any other test rows are gone —
no manual cleanup needed. (If you kept the DB, delete them: `openstack user delete bob`.)

---

## 8. Troubleshooting

- **Still getting `500` on a malformed id** → you're not running the branch build. Rebuild
  (`make build`) and confirm `bin/lightstack` came from a `#515` commit; restart §1.
- **`Failed to bind :35357: address already in use`** → a prior Lightstack already holds the port —
  commonly a **real-mode / `hostNetwork` Lightstack on k3s** from earlier host-testing (its process
  is `/app/lightstack` with a `kubepods…cri-containerd` cgroup; find it via `pgrep -af lightstack`). Scale it
  to 0 (`sudo k3s kubectl -n <ns> scale deploy/<lightstack> --replicas=0`) or `helm uninstall` it, then
  start the host binary.
- **`401 invalid credentials` from `openstack token issue`** even though `config/lightstack.yaml` has
  `admin_password: secret` → the DB's admin password was overwritten. Most often a **second Lightstack
  sharing this host Postgres** — a `hostNetwork` k3s deploy also reaches `127.0.0.1:5432` — has
  re-bootstrapped *its own* admin password into the shared DB. Stop that instance, then
  `make db-down && make db-up` so the host binary is the **sole writer** and re-seeds
  `admin`/`secret`. (Or simply an expired token — re-run §2.)
- **`curl: connection refused` to `:35357`** → Lightstack isn't up yet; watch the §1 log until
  Keystone is listening.
- **`openstack project show default` still shows `HttpException 500`** → the by-id probe
  is still 500ing — the fix isn't in the running binary (see the first item).
- **`go test` all `SKIP`** → `LIGHTSTACK_TEST_DB_URL` isn't exported (§6); the DB-backed tests
  skip without it.
