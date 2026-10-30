# Terraform compatibility tests

Validates that the **unmodified** Terraform OpenStack provider works against Lightstack. Each
scenario is a self-contained module:

```text
test/terraform/
├── run.sh                # SPEC-028 (issue #105): the conformance wrapper — run the compat
│                         #   module end-to-end (init → validate → apply → destroy) with
│                         #   clean-state assertions. What `make test-terraform` runs.
├── compat/               # SPEC-016 (cloud-storage#481): the mode-agnostic compatibility smoke —
│                         #   compute instances, networks, security groups, volumes. Runs against a
│                         #   stub OR real lightstack. This is the #481 harness.
└── sc007/                # SPEC-009 (SC-007 / FR-018): the real-mode DEEP scenario — instance +
                          #   keypair + user_data + router + floating IP + attached volume, SSH-verified.
```

## Running the conformance wrapper

`run.sh` automates the manual host runbook
(`docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING.md`) against an
already-running Lightstack. It is rerun-safe and leaves nothing behind: the module runs in a
`mktemp` workspace copy (never in the repo tree), and everything it creates in Lightstack is
cleaned up at exit.

```bash
make test-terraform          # from the repo root
# or equivalently:
bash test/terraform/run.sh
```

### Prerequisites

- A **live Lightstack** (either mode). By default the wrapper authenticates against
  `http://localhost:35357/v3` as `admin/secret`, project `default`, domain `Default` — override
  with the standard `OS_*` environment variables. The provider resolves every service endpoint
  from the Keystone catalog, so `OS_AUTH_URL` is the only endpoint plumbing needed.
- **Terraform**, pinned to the version in `run.sh`'s `TF_VERSION_PIN` (currently **1.13.4**).
  The preflight prints an install hint if it is missing.
- The **OpenStack CLI** for prerequisite setup (image/flavor); set `OPENSTACK_CLI` if it is not
  on `PATH` (e.g. `/opt/osc-venv/bin/openstack`).
- **curl**, **jq**, and **timeout** (GNU coreutils) on `PATH`.
- Network access to the public Terraform registry on **first** run only (provider download);
  afterwards the shared `TF_PLUGIN_CACHE_DIR` (default `~/.terraform.d/plugin-cache`) serves it.

The wrapper creates the Glance image the module needs (a 64KiB dummy, raw/bare — stub Glance
stores metadata only) under a unique `tf-cirros-*` name and deletes it at exit. Set
`TF_IMAGE_NAME` to use a specific image name instead (e.g. `cirros`, the module's
`variables.tf` default); it is then only created if missing and only deleted if this run
created it. The `m1.small` flavor is seeded by Lightstack itself.

### What it asserts

A run passes (exit 0) iff **all** of the following hold:

| Step | Assertion |
|---|---|
| preflight | terraform present, `OS_AUTH_URL` + credentials work, every catalog endpoint reachable |
| `terraform init` | provider installs from the public registry, unmodified |
| `terraform validate` | module parses and validates |
| `terraform apply` | all 9 resources created (network, subnet, SG + 2 rules, keypair, instance, volume, attach) |
| re-plan after apply | `-detailed-exitcode` **0** — no drift; catches read-back regressions like SPEC-017/#524 |
| `terraform destroy` | all 9 resources destroyed |
| `terraform state list` | empty after destroy |
| `terraform output` | no outputs after destroy |
| OpenStack CLI residue check | zero `tf-compat-*` servers/networks/subnets/SGs/volumes/keypairs remain — the authoritative proof that destroy actually cleaned Lightstack (an empty state alone cannot show residue) |
| post-destroy plan | `-detailed-exitcode` 0, or exit 2 with **adds only** — an empty state always re-proposes the 9 creates, so a bare "exit 0" is unattainable there; any to-change/to-destroy, or CLI residue, is a failure |

### Pinning policy

- **Terraform**: pinned in `run.sh` (`TF_VERSION_PIN`, currently 1.13.4). Bump deliberately —
  a pin change is a harness change, not a drive-by.
- **Provider**: pinned in `compat/provider.tf` `required_providers` (`~> 3.0`, the 3.x
  series). The exact resolved version is printed in the run summary.
- The lockfile lives only in the per-run temp workspace (each run inits fresh), so pins are
  enforced by the version constraints above, not by a committed lockfile.

### CI

A **nightly workflow job** (`.github/workflows/nightly.yml`) runs `make test-terraform`
against a fresh stub-mode Lightstack deployment with only the wrapper's own image present.
The wrapper is not part of the per-PR CI gate because it needs a running server.

PR CI exercises the wrapper's evidence gates with fake external clients:

```bash
python3 -m unittest discover -s test/terraform -p 'test_*.py' -v
```

These regression tests check that failed catalog/resource listings and unreadable
Terraform state fail the run, and that leaked resources (including nameless floating
IPs captured from state) cannot produce a successful cleanup report.

## Known divergences

None currently. The one this wrapper used to carry — Glance ignoring the `?name=`
list filter, which broke the provider's `data.openstack_images_image_v2` with
`Your query returned more than one result` on any multi-image server — is fixed
(`internal/glance/images.go` implements the filter, guarded by
`image_list_filters_test.go`); the wrapper's apply-failure hint was reworded to
say exactly that: a "more than one result" failure now means genuinely duplicate
image names, not the ignored filter.

## The modules

- **`compat/`** — step-by-step host runbook (stub, k3s + Helm) with expected outcomes:
  `../../docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING.md`.
  A run passes if `apply` and `destroy` both succeed with **no provider modification** and all
  four resource categories are created and cleaned up. The wrapper above is the automated form
  of that runbook; the manual steps remain valid for first-time bring-up.
- **`sc007/`** — real-mode deep scenario; see
  `../../docs/specs/009-single-node-real-mode/quickstart.md` and its contracts under
  `../../docs/specs/009-single-node-real-mode/contracts/`.

To run the compat module by hand instead (e.g. while iterating on the module itself), the
runbook's env pattern works:

```bash
source ~/.lightstack-env   # OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_NAME, ...
cd test/terraform/compat
terraform init
terraform apply -auto-approve
terraform destroy -auto-approve
```

Working state (`.terraform/`, `*.tfstate`, lockfile) is git-ignored — see
`test/terraform/.gitignore` — but prefer the wrapper: it never touches the repo tree at all.

The compat module's keypair is declared without `public_key`, so the provider generates one
and stores the **private key unencrypted** in `terraform.tfstate` — upstream warns about this
on `openstack_compute_keypair_v2`. The wrapper's temp workspace (and its state) is deleted at
exit, but treat any by-hand `terraform.tfstate` as a secret while a run is in progress,
particularly on a shared host.
