# Quickstart / Validation: Ensure Node-Local Storage Directories Exist in Real Mode

**Feature**: `011-ensure-storage-subdirs` · **Date**: 2026-06-25

How to validate the fix. CI gates run the unit + chart tests; the host e2e requires a Linux node
with nested KVM and privileged root (per SPEC-009 HOST-TESTING).

## 1. Unit + chart tests (CI-runnable)

```bash
# Binary: Glance local store resolves/creates the canonical dir, never ~/.o3k
go test -race ./pkg/storage/...

# Chart: real-mode render has the init container; stub render does not
helm unittest charts/o3k

# Full gates (as CI)
go build ./... && golangci-lint run && go test -race ./...
```

**Expect**: `image_store_test.go` and `init_host_storage_test.yaml` pass; everything else stays green.

### TDD order (Constitution: Test-First)

1. Add `image_store_test.go` (C-BIN-1/2/3) and `init_host_storage_test.yaml` (C-CHART-1/2/3) →
   **confirm RED** (fallback still present; no initContainer rendered).
2. Apply Layer B (`image_store.go`) and Layer A (`_helpers.tpl`) → **GREEN**.

## 2. Host end-to-end (fresh deploy — SC-001/002/005)

On a clean node where `/var/lib/o3k` does **not** exist, with **no** manual `mkdir`:

```bash
# Deploy real mode (single-node), e.g.:
helm install o3k charts/o3k \
  --set realMode.enabled=true \
  --set glance.storageMode=local \
  --set nova.libvirtMode=real \
  # … remaining real-mode values per HOST-TESTING.md

# Subdirs exist immediately after the pod starts (initContainer ran):
ls /var/lib/o3k            # images volumes instances cloud-init dhcp

source ~/.o3k-env
openstack image create cirros --disk-format qcow2 --container-format bare --file cirros.img
openstack server create --flavor m1.small --image cirros --network <net> vm0 --wait
openstack server show vm0 -f value -c status      # => ACTIVE  (was: ERROR)

# Base image is at the canonical path, NOT the pod-ephemeral fallback:
ls /var/lib/o3k/images/    # image-<id>.raw present
ls ~/.o3k/images 2>/dev/null || echo "no ~/.o3k fallback — correct"
```

**Pass**: VM is `ACTIVE`, base image under `/var/lib/o3k/images`, no manual directory creation at
any step.

## 3. Redeploy after teardown (idempotence — SC-003 / US3)

```bash
helm uninstall o3k                          # (or run the teardown feature)
sudo rm -rf /var/lib/o3k/*                   # simulate trees wiped
# redeploy exactly as in step 2 — no manual mkdir
openstack server create … vm1 --wait
openstack server show vm1 -f value -c status # => ACTIVE
```

**Pass**: subdirs recreated automatically; VM reaches `ACTIVE` on the first attempt.

## 4. Stub unaffected (SC-006 / FR-009)

```bash
helm template o3k charts/o3k --set realMode.enabled=false | grep -A3 initContainers || \
  echo "no initContainers in stub render — correct"
```

**Pass**: stub render has no initContainer and existing stub tests stay green.

## 5. Docs cleanup (SC-004)

Confirm the manual workaround is gone from:
`docs/specs/009-single-node-real-mode/HOST-TESTING.md`, `docs/SINGLE_NODE_DEPLOYMENT.md`,
`docs/STORAGE_MODES.md`, `deployments/README-KVM.md`.

```bash
# exclude this feature's own planning docs, which mention the workaround by name
grep -rn 'mkdir -p /var/lib/o3k\|mkdir -p ~/.o3k' docs deployments \
  | grep -v '011-ensure-storage-subdirs' \
  && echo "WORKAROUND STILL PRESENT" || echo "workaround removed — correct"
```
