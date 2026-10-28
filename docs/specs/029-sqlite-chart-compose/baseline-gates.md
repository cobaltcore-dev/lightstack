# Baseline gates (T001)

Recorded on `feature/599` before the SQLite chart/compose changes, as the oracle for FR-017 (PostgreSQL renders unchanged).

## Gates (green on the pre-change tree)

- `helm unittest charts/lightstack` → **Charts 1/1, Test Suites 21/21, Tests 122/122 passed**.
- `go test ./internal/teardown/...` → **ok** (`github.com/cobaltcore-dev/lightstack/internal/teardown`).

## PostgreSQL render reference

Captured with `helm template r charts/lightstack --set image.repository=ghcr.io/lightstack/lightstack --set secrets.jwtSecret=jwt --set secrets.adminPassword=pw --set database.url=postgres://lightstack@db/lightstack [...]`:

| Values | Lines | Key facts (must be preserved, aside from the added `LIGHTSTACK_DB_BACKEND=postgres`) |
|---|---|---|
| stub (default) | 207 | **no** `strategy` block (default RollingUpdate); **no** `LIGHTSTACK_DB_BACKEND` before the change |
| real (`realMode.enabled=true`) | 420 | `strategy: { type: Recreate }` (lines 279-280) |
| stub + `dataRetentionPolicy=delete` | 374 | teardown quiesce + purge Jobs rendered; purge Job carries no node pin yet |

Reference renders saved under `/tmp/ls-baseline/{pg-stub,pg-real,pg-stub-delete}.yaml` for the T013 parity diff. After the change, a PostgreSQL render must differ only by the added `LIGHTSTACK_DB_BACKEND=postgres` env and the scheduling emitted through the new shared helper (byte-identical nodeSelector/affinity/tolerations).

## Environment

- helm v3.22.0, helm-unittest 1.2.1, go 1.27.3.

## T013 parity result (post-change)

`diff` of the PostgreSQL stub render (`--set database.url=postgres://lightstack@db/lightstack`) before vs after the change:

```
146a147,148
>             - name: LIGHTSTACK_DB_BACKEND
>               value: "postgres"
```

The **only** difference is the added `LIGHTSTACK_DB_BACKEND=postgres` env (FR-003). The scheduling helper (`lightstack.scheduling`) renders byte-identically for default and real-mode PostgreSQL values (both diffs clean). FR-017 holds.
