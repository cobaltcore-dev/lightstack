# Terraform OpenStack-provider compatibility report

**Ticket**: [cloud-storage#481](https://github.com/cobaltcore-dev/cloud-storage/issues/481) ·
**Date**: 2026-07-17 ·
**Verdict**: ✅ **PASS** for all four in-scope resource categories (one documented, non-blocking
read-back deviation on compute instances).

Verifies Lightstack's "drop-in Terraform" claim by driving the **unmodified**
`terraform-provider-openstack` through a full `terraform apply` → `terraform destroy` against a
chart-deployed Lightstack. Scope is #481's validation set: **compute instances, networks, security
groups, volumes**. Object storage is **N/A** — Lightstack exposes no Swift API (see
[lightstack#15](https://github.com/cobaltcore-dev/lightstack/issues/15)).

## Environment

| Component | Value |
|---|---|
| Lightstack | built from `feature/481` (PR lightstack#75); Helm chart `lightstack-0.2.0` (appVersion 0.7.0) |
| Mode | **stub** (`libvirt/networking/securityGroup/cinder/glance` all `stub`) |
| Deployment | SPEC-008 Helm chart on **k3s**, external PostgreSQL, `ClusterIP` + `kubectl port-forward` |
| Terraform | v1.15.8 |
| Provider | `terraform-provider-openstack/openstack` `~> 3.0` (resolved **v3.4.0**), **unmodified** |
| Client | python-openstackclient 10.0.0 |

_Point-in-time snapshot: re-run the harness (`test/terraform/compat/`) if Lightstack has changed since._

Stub mode is the correct fidelity for a **provider-compatibility** check: it exercises the full
API surface (request/response shapes, status fields the provider polls, catalog wiring) without
libvirt/KVM. Real VMs and real Linux networking are a separate concern,
covered by the `sc007` real-mode scenario and the optional real-mode runbook
(`docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING_REAL_MODE.md`).

## Method

One self-contained module — [`test/terraform/compat/`](../../test/terraform/compat/) — creates
9 resources across the four categories in a single `apply`, then `destroy`s them. Reproduce with
[`docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING.md`](../specs/016-terraform-openstack-provider-verification/HOST_TESTING.md).
Result recorded from a live host run **and** independently reproduced against a bare-binary stub
Lightstack. "Working" = the provider's create + read + delete all succeed with no provider modification.

## Results — per resource

| Terraform resource | OpenStack service — endpoint | Create | Read | Destroy | Result |
|---|---|:--:|:--:|:--:|:--:|
| `data.openstack_images_image_v2` | Glance — `GET /v2/images` | — | ✅ | — | **Working** |
| `openstack_networking_network_v2` | Neutron — `POST/GET/DELETE /v2.0/networks` | ✅ | ✅ | ✅ | **Working** |
| `openstack_networking_subnet_v2` | Neutron — `.../v2.0/subnets` | ✅ | ✅ | ✅ | **Working** |
| `openstack_networking_secgroup_v2` | Neutron — `.../v2.0/security-groups` | ✅ | ✅ | ✅ | **Working** |
| `openstack_networking_secgroup_rule_v2` | Neutron — `.../v2.0/security-group-rules` | ✅ | ✅ | ✅ | **Working** |
| `openstack_compute_keypair_v2` | Nova — `.../v2.1/os-keypairs` | ✅ | ✅ | ✅ | **Working** |
| `openstack_compute_instance_v2` | Nova — `POST/GET/DELETE /v2.1/servers` | ✅ | ⚠️ | ✅ | **Working\*** |
| `openstack_blockstorage_volume_v3` | Cinder — `POST/GET/DELETE /v3/volumes` (Lightstack also serves the project-scoped `/v3/{project_id}/volumes`) | ✅ | ✅ | ✅ | **Working** |
| `openstack_compute_volume_attach_v2` | Nova — `.../v2.1/servers/{id}/os-volume_attachments` | ✅ | ✅ | ✅ | **Working** |

\* create/read/delete all succeed and the VM reports `ACTIVE` with a fixed IP; the read-back is
partial — see Finding F1.

### Against #481's acceptance categories

| #481 category | Result |
|---|:--:|
| Compute instances | ✅ Working (create/read/delete); re-plan is a no-op — F1 resolved (SPEC-017) |
| Networks | ✅ Working |
| Security groups | ✅ Working |
| Volumes | ✅ Working |
| Object storage | ⛔ N/A — no Swift API (lightstack#15) |

## Lifecycle evidence

```
terraform apply   → Apply complete!  Resources: 9 added, 0 changed, 0 destroyed.
terraform destroy → Destroy complete! Resources: 9 destroyed.
residue (server/network/security group/volume/keypair) → 0 / 0 / 0 / 0 / 0
```

Observed during the run: instance `ACTIVE` with `access_ip_v4 = 10.10.0.246`; volume `in-use`,
attached at `/dev/vdb`; security group carried the 2 authored rules (ssh/tcp/22, icmp) plus the
3 project defaults; keypair generated and injected. All created with the **unmodified** provider
— no overrides, no patches.

**Wire-level confirmation** (Lightstack access log): every resource showed the provider's
create → read → `DELETE` → poll-`GET`-until-`404` cycle with correct status codes throughout
(`200` reads, `201` token, `202`/`204` deletes, `404` after deletion), confirming both the CRUD
paths and delete-confirmation semantics the provider relies on.

## Findings

### F1 — `openstack_compute_instance_v2` is not idempotent on re-plan · Minor · ✅ Resolved (SPEC-017 / #524)

After a successful `apply`, a second `terraform plan` reports the instance **must be replaced**:

```
# openstack_compute_instance_v2.vm must be replaced
    + key_pair = "tf-compat-key" # forces replacement
# openstack_compute_volume_attach_v2.va must be replaced   (cascades from the instance id)
```

**Cause.** Lightstack's Nova server-detail (`GET /v2.1/servers/{id}`) omits `key_name` and returns an
empty `security_groups`. The provider reads those back as "unset", computes a diff on the
force-new `key_pair` attribute, and plans a replacement.

**Impact.** `apply` and `destroy` are correct (the AC). The gap only bites a **repeated** apply
on the same state: it would recreate the VM (and its volume attachment) instead of no-op'ing.
Single-shot create/destroy pipelines are unaffected.

**Recommendation (out of scope for #481, which is verify-only).** Populate `key_name` and
`security_groups` in the Nova server-detail response to make re-apply a true no-op. This is the
same class of gap as the documented port-id deviation in
[`docs/blueprint.md` §6](../blueprint.md); tracked as a follow-up, not a blocker.

**Resolved — SPEC-017 (#524).** Nova server-detail (`GET /v2.1/servers/{id}` **and**
`/servers/detail`) now returns `key_name` and `security_groups`. The estimate above ("≈1 line
each") proved optimistic: neither value was persisted, so the fix also persists them at create —
`key_name` via migration `063` (`instances.key_name`), security groups via `server_security_groups`
— validates them synchronously (unknown keypair / non-default group → `400`, matching Nova v2.1),
ensures the project `default` group on demand, and defaults `security_groups` to
`[{"name":"default"}]`. A repeated `terraform plan` is now a no-op — confirmed on a host run
(2026-07-21, all-stub mode, unmodified provider 3.4.0): `apply` → `plan -detailed-exitcode` **0** →
`destroy`. Covered by DB-backed unit tests in `internal/nova` that assert both fields on show +
list-detail and run in CI. Real mode is not covered by that run.

## Notes & scope

- **Object storage** is deliberately excluded: Terraform's `openstack_objectstorage_*` is
  Swift-based and Lightstack exposes no `object-store` service (tenant object storage exists via S3 /
  Ceph RGW + EC2 creds — SPEC-014 — but not through this provider). See lightstack#15.
- **Regression coverage.** The full terraform-in-CI smoke still runs against a live instance and
  remains a candidate for a dedicated CI "Terraform validation" job
  ([`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)). In the meantime, the F1 fields
  are guarded by DB-backed unit tests in `internal/nova` (`server_detail_keyname_test.go`,
  `server_detail_secgroups_test.go`) that already run in the CI `unit-tests` job (SPEC-017 US3).
- **Complementary coverage.** [`test/terraform/sc007/`](../../test/terraform/sc007/) (SPEC-009)
  is the real-mode *deep* scenario — router + floating IP + FIP association + SSH-verified key /
  user-data injection — validating what stub mode can't (actual boot, L3/NAT).

## Conclusion

For every resource on #481's validation path, the unmodified Terraform OpenStack provider drives
Lightstack through a clean create-and-destroy with no modification. The "drop-in Terraform" claim holds
for compute, networks, security groups, and volumes. The single caveat (F1) affects only
repeated applies and has a small, well-understood fix. Object storage remains the one category
that cannot be served through this provider, by design.
