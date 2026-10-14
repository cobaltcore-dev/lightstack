## What & why

<!-- What does this PR do and why? Link the issue it closes. -->
Fixes #

<!-- If this closes or narrows an API-compliance gap, say what upstream OpenStack does. -->

## Spec / ADR references

<!-- If this implements or amends a spec in docs/specs/ or an ADR in docs/adr/, cite it. -->
- [ ] Spec/ADR referenced (e.g. `docs/specs/000-api-compliance`) or "none — bugfix/docs only"

## Checklist

- [ ] Tests added/updated — unit tests for logic, contract tests in `test/contract/` (gophercloud-based) for any API surface change
- [ ] `make build` passes
- [ ] Changelog entry added to `CHANGELOG.md` under `[Unreleased]` (user-visible changes only)
- [ ] Docs updated under `docs/` (see `docs/CONTRIBUTING.md`) where behavior or configuration changed
- [ ] Every commit carries `Signed-off-by: Name <name@sap.com>` — DCO; the "SAP Sign-off Check" CI job enforces this. Use `git commit -s`.
- [ ] No secrets, tokens, or credentials in the diff

## Testing evidence

<!-- How you verified this — commands and results. For API changes, include the curl or contract-test run against a running lightstack. -->
```
$ make build
...
$ go test ./test/contract/... -run TestNovaServers
...
```
