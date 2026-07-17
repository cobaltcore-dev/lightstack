# SPEC-016 — Verify Terraform OpenStack-provider compatibility

Tracking: [cloud-storage#481](https://github.com/cobaltcore-dev/cloud-storage/issues/481).
Direct implementation (no speckit): a Terraform harness + a pass/fail report.

**Goal.** Prove o3k's "drop-in Terraform" claim for the resources on the validation path —
**compute instances, networks, security groups, volumes** — by driving the *unmodified*
`terraform-provider-openstack` through a full `apply` → `destroy`. Object storage is out of
scope: o3k exposes no Swift API (see o3k#15).

## Contents / where things live

| Artifact | Path | Tracked |
|---|---|:--:|
| Terraform manifests (4 AC categories) | `test/terraform/compat/` | ✅ |
| Stub-mode host runbook (k3s + Helm) | `HOST_TESTING.md` | ✅ |
| Real-mode host runbook (k3s + node-local storage) | `HOST_TESTING_REAL_MODE.md` | ✅ (optional; does not gate the ticket) |
| Pass/fail report (AC4) | `docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md` | ✅ (written from host results) |

## Flow

1. Follow `HOST_TESTING.md` on a host → paste the results.
2. The report in `docs/compatibility/` is written from those results (per-resource:
   Terraform type → OpenStack endpoint → working / not working).
3. `HOST_TESTING_REAL_MODE.md` is an optional side-quest to confirm the same manifests drive a
   real-mode o3k; it does not gate the ticket.

The `sc007` module (`test/terraform/sc007/`, SPEC-009) remains the real-mode *deep* scenario
(router + floating IP + SSH key/user-data verification); the `compat/` module here is the
lean, mode-agnostic smoke that covers #481's exact category list.
