# Contract: CI checks and scripts

## CI

| Check | Where | Runs on | Fails when | Message |
|---|---|---|---|---|
| Numbered migrations are immutable (FR-025) | `.github/workflows/ci.yml`, a step in the `build` job | pull requests to `main`, pushes to `main` | `git diff --no-renames --name-status --diff-filter=DM <base> HEAD -- 'migrations/001_baseline.*' 'migrations/[0-9][0-9][0-9]_v[0-9]*'` lists any path | `::error file=<path>::numbered migrations are immutable once on main; put the change in a fragment (migrations/next/)` |
| Folder grammar and hand-numbered files (FR-016, FR-026) | `internal/database` unit test running `ValidateMigrations` on `../../migrations` | the existing `unit-tests` job, both backends | any rule in [migrations-folder.md](migrations-folder.md) fails | the validation error |
| Baseline equals the history (FR-003, FR-004, FR-005) | `TestBaselineEquivalencePostgres`, `TestBaselineEquivalenceSQLite`, `TestBaselineGeneratesInstallTimeIDs`, `TestBaselineProjectIDNotNull` | the existing `unit-tests` job: PostgreSQL service and SQLite | any difference outside `schema_migrations`/`applied_migrations` and the FR-005 exception; a NULL `project_id` that is not reported as a NOT NULL violation | the list of differences |
| Fragment order and conflicts (US3) | `TestFragmentsApplyInOrder`: a per-fragment replay of the real `migrations/next/`, applying each fragment's up SQL alone in name order on top of the numbered migrations | the existing `unit-tests` job | a fragment fails on a fresh database: it runs before what it depends on, or it conflicts with another | the name of the first fragment that fails, with the database's error. `TestMigrationsRoundTrip` and `TestSchemaParity` also run over the whole folder, `next/` included, but golang-migrate's error names only `NNN_open`. |
| No fragments in a release (FR-027) | `.github/workflows/security.yml`, a job guarded by `if: startsWith(github.ref, 'refs/tags/v')` | every `v*` tag push | `migrations/next/` holds any `.sql` file | `::error::<tag> carries unfrozen fragments in migrations/next/: <files>. Run make migrations-freeze RELEASE=<tag> on main, then delete and re-cut the tag.` |

How the immutability step finds its base:
- On a pull request: `git fetch --depth=1 origin "$GITHUB_BASE_REF"`, then diff against `FETCH_HEAD`.
- On a push: `git fetch --depth=1 origin "${{ github.event.before }}"`, then diff against it.
- The second pattern requires a digit after `_v`, as every release name has. The pre-squash `044_volume_groups` and
  `073_volume_transfer_awaiting` do not match it, so the squash change, which moves them, passes. A looser `_v*` would
  match both and fail the squash change itself.
- A new release migration shows up as `A` (added), which is allowed.

The `status-check` job's `needs` list does not change: the immutability step lives inside `build`.

## Makefile

| Target | Change |
|---|---|
| `migrations-freeze` | new: `RELEASE=<release>` runs `lightstack-migrate freeze` |
| `migration-fragment` | new: `TICKET=<n> NAME=<purpose>` creates an empty up/down fragment pair named with the UTC time |
| `test-endpoint-migration` | removed, with `test/endpoint_hostname_migration_test.sh`: it checks migration `069`, which no longer exists |
| `migrate` | unchanged |

## Test scripts

The `build` job's step "Test the migration and deploy scripts" runs `test/migrations_immutable_test.sh` and
`test/deploy_single_node_test.sh`. Neither needs root, a database or a server.
