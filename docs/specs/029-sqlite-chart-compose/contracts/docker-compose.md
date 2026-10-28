# Contract: Docker Compose SQLite variant

**Feature**: `029-sqlite-chart-compose`

Covers the new stub-mode SQLite Compose file and the untouched default. Clauses are verified by inspecting the file and a smoke `docker compose up`.

## C-COMPOSE-1 — New SQLite variant (FR-015)

- `deployments/docker-compose-sqlite.yml` exists.
- It defines **no** `postgres` service and the `lightstack` service has **no** `depends_on: postgres`.
- The `lightstack` service sets `LIGHTSTACK_DB_URL: "sqlite:///var/lib/lightstack/db/lightstack.db"` (absolute path; three slashes after the scheme).
- A named volume (e.g. `lightstack_db`) is mounted at the database directory (`/var/lib/lightstack/db`) so the database file persists across `up`/`down` without `-v`.
- Redis is kept as in the default file (optional cache); its presence or absence does not affect the database.
- `LIGHTSTACK_DB_BACKEND` is left unset (SPEC-028: unset means no scheme check; the URL alone selects SQLite).

## C-COMPOSE-2 — Default file untouched (FR-015, FR-017)

- `deployments/docker-compose.yml` is unchanged: it still defines the `postgres` service and a `postgres://` `LIGHTSTACK_DB_URL`.

## C-COMPOSE-3 — Smoke behaviour

- `docker compose -f deployments/docker-compose-sqlite.yml up` brings up `lightstack` with no postgres container; the binary creates `/var/lib/lightstack/db/lightstack.db` on the named volume, migrates, and `openstack token issue` succeeds.
- `down` then `up` on the same named volume finds the database and the resources created before.

## Out of scope

- `docker-compose-horizon.yml`, `docker-compose.rgw-test.yml`, `docker/docker-compose.yaml`, `horizon/docker-compose.yaml` stay PostgreSQL test stacks (unchanged).
- The real-mode `docker-compose-kvm.yml` already runs either backend (SPEC-028 FR-042) and is not changed here.
