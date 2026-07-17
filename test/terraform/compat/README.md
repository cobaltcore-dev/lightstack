# compat/ — Terraform provider-compatibility smoke (SPEC-016 / cloud-storage#481)

The mode-agnostic smoke that drives the unmodified `terraform-provider-openstack` through a
full `apply` → `destroy` covering #481's four categories: **compute instances, networks,
security groups, volumes**. See `main.tf` for the resource list.

Run it via the host runbook (setup, expected outcomes, teardown):
`../../../docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING.md`.

Prerequisites the manifests assume: a live Lightstack, a Glance image named `cirros`, and a flavor
named `m1.small` (seeded). Auth via the standard `OS_*` environment variables.

The keypair is declared without `public_key`, so the provider generates one and stores the
**private key unencrypted** in `terraform.tfstate` — upstream warns about this on
`openstack_compute_keypair_v2`. It is git-ignored (never committed) and the runbook teardown
deletes `terraform.tfstate*`, but treat the file as a secret while a run is in progress,
particularly on a shared host.
