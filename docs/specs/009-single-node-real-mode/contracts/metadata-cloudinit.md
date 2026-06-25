# Contract: Metadata Reachability & Cloud-Init (WS-4)

External interface = the OpenStack/EC2 metadata contract at `http://169.254.169.254/` consumed by cloud-init inside the guest, plus the Nova create API accepting `key_name` and `user_data`. Verified from inside the guest and via the OpenStack CLI / Terraform.

## Behavioral contracts

| ID | Given | When | Then |
|----|-------|------|------|
| C-WS4-1 | a booted VM on a tenant network | guest requests `http://169.254.169.254/openstack/latest/meta_data.json` and `/latest/meta-data/` (EC2) | returns **its own** `instance-id`, `hostname`, `public-keys`, and `meta_data.json`. |
| C-WS4-2 | a server created with `--key-name K` | guest boots, cloud-init runs | the injected key is present; `ssh -i K cirros@<ip>` succeeds. |
| C-WS4-3 | a server created with `--user-data file` | guest boots | the `user_data` is delivered (via metadata and/or config drive) and applied (verifiable by its documented effect). |
| C-WS4-4 | two servers in different tenants sharing subnet `10.0.0.0/24` | each requests its metadata | each gets only its own; neither can read the other's (namespace/project-aware identity). |
| C-WS4-5 | a guest that uses config drive (no network metadata) | it boots | it is still provisioned (key + user_data) from the attached config-drive ISO. |
| C-WS4-TF | a Terraform config setting `key_pair` + `user_data` on `openstack_compute_instance_v2` | `terraform apply` | succeeds unmodified; the VM reflects both. |

## Contract tests (RED-first)

- `test/contract/ws4_userdata_test.go` (gophercloud): create a server with `UserData` + `KeyName`; assert the create API accepts/persists them (read back `instance_userdata`).
- `test/ws4_metadata_test.sh` (host/guest): C-WS4-1/2/3 — from inside cirros (or via the netns), `curl` the metadata endpoints; assert keys/user_data; SSH in with the key.
- `test/ws4_isolation_test.sh`: C-WS4-4 — two tenants, overlapping subnet; assert each metadata response is the caller's only.
- `test/ws4_configdrive_test.sh`: C-WS4-5 — disable the network redirect, assert config-drive still provisions.
- `test/terraform/ws4/`: C-WS4-TF — `terraform apply`/`destroy` with key + user_data.

## Notes
- Core surface only (instance-id, hostname, public-keys, user_data, meta_data.json) on OpenStack + EC2 endpoints; `network_data.json`/`vendor_data` deferred.
- Config drive needs `genisoimage` in the image (research R12); metadata redirect is per-namespace DNAT added by the reconciler (research R7), so it survives restart.
