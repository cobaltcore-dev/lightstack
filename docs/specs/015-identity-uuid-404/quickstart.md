# Quickstart: Identity resource not-found for malformed ids

## Prerequisites

- o3k running (`make run` or docker compose), admin creds sourced (`~/.o3k-env`).
- For DB-backed tests: `export O3K_TEST_DB_URL=postgres://…` (a throwaway DB — migrations are applied).

## 1. Reproduce the bug (RED baseline)

```bash
export OS_AUTH_URL=http://127.0.0.1:35357/v3
TOKEN=$(openstack token issue -f value -c id)

# BUG today: non-UUID id → 500 (expected 404)
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" "$OS_AUTH_URL/projects/not-a-uuid"   # 500
# Control: valid-but-unknown UUID already 404
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" "$OS_AUTH_URL/projects/00000000-0000-0000-0000-0000000000ff"  # 404
# Spot-check other groups (all 500 today): roles, groups, services, credentials, role-assignment path
for p in roles/x groups/x services/x credentials/x projects/x/users/y/roles/z; do
  curl -s -o /dev/null -w "%{http_code} $p\n" -H "X-Auth-Token: $TOKEN" "$OS_AUTH_URL/$p"
done
```

## 2. Run the tests

```bash
# Middleware unit tests (no DB)
go test ./internal/keystone/ -run UUIDParam -race

# DB-backed e2e (requires O3K_TEST_DB_URL)
go test ./internal/keystone/ -run NotFound -race

# Full gates
make test && make lint && make build
```

Expected after implementation: malformed id → **404** with `"Could not find <resource>: <id>."`;
valid-unknown → identical 404; valid-existing → 200/204; non-admin+malformed on gated routes → **403**;
unauthenticated → **401**; a simulated genuine DB error → **5xx**.

## 3. Verify the fix + conformance

```bash
# Ticket repro now returns 404
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" "$OS_AUTH_URL/projects/not-a-uuid"   # 404

# CLI name resolution (the user-visible payoff)
openstack project show not-a-uuid            # => NotFound (not HttpException 500)
openstack project show <existing-name>       # => resolves by name
openstack role add --user <name> --project <name> <role-name>   # => succeeds

# (optional) message parity vs reference Keystone / DevStack
#   compare body text per resource against exception.py *NotFound messages
```

## 4. Regression checks

```bash
./test/quick_test.sh          # keystone-inclusive quick suite
# existing identity flows (create/show/update/delete by valid id) unchanged
```

## Done when

- SC-001..005 hold (see spec): 100% malformed→404 / 0% 500 across the surface; CLI+Terraform name resolution works; status+message match reference Keystone; no regression; masked-fault 5xx restored on touched handlers.
