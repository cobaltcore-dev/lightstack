# SBOM and Vulnerability Triage

## Purpose

This document defines how Lightstack inventories dependencies, evaluates vulnerability reports, and accepts a release artifact. It also records the remediation of the Black Duck SPDX 3.0.1 export generated on 2026-09-30.

## Scope model

Dependency results must be assigned to one of these scopes before they are treated as release risk:

| Scope | Source of truth | Included in release SBOM | Security treatment |
| --- | --- | --- | --- |
| Runtime image | Final `build/package/Dockerfile` stage, including Alpine packages and both Go binaries | Yes | Release-blocking |
| Production Go | Modules embedded in `/app/lightstack` or `/app/lightstack-migrate` | Yes | Reachability scan and image scan |
| Build-only | Builder image packages and compiler toolchain | No | Patch and pin; scan separately when builder images are published |
| Development-only | `tools/go.mod`, test dependencies, linters, IDEs, and local utilities | No | Maintain and license-review, but do not report as shipped code |
| Source repository | Documentation, examples, generated files, and all modules visible to a source scanner | No | Useful discovery input; never substitute for the runtime inventory |

The release SBOM is generated from the final container image. CI compares its Go packages with `go version -m` output from both compiled binaries and fails if an embedded module is absent. The image ID and OCI source, revision, and version labels bind the evidence to a build.

## 2026-09-30 scan assessment

The supplied export contains 271 package records, 20 vulnerability records, and 29 vulnerability associations. Every VEX association is `underInvestigation`. It combines production, test, and development dependencies and therefore is not a faithful inventory of the release image.

Observed quality issues:

- 266 Go packages are described as dynamically linked even though the release binaries use `CGO_ENABLED=0`.
- The compiled binary contains roughly 57 dependency modules, far fewer than the 271 source-scan package records.
- Alpine runtime packages, Git revision, and image identity are absent.
- Known production modules such as `github.com/ceph/go-ceph` and `github.com/digitalocean/go-libvirt` are absent.
- `Jetbrains ideavim`, `elsa-workflows/elsa-core`, `Kubernetes`, `github/spec-kit`, and `lighttiger2505/wakabatasks` are not in the production Go module graph. They are likely repository-content matches or component misidentifications and require evidence before attribution.
- `software_downloadLocation` is `NOASSERTION` throughout the export. This means provenance is unknown; it is not evidence that a component ships or that its license is acceptable.

The generated runtime SPDX artifact replaces this export as the release inventory. The source export remains useful as a discovery and license-review input.

## Remediation decisions

| Finding | Assessment | Resolution |
| --- | --- | --- |
| GO-2026-6238 / CVE-2026-10722 in `cilium/ebpf` | Reachable | Upgrade 0.21.0 to 0.22.0 |
| GO-2026-5970 / CVE-2026-56852 in `x/text` | Reachable | Upgrade transitively from 0.38.0 to 0.41.0 |
| GO-2026-5676 / CVE-2026-40898 in `quic-go` | Reachable | Upgrade 0.59.0 to 0.59.1 |
| `x/crypto/ssh` advisories | Imported but not reachable in the assessed call graph | Upgrade `x/crypto` from 0.53.0 to 0.56.0 and continue reachability scanning |
| `x/net` DNS parser advisory | Module/package finding, not reachable | Upgrade transitively from 0.55.0 to 0.57.0 |
| GO-2026-5932 in `x/crypto/openpgp` | Affected module present; vulnerable packages absent | `not_affected` with `vulnerable_code_not_present`; see `openvex.json` |
| Go 1.26.2 standard-library advisories | Toolchain affects every binary | Pin Go 1.26.8 in modules, CI, builder image, and test container |
| GPL and other linter-only dependencies in the root module | Development-only, not shipped | Move the pinned linter graph to `tools/go.mod` |
| Vulnerable transitive modules in the pinned linter graph | Development-only but executed in CI | Upgrade the affected modules and scan the linter command with `govulncheck` |
| GO-2026-6603, -6605, -6607, -6608, -6610, -6611, -6612, -6613, -6617 in the Go 1.26.8 standard library, published 2026-10-08 | Reachable; toolchain affects every binary | Upgrade the toolchain in modules, CI, builder image, test container, and the single-node deploy script; current pin is Go 1.27.3 |
| GO-2026-6809 in `crypto/x509`, detected by CI on 2026-10-30 | Reachable through the HTTP server; Go 1.27.2 trusts compromised intermediate certificates | Pin Go 1.27.3 throughout the build and test toolchain; keep vulnerability scanning enabled |
| GO-2026-6603, -6610, -6611, -6612, -6617 in `x/net`, published 2026-10-08 | Reachable through gin's HTTP/2 support | Upgrade transitively from 0.57.0 to 0.60.0; `x/crypto`, `x/sys`, `x/sync`, and `x/text` move to its minimums |
| GO-2026-6809 (`crypto/x509` intermediate-certificate distrust) in the Go 1.27.2 standard library, published after the 1.27.2 pin; reachable via `http.Server.Serve` → `x509.Certificate.Verify` | Reachable; toolchain affects every binary | Raise the go.mod floor to 1.27.3; CI (setup-go `go-version: stable`) and the builder and test images (`golang:1`) track the latest stable release, so later standard-library patches apply without a code change. The checksum-pinned single-node deploy script is the deliberate exception. |

## Triage procedure

For each new report:

1. Normalize identity using package URL, ecosystem, module path, version, and image digest. Reject name-only CPE matches until independently confirmed.
2. Establish scope using the final image SBOM, `go version -m`, the root module graph, the tools module graph, and Alpine's installed package database.
3. Check whether the affected package is imported and whether a vulnerable symbol is reachable with `govulncheck`. Reachability lowers uncertainty but does not overrule an affected runtime OS package or a credible non-Go exploit path.
4. Upgrade to a fixed version when available. If no upgrade is possible, record an owner, compensating control, target date, and explicit risk acceptance.
5. Write VEX only when the disposition is supported by reproducible evidence. Never suppress an unresolved match merely to pass CI.
6. Rebuild the final image and regenerate the SBOM after remediation; do not edit generated SBOM findings manually.

For `NOASSERTION` or unidentified records, locate the upstream repository and authoritative license, confirm the exact version and checksum, and add a valid purl. If identity or licensing remains unresolved, exclude the component only with proof that it is absent from the release artifact; otherwise escalate it for legal/security review.

## Release acceptance criteria

A release candidate is acceptable only when all of the following hold:

- Linux-target `govulncheck` reports no reachable vulnerability in `cmd`, `internal`, or `pkg`.
- The final image has no fixable High or Critical finding from the configured image scanner.
- An SPDX JSON SBOM is generated from the final image and retained with the image ID.
- Every Go dependency reported by `go version -m` for both production binaries is represented in the SBOM.
- OCI source and revision labels identify the repository and commit.
- The root and tools module files are tidy and reproducible with the pinned Go toolchain.
- Each `not_affected` or `fixed` exception has current evidence and an owner-review path.

## VEX lifecycle

VEX statements are reviewed whenever the affected module, production import graph, builder, or runtime base image changes, and at least once per release. A statement expires operationally after 90 days without review. Remove or replace it immediately if the vulnerable package becomes imported, binary/SBOM evidence changes, or the advisory's affected range or exploit conditions are revised.

GO-2026-5932 is currently supported by two checks: source-level `govulncheck` reports only a module-level finding, and `golang.org/x/crypto/openpgp` is absent from the import graph and compiled-binary inventory.
