# Terraform compatibility tests (SPEC-009)

Validates that the **unmodified** Terraform OpenStack provider works against Lightstack
(spec FR-018 / SC-007). Each workstream gets its own self-contained module:

```text
test/terraform/
├── provider.tf.example   # provider block to copy into each wsN/ dir
├── ws3/                  # north-south: floating IP (added in WS-3 phase)
├── ws4/                  # cloud-init: instance + key_pair + user_data (WS-4 phase)
└── ws5/                  # block storage: volume + attach + boot-from-volume (WS-5 phase)
```

## Running

These run against a **live lightstack** (real mode, on the Linux/k3s host — see
`../../docs/specs/009-single-node-real-mode/quickstart.md`). They are not run in
CI.

```bash
source ~/.lightstack-env            # OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_NAME, ...
cd test/terraform/ws4
cp ../provider.tf.example provider.tf
terraform init
terraform apply -auto-approve
# ... assert the created resources ...
terraform destroy -auto-approve
```

A test passes if `apply` and `destroy` succeed with no provider modification and
the resulting OpenStack resources match the workstream's contract
(`../../docs/specs/009-single-node-real-mode/contracts/`).
