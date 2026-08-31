# Quickstart: Nova Gaps & Client Discovery

**Feature**: `025-nova-gaps-client-discovery` | **Date**: 2026-08-31

How to exercise each story. Shell blocks are zsh-copy-paste-safe: no line continuations, no history
expansion, no inline comments.

## Which stories need which mode

| Story | Stub | Real mode |
|---|---|---|
| US1 discovery, US2 identity links, US5 action by name, US7 usage/history, US8 log | yes | not needed |
| US3 boot failure | pre-flight half | full path |
| US4 stop/start + self-heal | transitions only | required |
| US6 floating IPs in addresses | no | required (router + external network + FIP) |
| US9 boot from new volume | volume row only | required |

## Stub stack

```
export HORIZON_HTTP_PORT=8080
export LIGHTSTACK_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
T=$(openstack token issue -f value -c id)
```

> Check the shared PostgreSQL first. One container is shared across worktrees, and a leftover
> `quotas.instances=0` for `default` makes the compute contract suite fail for reasons unrelated to any
> code. Verify quotas and the project list before trusting a red run.

## US1 — discovery and self-links

```
curl -s localhost:8774/v2.1 | jq '.version.links'
openstack flavor list
curl -s localhost:8776/v3 | jq -r '.version.links[0].href'
curl -s localhost:9696/v2.0 | jq -r '.version.links[0].href'
```

Expected: a `self` href on the compute version document; five flavors listed with no
`OS_COMPUTE_API_VERSION` set; the volume and network hrefs absolute and naming *your* host, not `lightstack` and
not a bare path.

Endpoint override, the half that is B3 proper:

```
OS_COMPUTE_ENDPOINT_OVERRIDE=http://127.0.0.1:8774/v2.1 openstack server list
```

Expected: the override is the host contacted. To prove it is honoured rather than coincidentally right,
run it against a host reachable under two names.

## US2 — identity read-back

```
which -a openstack
openstack project show default -f value -c id
openstack user show admin -f value -c id
openstack role show admin -f value -c id
curl -s "$OS_AUTH_URL/projects" -H "X-Auth-Token: $T" | jq '.projects[0] | has("links")'
```

Expected: all three exit 0 and print an id; the list entry has `links`. The failure only appears on the
6.6.0 client that ships with Debian/Ubuntu, so check `which -a` first. Then re-run SPEC-023's real-mode
fixture block and confirm the identifiers are non-empty.

## US3 — a boot that cannot get its network

Pre-flight half, stub mode:

```
openstack server create --flavor m1.small --image cirros --network 00000000-0000-0000-0000-000000000000 probe-net
```

Expected: refused with a message naming the network, and no server created. Confirm with
`openstack server list`.

Post-accept half needs a real stack with a port failure induced:

```
openstack server show probe -f value -c status
openstack server show probe -f json | jq '.fault'
```

Expected: not `ACTIVE`, and `fault.message` says why. Today the status is `ACTIVE` and `addresses` is
empty with nothing explaining it.

## US4 — stop, start, and self-heal

```
openstack server stop probe
openstack server show probe -f value -c status -c power_state
openstack server start probe
openstack server show probe -f value -c status -c power_state
virsh -c qemu:///system list --all
```

Expected: `SHUTOFF` then `ACTIVE`, agreeing with `virsh` after each. Repeat ten times.

Self-heal, out-of-band removal:

```
DOM=$(openstack server show probe -f value -c id)
virsh -c qemu:///system destroy "$DOM"
virsh -c qemu:///system undefine "$DOM"
openstack server start probe
openstack server event list probe
```

Expected: the start succeeds, and the `os-start` event carries a message recording the repair.

> Use the libvirt URI Lightstack itself connects to, not the host default. Research R2 lists this as the leading
> hypothesis for the original disappearance, so a mismatch here is a finding, not a test error.

## US5 — an action addressed by name

Must be driven directly: the CLI resolves names before the request leaves the machine.

```
openstack server create --flavor m1.small --image cirros b14-probe
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"suspend": null}' localhost:8774/v2.1/servers/b14-probe/action
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"suspend": null}' localhost:8774/v2.1/servers/no-such-server/action
```

Expected: `202` then a not-found status. Today the first is `500`.

## US6 — floating IPs in the address map

```
openstack server add floating ip probe 192.0.2.100
openstack server show probe -f json | jq -c '.addresses'
openstack server list -f json | jq -c '.[0].Networks'
openstack floating ip list -f value -c "Floating IP Address" -c Status
```

Expected: both addresses under the network, the floating one tagged `floating`, and the single read and
the listing agreeing.

## US7 — usage counts and action history

```
openstack limits show --absolute -f value -c Name -c Value | grep -i instances
openstack quota show default -f value -c instances
SRV=$(openstack server create --flavor m1.small --image cirros hist-probe -f value -c id)
openstack server delete hist-probe
curl -s -H "X-Auth-Token: $T" "localhost:8774/v2.1/servers/$SRV/os-instance-actions" | jq '.instanceActions | length'
```

Expected: the two usage figures agree; the deleted server's history is still returned to its owner.
As another project's user, the same request reports the server absent.

Reclamation, with the retention age set short in `lightstack.yaml`:

```
psql "$LIGHTSTACK_DB_URL" -c "SELECT count(*) FROM instance_actions WHERE start_time < now() - interval '1 hour';"
```

Expected: the count falls to zero within one reconciliation interval, while a live server's older records
remain.

## US8 — access-log attribution

As an administrator, act on another project's server by id, then read the service log.

```
kubectl logs deploy/lightstack | jq -c 'select(.message == "request completed") | {user_id, project_id, path}' | tail -5
```

Expected: `project_id` is the operator's own project; the target project remains identifiable. Today it
is the target's.

## US9 — boot from a new volume

```
openstack server create --flavor m1.small --image cirros --boot-from-volume 25 vol-probe
openstack volume list -f value -c ID -c Status -c Bootable -c Size
openstack server show vol-probe -f value -c status
openstack server delete vol-probe
openstack volume list -f value -c ID -c Status
```

Expected: a bootable 25 GiB volume, in use, attached as the root disk; the server reaches `ACTIVE`; the
volume is gone after deletion when delete-on-termination was requested, and `available` when it was not.

Omitting the size must yield the flavor's disk size, floored at the image's minimum, and never zero.

## Gates

```
make fmt
make lint
go test ./...
go test ./test/contract/nova/... ./test/contract/keystone/...
```

Both migrations must apply and roll back:

```
make migrate
```

## Teardown

```
docker compose -f deployments/docker-compose-horizon.yml down -v
```
