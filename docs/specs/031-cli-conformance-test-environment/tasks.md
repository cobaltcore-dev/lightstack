# Tasks: OpenStack CLI and Terraform Conformance

**Input**: spec.md and plan.md in this directory
**Status**: Tasks in scope for PR 107 are complete. Additional Terraform resources remain follow-up work.

## Phase 1: Harness and evidence

- [X] T001 Pin python-openstackclient and its resolved dependency set.
- [X] T002 Add service and workflow manifests, validate their schema and IDs, and gate required
  cases against run results.
- [X] T003 Record JSON and JUnit reports with stable handling for empty fields and multiline errors.
- [X] T004 Centralize known-failure baseline checks and validate the baseline in both gates.
- [X] T005 Make negative cases assert expected error classes and document when CLI lookup is the
  furthest stage a case exercises.
- [X] T006 Make fixture cleanup safe for unrelated resource names, floating IPs, and router
  interfaces; make standalone suites fail when shared fixtures are missing.
- [X] T007 Remove the redundant smoke suite because the gated suites already cover its commands.

## Phase 2: Built artifact and CI

- [X] T008 Build the binary with CGO disabled and mount the checkout's binary, config, and
  migrations into the Alpine binary-test stack.
- [X] T009 Rebuild and recreate the binary-test stack when the local CLI runner starts it.
- [X] T010 Run CLI conformance as a required pull request job against the build artifact.
- [X] T011 Provide read-only permissions to the nightly workflow and share its CLI/Terraform server
  setup through a suite matrix.
- [X] T012 Run CI database regression tests with Redis available and remove the unused CLI install
  from the contract/integration job.

## Phase 3: Terraform and contract runner

- [X] T013 Enforce the Terraform CLI version declared by the wrapper and disable the setup action's
  exit-code wrapper in nightly CI.
- [X] T014 Guard cleanup probes when no state file exists; preserve a workspace only when state
  remains after cleanup fails.
- [X] T015 Remove the empty-state post-destroy plan, which cannot detect remote resource residue.
- [X] T016 Check named OpenStack resources after destroy and keep the wrapper tests aligned with
  Terraform's no-state behavior.
- [X] T017 Run all contract packages through JSON events and keep stderr separate from the stream.

## Phase 4: Follow-up

- [ ] T018 Extend the Terraform compatibility module to additional provider resources as separate
  batches with their own evidence and cleanup checks.
- [ ] T019 Continue closing API inventory gaps through CLI or contract cases as implementation
  support is added.
