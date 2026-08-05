# Quickstart — validating the Horizon compat fixes (F4, F5, F6, F9)

Fast CLI validation of the four fixes. Run the OpenStack CLI against a running O3K (source `~/.o3k-env` or
your env first). Full runbook validation is the SPEC-018 `HOST_TESTING.md` (stub) and
`HOST_TESTING_REAL_MODE.md` (real) passes — this is the 60-second smoke.

Shell blocks are zsh-copy-paste-safe (one command per line, no continuations).

## Unit tests (fastest, both modes covered by stub-level tests)

```
make test
go test ./internal/cinder/... ./internal/neutron/... ./internal/glance/... ./internal/keystone/...
```

## F9 — Volumes render once a volume exists

```
openstack volume create --size 1 qs-vol
openstack volume show qs-vol -f json
```

Expect: `volume_type` = `__DEFAULT__`, `availability_zone` = `nova`, and non-null `encrypted`,
`multiattach`, `user_id`, `os-vol-tenant-attr:tenant_id`, `os-vol-host-attr:host`, `consumes_quota`,
`shared_targets`, `metadata`. Then open Horizon → Project → Volumes: the panel loads (no HTTP 500).

## F5 — Block-storage and S3 catalog resolve to the real host

Run against a deployment with a non-localhost `O3K_ENDPOINT_HOST`.

```
openstack catalog show volume -f json
openstack catalog show volumev3 -f json
openstack catalog show s3 -f json
```

Expect: no advertised URL contains `localhost`; all use the configured host.

## F4 — Each network is listed once

```
openstack network create qs-net
openstack network list -f json
openstack network list --long -f json
```

Expect: `qs-net` appears exactly once; each network shows `router:external` = `False`. In Horizon →
Project → Network → Networks the network appears once (not twice).

## F6 — Deleting an image removes it

```
openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/qs.img qs-img
openstack image delete qs-img
openstack image list -f json
```

Expect: `qs-img` no longer listed. Deleting a nonexistent id returns Not Found (404); a non-owner
non-admin deleting a public image is Forbidden (403).

## Both modes

Re-run the four blocks under the stub stack (`deployments/docker-compose-horizon.yml`) and the real stack
(k3s + `charts/o3k` real mode + `deployments/horizon-chart/`, Ceph RGW via `deployments/rook/setup-rook-rgw.sh`),
per the two SPEC-018 runbooks.
