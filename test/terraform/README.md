# Terraform compatibility tests

Validates that the **unmodified** Terraform OpenStack provider works against o3k. Each
scenario is a self-contained module:

```text
test/terraform/
├── provider.tf.example   # provider block, for reference (each module ships its own provider.tf)
├── compat/               # SPEC-016 (cloud-storage#481): the mode-agnostic compatibility smoke —
│                         #   compute instances, networks, security groups, volumes. Runs against a
│                         #   stub OR real o3k. This is the #481 harness.
└── sc007/                # SPEC-009 (SC-007 / FR-018): the real-mode DEEP scenario — instance +
                          #   keypair + user_data + router + floating IP + attached volume, SSH-verified.
```

## Running

Both modules run against a **live o3k** and are **not** CI-gated (they need a running
instance). `compat/` is the one to run for #481:

```bash
source /tmp/o3k-481-env.sh   # OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_NAME, ...
cd test/terraform/compat
terraform init
terraform apply -auto-approve
terraform destroy -auto-approve
```

- **`compat/`** — step-by-step host runbook (stub, k3s + Helm) with expected outcomes:
  `../../docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING.md`.
  A run passes if `apply` and `destroy` both succeed with **no provider modification** and all
  four resource categories are created and cleaned up.
- **`sc007/`** — real-mode deep scenario; see
  `../../docs/specs/009-single-node-real-mode/quickstart.md` and its contracts under
  `../../docs/specs/009-single-node-real-mode/contracts/`.

Working state (`.terraform/`, `*.tfstate`, lockfile) is git-ignored — see `.gitignore`.
