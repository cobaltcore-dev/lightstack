# Contracts: <TITLE>

External interface = the exact OpenStack API surface this spec touches, exercised
via the unmodified clients that pin it (python-openstackclient, gophercloud,
Terraform provider, Horizon). All behavior must match upstream with no client
modification. Contract tests are authored **RED-first**, before implementation,
and live under `test/contract/` (CI) and `test/` (host runbooks).

## Behavioral contracts

<!-- One table per API area. IDs are cited by tasks.md and checklists/. -->

| ID | Given | When | Then |
|----|-------|------|------|
| C-1 | ... | ... | ... |
| C-2 | ... | ... | ... |

## Contract tests (to author RED-first)

- `test/contract/<area>_test.go` (gophercloud): C-1, C-2 — what it asserts.
- `test/<area>_real_test.sh` (host, real mode): C-1 — what it asserts.

## Notes

<!-- Upstream quirks, deliberate divergences (and why), fixtures needed. -->
