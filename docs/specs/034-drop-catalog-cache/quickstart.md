# Quickstart: Stop Caching the Service Catalog

All commands from the repository root. Test database per the project guide: `make db-up` for
PostgreSQL. Never a shared, long-lived database (stale state fakes failures).

## 1. Unit tests, both backends

```zsh
export LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'
go test ./internal/keystone/ -run 'TestCatalog|TestBuildServiceCatalog' -count=1 -v
make test
mkdir -p /tmp/ls-test && export LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db'
go test ./internal/keystone/ -run 'TestCatalog|TestBuildServiceCatalog' -count=1 -v
make test
```

**RED (before the change, with the cache-on variant present)**: `TestCatalogFollowsIdentityAPIWrites`
cache-on subtest fails on the created endpoint, the deleted endpoint and the scheme. The cache-off
subtest passes.
**GREEN (after)**: the cache-on variant no longer exists. `TestCatalogListsEndpointCreatedAfterIssue`,
`TestCatalogDropsDeletedEndpoint` and `TestCatalogFollowsSchemeChange` pass, and `grep -n miniredis go.mod`
prints nothing.

```zsh
grep -rn service_catalog --include='*.go' .      # prints nothing (SC-003)
grep -rn catalogCacheable --include='*.go' .     # prints nothing
make fmt && make lint
```

## 2. Compose stacks

```zsh
docker compose -f deployments/docker-compose.yml up -d --build
docker compose -f deployments/docker-compose.yml ps          # no redis service; all healthy
grep -nE 'redis|CACHE_ENABLED' deployments/docker-compose.yml deployments/docker-compose-sqlite.yml   # prints nothing
docker compose -f deployments/docker-compose.yml down -v
docker compose -f deployments/docker-compose-sqlite.yml up -d --build
docker compose -f deployments/docker-compose-sqlite.yml ps
docker compose -f deployments/docker-compose-sqlite.yml down -v
```

With the environment from `~/.lightstack-env`, `openstack token issue` succeeds on each.

## 3. Host check with a real Redis (cache on)

`config/lightstack.yaml` with `cache.enabled: true` and `cache.redis_url` pointing at a running
Redis (for example `docker run -d --rm -p 6379:6379 --name ls-redis redis:7-alpine`). The compose
environment variables do not enable it.
Address the deployment as `localhost` (`OS_AUTH_URL=http://localhost:35357/v3`), or set
`LIGHTSTACK_ENDPOINT_HOST` to the name you use: before the change only the configured host's catalog
is cached, so the "before" run shows nothing stale through any other name.

```zsh
make build && ./bin/lightstack -config config/lightstack.yaml
openstack token issue -f value -c id
openstack service create --name probe581 probe581
openstack endpoint create probe581 public http://localhost:8999/v1
openstack catalog show probe581 -f value -c endpoints     # lists http://localhost:8999/v1 at once
T=$(openstack token issue -f value -c id)
E=$(openstack endpoint list --service probe581 -f value -c ID)
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "X-Auth-Token: $T" "http://localhost:35357/v3/endpoints/$E")
echo "$code"                                              # 204
openstack catalog show probe581 -f value -c endpoints     # []
openstack service delete probe581
docker exec ls-redis redis-cli --scan --pattern '*service_catalog*'   # prints nothing
```

Two client commands do not work against Lightstack, for reasons unrelated to this feature. `--region
RegionOne` fails because `/v3/regions` is not routed. `openstack endpoint delete` first sends
`GET /v3/endpoints/{id}`, which is not routed either (405), so the delete is sent with curl.

On the commit *before* the change, the created endpoint is missing only because the cache was warmed
before the write. To see a stale *delete* there, warm it after the create:
`redis-cli FLUSHALL`, then `openstack token issue`, then delete.

Then restart with `LIGHTSTACK_ENDPOINT_SCHEME=https` and run `openstack catalog list -f json`.
Every URL except the `s3` entry is `https://`. On the commit before the change, both the catalog
checks and the scheme check show the old catalog. That is the manual before/after.

## 4. Contract suites

Run the contract suites as documented in `test/contract/`. Expect no new failures against the
known-failures list.
