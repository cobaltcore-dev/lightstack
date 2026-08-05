# Quickstart: validate Nova server-detail read-back (F1)

**Feature**: `017-nova-server-detail-readback` | stub mode is sufficient for all of this.

## 0. Build & migrate

```bash
make build
make migrate            # applies migration 063 (instances.key_name)
make run                # starts Lightstack with config/lightstack.yaml (Nova on :8774)
source ~/.lightstack-env       # OS_AUTH_URL, admin/secret, project default
```

## 1. key_name round-trips (US1)

```bash
openstack keypair create --public-key ~/.ssh/id_rsa.pub demo-key
openstack server create --flavor m1.small --image cirros --key-name demo-key demo1
openstack server show demo1 -f json | jq '.key_name'          # → "demo-key"
openstack server list --long -f json | jq '.[0].key_name'     # list-detail agrees
```

A server created **without** `--key-name` must show `key_name: null`:

```bash
openstack server create --flavor m1.small --image cirros demo-nokey
openstack server show demo-nokey -f json | jq '.key_name'     # → null
```

## 2. security_groups round-trip (US2)

```bash
openstack server create --flavor m1.small --image cirros --security-group default demo2
openstack server show demo2 -f json | jq '.security_groups'   # → [{"name":"default"}]
```

A server created with **no** `--security-group` still shows the default group:

```bash
openstack server show demo1 -f json | jq '.security_groups'   # → [{"name":"default"}]
```

## 3. Create-time validation (FR-002a / FR-005a)

```bash
# unknown keypair → 400, no instance
openstack server create --flavor m1.small --image cirros --key-name nope bad1   # → 400 Invalid key_name provided.
# unknown security group → 400, no instance
openstack server create --flavor m1.small --image cirros --security-group nope bad2  # → 400 Security group nope not found.
openstack server list -f value -c Name | grep -E 'bad1|bad2'   # → (no output)
```

## 4. The F1 no-op (SC-001) — the whole point

```bash
cd test/terraform/compat
terraform init
terraform apply -auto-approve      # config sets key_pair + security_groups
terraform plan -detailed-exitcode  # EXPECT exit 0 = no changes (was: wanted to replace the VM)
terraform destroy -auto-approve
```

`terraform plan -detailed-exitcode` returns **0** when there are no changes, **2** when a diff is
proposed. Success criterion SC-001 = exit **0**.

## 5. Unit tests (US3 regression guard)

```bash
export LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack_test?sslmode=disable'
go test ./internal/nova/...        # read-back + 400-path tests (skip if LIGHTSTACK_TEST_DB_URL unset)
```

> CI note: the `unit-tests` job must export `LIGHTSTACK_TEST_DB_URL` (it already has a Postgres service)
> or these tests silently skip and the guard is inert — see research.md D9.
