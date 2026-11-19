# Specification: OpenStack CLI and Terraform Conformance

**Status**: Implemented in PR 107
**Issue**: #105 — test the existing OpenStack API operations and Terraform use cases

## Goal

Provide a repeatable gate that drives Lightstack with the pinned OpenStack CLI and the unmodified
Terraform OpenStack provider. The gate must exercise the built binary, record command-level results,
and fail on uncovered or regressed required cases.

## Requirements

- **FR-001 — Pinned CLI:** CI installs python-openstackclient with a complete constraints file so
  the CLI and its transitive dependencies have reproducible versions.
- **FR-002 — Command inventory:** Each supported service has a JSON manifest with unique command
  IDs, endpoint inventory, and required coverage tiers. Invalid or empty manifests fail validation.
- **FR-003 — Recorded outcomes:** The CLI runner records pass, fail, known-failing, skip, and
  untested results in JSON and JUnit formats. The coverage gate rejects missing required cases,
  failures, skips, and known failures absent from the checked-in baseline.
- **FR-004 — Observable negative cases:** Negative cases check the expected refusal class. Cases
  that stop during client-side resource lookup describe that stage and do not claim to validate a
  later server write.
- **FR-005 — Built binary:** The local binary-test stack runs the binary built from the checkout,
  along with that checkout's configuration and migrations. The binary is built with CGO disabled
  for the Alpine runtime image.
- **FR-006 — Pull request gate:** CI runs the pinned CLI against the built artifact and enforces the
  required command coverage without pass-rate thresholds.
- **FR-007 — Nightly coverage:** Nightly CI runs the full CLI suite and Terraform compatibility
  module, and uploads each run's result artifacts.
- **FR-008 — Terraform evidence:** The wrapper checks the declared Terraform CLI version, performs
  init, validate, apply, re-plan, destroy, and verifies that state and named Lightstack resources
  are clean. Failed cleanup retains recovery state only when state exists and cannot be removed.
- **FR-009 — Contract runner:** The contract runner processes JSON output for all packages under
  test/contract, including the root package, without mixing stderr into the JSON stream.

## User scenarios

### Maintainer adds a CLI case

The maintainer adds a unique command ID to a service manifest and implements the matching case. The
coverage gate rejects malformed manifests and reports the case as untested until a full suite run
records its result.

### Pull request changes API behavior

CI builds the binary once and runs the conformance job against that artifact. A missing required
case, unexpected error class, failing assertion, or unbaselined known failure fails the job.

### Nightly checks Terraform compatibility

The nightly job installs the pinned Terraform CLI with its wrapper disabled, runs the compatibility
module against a live Lightstack server, and archives results for both CLI and Terraform suites.

## Out of scope

- Real guest, KVM, and SSH scenarios remain in the existing host-based test program.
- Adding every currently unexercised Terraform provider resource is incremental follow-up work.
- This specification does not claim complete coverage of every OpenStack SDK or API surface.
