# Feature Specification: Squash the migrations, restart at 001, one migration per release

**Feature Branch**: `032-migration-squash-per-release`
**Created**: 2026-11-17
**Status**: Draft
**Input**: GitHub issue [cobaltcore-dev/cloud-storage#596](https://github.com/cobaltcore-dev/cloud-storage/issues/596) — "[FEAT] - lightstack - Database - squash the migrations, restart at 001, one migration per release"

## Context

Lightstack's schema is the sum of 83 migration versions, `001` to `083`, in 184 files in one
`migrations/` folder. Both backends read the folder, each through its own view: PostgreSQL replays
all 83 versions; SQLite starts at a generated baseline at `073` and replays 10. Every feature that
changes the schema takes the next free number. The last release, v0.6.1, shipped `001`–`059`;
everything above is unreleased.

golang-migrate applies the migrations. It records two things per database: the version reached and
a dirty flag. It applies only versions above the recorded one, and it refuses a database whose
recorded version is not in the folder.

Three problems follow.

- **Per-feature numbers collide across parallel branches.** A migration merged below `main`'s
  newest runs on a fresh database, but every database already past that number skips it. Fresh and
  upgraded databases diverge without an error. This has happened: `069` reached `main` on
  2026-09-10, and `063`–`068` followed between 2026-09-21 and 2026-10-09. Every database already
  at `069` or above skipped them.
- **The history is noise.** Every fresh install, test database and CI run replays every migration
  ever written, including data fixes that mattered once. Five migrations exist only to rewrite
  endpoint URLs (`052`, `056`, `057`, `064`, `069`).
- **Releases are what gets deployed.** A database's recorded version names a feature, not a
  release.

This feature folds the history into one baseline per backend at `001`. A database from before the
squash is refused and recreated: v0.7.0 is a fresh install. After that, each release ships one
migration. During a cycle it is assembled
from per-change fragments, and a fragment has no number to pick.

## Clarifications

### Session 2026-11-17

- Q: Must a deployed database survive the upgrade, such as a v0.6.x deployment? → A: No. Nothing
  deployed today has to survive. The squash point is the newest migration on `main` when the
  squash lands. A database exactly at that point can be re-stamped; every other database is
  recreated (FR-001, FR-011, FR-013). Superseded in part on 2026-11-19: there is no re-stamp, so
  every database from before the squash is recreated (Session 2026-11-19).
- Q: Should fragments live in their own folder, or should each release use one shared file? → A:
  Fragments (FR-014).
- Q (follow-up): Fragments have no number, so how is their order guaranteed? → A: By name. Each
  name starts with the fragment's creation time in UTC, not with the ticket number. A fragment can
  only depend on what was in its author's tree when it was written, and that is always older, so
  creation order is dependency order. Ticket order is not: a later fragment can carry a lower
  ticket number. The one way to break the order is to edit an old fragment so it depends on a newer
  one. That fails CI's fresh-database migration on both backends, which names the fragment. Stale
  databases are recreated, not caught up, so every database applies the fragments in this one
  order, and the release step freezes it (FR-014, FR-015).
- Q: Is it acceptable to recreate dev, CI and staging databases once at the squash, and again
  whenever the open release migration changes? → A: Yes, both. Anything that must keep its data
  runs released builds only (FR-020).
- Q: Which version should the baseline carry? `001` reuses numbers that pre-squash databases
  recorded. Keeping the squash point's own number would avoid that, and would also remove the need
  for re-stamp. → A: `001`, as the ticket says. Re-stamp (FR-009) and the pre-squash marker
  (FR-012) stay. The accepted cost: a pre-squash Lightstack pointed at a post-squash PostgreSQL
  database replays `002` onward over the baseline and leaves the database dirty. `docs/UPGRADING.md`
  warns about it (FR-013, Edge Cases). Superseded in part on 2026-11-19: re-stamp is gone; the
  pre-squash marker stays (Session 2026-11-19).
- Q: How strictly should re-stamp check a database at the squash point? → A: Exact schema. Any
  missing, different or extra table, column, type, default, key, constraint or index refuses the
  database, and every difference is listed. Data is never compared. No persisted database should
  exist yet, so a refusal costs at most one recreate (FR-009, FR-010). Superseded on 2026-11-19:
  there is no re-stamp (Session 2026-11-19).
- Q: What does CI compare a numbered migration against to prove it frozen? → A: Nothing outside
  `main`. A numbered migration is immutable once it is there. CI fails any change that modifies,
  renames or deletes one, except the squash change itself. This covers the baseline from the squash
  on and each release migration from its freeze, and needs no release tags (FR-025). The spec calls
  such a file "frozen". The open release migration takes the version one above the newest frozen
  migration (FR-015).
- Q: What stops a `v*` tag from being cut on a tree that still holds fragments? → A: A check on
  every `v*` tag push, in the workflow that already runs on tags. It fails when the tagged tree
  holds fragments, and the release step leaves none behind. A runtime refusal in release binaries
  was not chosen (FR-027).
- Q: Should the squash land before or after v0.7.0 is tagged? → A: Before. v0.7.0 is the first
  post-squash release, so no v0.7.0 database ever needs re-stamp. 0.6.x databases are recreated,
  and the 0.7.0 upgrade notes say so (FR-013, Dependencies).
- Q: The upgrade script rolls back by restoring an older binary, not the database. Should it guard
  a rollback across the squash? → A: Yes. The script refuses a `--version` rollback to a
  pre-squash build once the database is post-squash, and names the reason (FR-013). Superseded on
  2026-11-18: the script is deleted (Session 2026-11-18).
- Q: Which tags freeze a release migration, and may an older release line get a schema change? →
  A: Every `v*` tag, pre-releases included, is cut from `main` and freezes the open fragments. A
  release candidate's migration is therefore final. An older line never gets a schema change: a fix
  that needs one ships in the next release from `main` (FR-023, Assumptions).
- Q (planning): SQLite's `079` and `080` enforce NOT NULL on `project_id` of volumes, ports,
  networks, routers and floating_ips with 10 triggers, because SQLite cannot add NOT NULL to a
  referenced table without a rebuild. A baseline generated from the PostgreSQL catalog declares the
  columns NOT NULL instead. Which form should the SQLite baseline use? → A: Real NOT NULL, as on
  PostgreSQL, and the triggers go. A pre-squash SQLite database can therefore never match the new
  baseline, so re-stamp is PostgreSQL-only. SQLite was never released, and its databases are
  recreated (FR-005, FR-009, FR-010). Superseded in part on 2026-11-19: re-stamp is gone on both
  backends (Session 2026-11-19).

### Session 2026-11-18

- Q: `scripts/upgrade-lightstack.sh` serves only bare-metal installs. Keep it, with the two guards
  FR-013 gave it? → A: No, delete it.
  - It upgrades only the git checkout at `/opt/lightstack` that `lightstack.service` runs, the
    layout `scripts/deploy-single-node.sh` creates. Docker Compose and Helm never use it.
  - It could not finish an upgrade. Its migration step called a `migrate` subcommand the server has
    never had, which started the server in the foreground.
  - Its checkout stashed the node's configuration, which the deploy script writes over a tracked
    file, and nothing put it back. Its backup held no database, so its rollback was unsafe across
    any migration.
  - The binary itself refuses a pre-squash database (FR-011), and `docs/UPGRADING.md` gives the
    bare-metal steps by hand (FR-013).

### Session 2026-11-19

- Q: v0.6.1, the last release, shipped migrations up to `059`, so no release database can ever be
  at the squash point. Only a database built from `main` could, and recreating those was already
  accepted. Keep re-stamp? → A: No, delete it. v0.7.0 is a fresh install: nothing is upgraded in
  place, and every database from before the squash is recreated.
  - The pre-squash refusal stays (FR-011, FR-012). Its one way out, on both backends, is to
    recreate the database.
  - The squash point leaves the code and its messages. `status` names the baseline `baseline`.
  - The schema introspection that served re-stamp moves back into test code.
  - `docs/UPGRADING.md` keeps one section per release, newest first. The v0.7.0 section says to
    install fresh (FR-013). It does not mention the product's earlier name.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — A new database starts from one baseline that equals the history (Priority: P1)

A developer, a CI job or an operator installing Lightstack creates an empty database. Lightstack
applies one baseline migration instead of replaying the history. The result has the same schema and
the same seed data the history built.

**Why this priority**: The squash is the feature's core, and everything else depends on it. A
baseline that differs from the history would silently change every new install, so the equality has
to be proven.

**Independent Test**: On each backend, migrate one empty database with the pre-squash history and
one with the baseline. Compare schema and seed rows. The only allowed difference is values
generated at install time.

**Acceptance Scenarios**:

1. **Given** an empty PostgreSQL database, **When** Lightstack starts, **Then** it applies one
   migration and records version `001`. The schema and seed rows equal those of a database
   migrated with the pre-squash history.
2. **Given** an empty SQLite database, **When** Lightstack starts, **Then** the same holds.
3. **Given** seed data whose ids the history generates at install time, **When** two databases are
   created from the baseline, **Then** each gets its own generated ids, as with the history. The
   baseline never freezes one run's values.
4. **Given** the baseline differs from the history in any table, column, type, default, key,
   constraint, index or seed row, **When** CI runs the comparison, **Then** CI fails and names the
   difference.
5. **Given** a database at the baseline, **When** it is migrated all the way down and up again,
   **Then** its schema equals the schema after the first up.

---

### User Story 2 — A database from before the squash is refused with the way out (Priority: P1)

An operator or developer starts a post-squash Lightstack on a database migrated by a pre-squash
Lightstack. Nothing carries such a database over: v0.7.0 is a fresh install. Lightstack refuses
the database on both backends, without touching it, with a message that says why and what to do:
recreate the database.

**Why this priority**: Without this, a post-squash build answers every existing database with
golang-migrate's `no migration found for version N`. Worse, Lightstack's own guard says the
database "was written by a newer Lightstack", which is false. Once a few releases exist, an old
database at a low version would even pass as current and be migrated on top of the wrong schema.
And `lightstack-migrate force 1` would stamp any database, including one whose schema differs.

**Independent Test**: Prepare a pre-squash database on each backend, and an empty one. Start the
post-squash build and run `lightstack-migrate` against each. Check the exit codes and messages, and
that the pre-squash databases are unchanged.

**Acceptance Scenarios**:

1. **Given** a pre-squash database on either backend, **When** a post-squash Lightstack starts on
   it, or `lightstack-migrate up`, `down`, `goto` or `force` runs, **Then** it refuses and changes
   nothing. The message says the database predates the squash, names its version and says to
   recreate the database. It never says a newer Lightstack wrote the database.
2. **Given** a pre-squash database, **When** `lightstack-migrate status` runs, **Then** it reports
   `PRE-SQUASH` with the same message and exits non-zero.
3. **Given** a pre-squash database, **When** `lightstack-migrate reset` or `backup` runs, **Then**
   it works: reset is one way to recreate the database, and a backup keeps its data.
4. **Given** the database was recreated, **When** Lightstack starts, **Then** it migrates it from
   the baseline and serves.

---

### User Story 3 — Parallel changes add schema without picking numbers (Priority: P2)

Two developers on parallel branches both need a schema change for the next release. Each adds a
fragment whose name starts with its creation time, followed by the ticket and purpose. Neither
picks a number. Both branches merge, in either order. Every database, fresh or upgraded, gets both
changes.

**Why this priority**: This removes the collision for good. The squash alone resets the history but
keeps per-feature numbering, and with it the collisions.

**Independent Test**: Two branches each add a fragment. Merge them in both orders. A fresh database
gets both changes. A database at the newest frozen version gets both on its next start.

**Acceptance Scenarios**:

1. **Given** two branches that each add a fragment, **When** both merge in either order,
   **Then** the migrations folder has no conflict. A fresh database applies both fragments as one
   migration, the open release migration, in fragment-name order. Name order is creation order,
   so the result does not depend on merge order.
2. **Given** a fragment that needs backend-specific SQL, **When** it ships as a PostgreSQL/SQLite
   pair, **Then** each backend applies its own half. Both backends apply the cycle's plain
   fragments.
3. **Given** a fragment with an up part and no down part, or the reverse, **When** the migrations
   are validated, **Then** validation refuses the folder and names the fragment.
4. **Given** a fragment created after another fragment of the same cycle and depending on it,
   **When** a fresh database is migrated, **Then** it applies after the fragment it depends on,
   because its name sorts later.
5. **Given** an old fragment edited to depend on a fragment created after it, **When** CI migrates
   a fresh database, **Then** the migration fails and names the fragment. The failure shows up in
   CI, not later.
6. **Given** a database at the newest frozen version and fragments present, **When** Lightstack
   starts, **Then** it applies the open release migration and logs that the migration is
   unreleased.

---

### User Story 4 — A database that applied an earlier state of the open release migration is refused (Priority: P2)

A developer's database applied the open release migration, and another fragment merged after that.
golang-migrate considers the database current and will never apply the new fragment. Lightstack
detects that the open release migration has changed since this database applied it. It refuses to
start and says why: the database must be recreated.

**Why this priority**: Without this guard, the fragment design brings back the silent divergence it
exists to remove, one level down.

**Independent Test**: Apply the open release migration, add a fragment and restart: Lightstack
refuses and names the reason. Then freeze the release with the fragments unchanged: the database
carries on.

**Acceptance Scenarios**:

1. **Given** a database that applied the open release migration, **When** a fragment has since been
   added, changed or removed and Lightstack starts, **Then** it refuses to start. The message names
   the migration, says the database applied an earlier state of it, and says to recreate the
   database.
2. **Given** the same database, **When** `lightstack-migrate up` or `status` runs, **Then** it
   reports the same cause.
3. **Given** the open release migration has not changed since the database applied it, **When**
   Lightstack starts, **Then** it starts normally.
4. **Given** a database that applied exactly the state that was then frozen into the release,
   **When** a build with the frozen release migration starts, **Then** the database carries on
   without being recreated.
5. **Given** a database that applied an earlier state, and the release was later frozen with more
   fragments, **When** a build with that release starts, **Then** it is refused as in scenario 1.

---

### User Story 5 — A release freezes its migration, and CI keeps it frozen (Priority: P2)

At release time, a maintainer runs one release step. It writes the open release migration to
numbered files named for the release, `NNN_v<release>`, with down files, and it empties the
fragments. Once those files are on `main`, CI fails any change to them.

**Why this priority**: One migration per release holds only if a frozen file never changes, and
golang-migrate cannot tell when one did.

**Independent Test**: Run the release step with fragments present. Check for one numbered file per
variant, no fragments left, and the same schema on a fresh database before and after the freeze.
Change a frozen file: CI fails. Run the release step with no fragments: no file is added.

**Acceptance Scenarios**:

1. **Given** fragments for release vX.Y.Z, **When** the maintainer runs the release step,
   **Then** the open release migration becomes `NNN_vX.Y.Z` (one above the newest frozen
   version) and the fragments are removed. A fresh database migrated from the frozen files equals
   one migrated from the fragments.
2. **Given** no fragments, **When** the release step runs, **Then** it adds no migration and says
   so.
3. **Given** a numbered migration on `main`, whether the baseline or a frozen release migration,
   **When** a change edits, renames or deletes it and CI runs, **Then** CI fails and names the
   file.
4. **Given** a numbered migration that is neither the baseline nor a release migration, such as a
   hand-numbered per-feature file, **When** CI runs, **Then** CI fails and says schema changes go
   into fragments.
5. **Given** a `v*` tag pushed on a tree that still holds fragments, **When** the tag-push check
   runs, **Then** it fails and names the fragments. The tag is deleted and re-cut after the freeze.
   Production runs released migrations only.

---

### Edge Cases

- **A pre-squash database above every post-squash version**: the usual cases are a v0.6.x database
  at `059` meeting v0.7.0, and a `main`-tracking one at `083`. It is refused as pre-squash
  (FR-011), not as "written by a newer Lightstack".
- **A pre-squash database at a version the new numbering also uses**: it is still recognised as
  pre-squash (FR-012). The O3K-era v1.0.0 shipped migrations up to `002`, and over the years the
  per-release numbering will climb through `059` and `083`.
- **A pre-squash Lightstack pointed at a post-squash database**: this happens on a rollback across
  the squash, or when an old branch runs its tests against a shared test database.
  - On PostgreSQL, the old Lightstack reads the database as its own `001` and replays `002` onward
    over the baseline. `002` inserts the default security group's two egress rules a second time,
    and `003` then fails on an index that already exists. The database is left dirty at `003`.
  - On SQLite it refuses, because its view starts at `073`.

  The new code cannot stop an old binary. `docs/UPGRADING.md` warns about it (FR-013). The way
  back is to recreate the database.
- **A NULL `project_id` written to volumes, ports, networks, routers or floating_ips on SQLite**:
  under the baseline it fails as a NOT NULL violation, as on PostgreSQL. Under the old triggers it
  failed with the trigger's own message.
- **A SQLite backup taken by a pre-squash Lightstack, restored under a post-squash one**: startup and
  the integrity check refuse it like any other pre-squash database, with the same way out.
- **A release with no schema change**: it adds no release migration. The next release that has
  fragments takes the next number.
- **A pre-release tag with fragments** (v0.7.0-rc.1): it freezes `NNN_v0.7.0-rc.1`. The final
  v0.7.0 freezes only what merged after the release candidate, or nothing.
- **Two fragments with the same name for one backend**: validation refuses the folder.
- **Two fragments with the same creation time**: the rest of the name (ticket, then purpose) breaks
  the tie, so the order stays the same on every database.
- **An old fragment edited to depend on a fragment created after it**: it still sorts first, so
  migrating a fresh database fails in CI and names it. The fix is to give it a new creation time.
- **Two fragments that conflict, such as both adding the same column**: each branch passes on its
  own, and `main`'s CI fails as soon as both are in, naming the second. No order resolves a
  semantic conflict. The second change has to be rewritten.
- **A developer switches a database at the open release migration to a branch that lacks some of
  its fragments**: the database is refused as stale (FR-020). If the branch holds no fragments at
  all, it is refused as written by a newer Lightstack.
- **Rolling back the open release migration**: it rolls back every fragment of the cycle at once.
  A single fragment cannot be rolled back alone.
- **The open release migration fails part-way**: the database is marked dirty, as today. The
  migration is unreleased, so the way out is to recreate the database.
- **A branch that still carries a hand-numbered migration after the squash**: CI refuses it
  (FR-026). The branch moves the change into a fragment.
- **The old release tags** (v0.4.1 to v0.6.1, and the O3K-era v1.0.0): the frozen check reads no
  tags, so they play no part. A future Lightstack 1.0.0 still collides with the O3K-era tag. That
  belongs to the release process (Out of Scope).

## Requirements *(mandatory)*

### Functional Requirements

#### The baseline

- **FR-001**: The migrations folder MUST hold one baseline per backend at version `001`. It equals
  the schema and seed data at the squash point, which is the newest migration on `main` when the
  squash lands. Nothing deployed today has to survive the upgrade.
- **FR-002**: The baselines MUST be generated from a database migrated with the pre-squash
  history, never written by hand. The same history MUST always generate the same files.
- **FR-003**: Values the history generates at install time, such as seed ids and seed timestamps,
  MUST also be generated at install time by the baseline. No baseline freezes one run's values.
- **FR-004**: An automated comparison MUST prove, on both backends, that a database migrated from
  the baseline equals one migrated with the pre-squash history. It compares tables, columns,
  types, defaults, keys, constraints, indexes and seed rows, with install-time values compared as
  generated. CI MUST run the comparison for the change that introduces the baselines, and MUST fail
  on any difference, naming it.
- **FR-005**: The squash MUST NOT change any schema element or seed row. Clean-ups of either are
  separate changes. There is one exception, on SQLite only. The 10 triggers that `079` and `080`
  use to keep `project_id` non-null on volumes, ports, networks, routers and floating_ips become
  real NOT NULL constraints, as PostgreSQL has them. The equivalence check (FR-004) treats the two
  forms as equal. A violation then reports as a NOT NULL violation on both backends.
- **FR-006**: Both backends MUST start at `001`. The rule that SQLite skips every version below its
  own baseline is retired, because nothing is left below it.
- **FR-007**: The pre-squash migration files MUST leave the folder. Tests and tooling that exist
  only to exercise one pre-squash migration MUST leave with them; the pre-squash release keeps them.
- **FR-008**: A database at the baseline MUST survive a round trip: migrated down and up again, its
  schema equals the schema after the first up.

#### Existing databases

- **FR-009**: *Removed on 2026-11-19 (Clarifications, Session 2026-11-19): there is no re-stamp; a
  database from before the squash is recreated (FR-011).*
- **FR-010**: *Removed on 2026-11-19 (Clarifications, Session 2026-11-19): there is no re-stamp; a
  database from before the squash is recreated (FR-011).*
- **FR-011**: Every entry point that migrates or inspects a database MUST recognise a pre-squash
  database: server start, the `lightstack-migrate` commands `up`, `down`, `goto`, `force` and
  `status`, and the SQLite integrity check `check`. Each MUST refuse to act on it, or, for
  `status`, report it and exit non-zero. The message MUST name the database as pre-squash, give its
  version and give the way out, the same on both backends: recreate the database. The message MUST
  NOT be golang-migrate's raw `no migration found` error or "written by a newer Lightstack".
  `reset`, which recreates the database, and `backup` stay available.
- **FR-012**: Lightstack MUST tell a pre-squash database from a post-squash one without relying on
  the version number alone, because the restarted numbering reuses numbers that pre-squash
  databases recorded.
- **FR-013**: `docs/UPGRADING.md` MUST keep one section per release, newest first, each saying
  what an operator does to upgrade to that release from the one before. v0.7.0, the first
  post-squash release, upgrades nothing in place. Its section MUST say to install it fresh on an
  empty database and show how to empty the database for each deployment. It MUST NOT mention the
  product's earlier name.

  A skipped step costs a restart, not data, because the new binary refuses a pre-squash database
  without touching it (FR-011). There is no upgrade script: `scripts/upgrade-lightstack.sh` is
  deleted (Clarifications, Session 2026-11-18), and each release's section gives its steps by hand.
  The document MUST warn that a pre-squash Lightstack must never run against a post-squash
  database. On PostgreSQL it replays the old history over the baseline and leaves the database
  dirty. A rollback across the squash therefore means recreating the database.

#### Fragments and the open release migration

- **FR-014**: A schema change for the next release MUST be added as a fragment in a fragments folder
  inside `migrations/`. A fragment carries no version number. Its name starts with its creation
  time in UTC, to the second, followed by its ticket and purpose. A fragment is either one plain
  file for SQL both backends treat alike, or a PostgreSQL/SQLite pair, each with its down part.
- **FR-015**: Lightstack MUST assemble the fragments into one open release migration, one version
  above the newest frozen migration (before v0.7.0 that is the baseline, so the first open release
  migration is `002`). Each backend gets the fragments in name order, which is
  creation order: the plain fragments, plus its own backend fragments. The down part runs the
  fragments' down parts in reverse order. The order MUST be the same on every database and in the
  frozen release migration.
- **FR-016**: Migration validation MUST also cover fragments. It refuses an up part without its
  down part or the reverse, two fragments of one name for one backend, and a fragment name the
  naming convention rejects.
- **FR-017**: Every entry point that reads the migrations MUST see the same assembled set: server
  start, every `lightstack-migrate` command, the integrity check and the tests' database helper.
- **FR-018**: When Lightstack applies the open release migration, it MUST log that the migration is
  unreleased.

#### The stale-database guard

- **FR-019**: Lightstack MUST record, for each database, which state of the open release migration
  that database applied. golang-migrate records only the version and the dirty flag.
- **FR-020**: Lightstack MUST refuse to start, and `lightstack-migrate` MUST refuse to migrate, a
  database whose applied state of its current version differs from the migration of that version
  Lightstack now carries. That happens when a fragment was added, changed or removed since, or when
  the migration was since frozen with different content. Both MUST name the migration and say to
  recreate the database. Recreating dev, CI and staging databases is acceptable. Any environment
  that must keep its data runs released builds only.
- **FR-021**: A database that applied exactly the state that was frozen MUST carry on without being
  recreated.
- **FR-022**: `lightstack-migrate status` MUST report whether a database's version is a frozen
  migration or the open release migration. It MUST report a stale open state as such.

#### Release freeze

- **FR-023**: A single release step for maintainers MUST freeze the open release migration into
  numbered files named `NNN_v<release>`, one for each variant present, with down files, and remove
  the fragments. It runs for every `v*` tag, pre-releases included, before the tag is cut from
  `main`. A fresh database migrated from the frozen files MUST equal one migrated from the
  fragments, and the guard (FR-020) MUST see the same state.
- **FR-024**: The release step MUST add no migration when there are no fragments. It MUST refuse a
  release that already has a migration.
- **FR-025**: A numbered migration MUST be immutable once it is on `main`. This covers the baseline
  from the squash on and each release migration from its freeze. CI MUST fail any change that
  modifies, renames or deletes one, and name the file. Only the baseline's and the release
  migrations' names are protected, so the squash change, which removes the pre-squash files, passes.
  Release tags play no part in the check.
- **FR-026**: CI MUST fail when the folder holds a numbered migration that is neither the baseline
  nor a release migration, and say that schema changes go into fragments.
- **FR-027**: A release MUST NOT ship an open release migration. A check MUST run on every `v*` tag
  push, in the workflow that already runs on tags. It fails when the tagged tree holds fragments
  and names them. The release step itself MUST leave no fragments behind.

#### Documentation and gates

- **FR-028**: `migrations/README.md` and `CLAUDE.md` MUST describe the new rules: the baseline at
  `001`, fragments, one migration per release, the frozen rule and the guard. Living documents and
  code comments that send a reader to a pre-squash migration file as the place something is defined
  MUST point to the baseline or state the schema fact instead. Historical records stay as written:
  the changelog, archived specs and reports, past `UPGRADING.md` sections, and other features'
  specs.
- **FR-029**: The unit tests on both backends, both contract suites and `test/quick_test.sh` MUST
  pass.

### Key Entities

- **Squash point**: the newest pre-squash migration on `main` when the squash lands. The baseline
  reproduces its schema and seed data.
- **Baseline**: the migration at version `001`, one per backend, generated from the squash point.
  It is frozen from the squash on.
- **Pre-squash / post-squash database**: a database migrated by a Lightstack from before or after
  the squash. A pre-squash database becomes post-squash only by being recreated.
- **Fragment**: one change's contribution to the next release's schema. Its name is its UTC
  creation time, then its ticket and purpose, and name order is the order it is applied in. It is
  plain or a backend pair, always with its down part, and has no version number.
- **Open release migration**: the fragments assembled into one migration at the version after the
  newest frozen one. It is unreleased and can still change.
- **Release migration**: the open release migration frozen at release time as `NNN_v<release>`.
  It never changes once it is on `main`.
- **Frozen migration** (formerly called "released migration"): any numbered migration on `main`,
  meaning the baseline from the squash on, or a release migration from its freeze. It never changes
  (FR-025).
- **Applied-state record**: Lightstack's own record, one entry for each post-squash migration a
  database applied, holding the checksum of what it applied. Among those entries is the state of the
  open release migration (FR-019). Its presence also marks a database as post-squash (FR-012).
  golang-migrate keeps no such record.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A new database applies 1 migration per backend, down from 83 on PostgreSQL and 11 on
  SQLite. The automated comparison finds 0 differences in schema and seed rows on both backends.
- **SC-002**: In a scripted scenario, two parallel branches both change the schema and are merged
  in both orders. The migrations folder has 0 conflicts, and 0 changes are skipped on fresh or
  upgraded databases.
- **SC-003**: Every refusal this feature adds names its cause and its way out. 0 of them surface as
  a raw migration-tool error or as "written by a newer Lightstack". The refusals cover: a
  pre-squash database; a stale open release migration; a changed numbered migration; a
  hand-numbered migration; and a release that still holds fragments.
- **SC-004**: *Removed on 2026-11-19 (Clarifications, Session 2026-11-19): there is no re-stamp; a
  database from before the squash is recreated (FR-011).*
- **SC-005**: Starting with the first release after the squash, each release adds at most one
  migration per backend, and the file name names the release.
- **SC-006**: One command tells a developer or operator whether a database is pre-squash, at a
  frozen migration or at the open release migration, and whether that open state is stale.
- **SC-007**: The unit tests on both backends, both contract suites and the quick integration test
  pass, and no test is weakened to get there.

## Assumptions

- Every migration merged to `main` before the squash is folded into the baseline. A numbered
  migration still on an open branch after the squash is rewritten as a fragment on that branch, and
  CI refuses it until then (FR-026).
- golang-migrate stays. Lightstack keeps its own applied-state record next to golang-migrate's
  version table.
- The baseline restarts at `001`, as the ticket says. Keeping the squash point's own number, as
  SQLite's `073` baseline does today, was considered and rejected (Clarifications). The restarted
  numbering is why the pre-squash marker (FR-012) exists.
- Release names are `v<semver>` and match the git tag. A maintainer runs the release step as part
  of cutting a release.
- Every `v*` tag is cut from `main`, as all five tags so far were, and pre-releases freeze like
  final releases. An older release line never gets a schema change, because linear numbering would
  hand it a number `main` also uses (Clarifications).
- No database has to be carried over. v0.6.1, the last release, shipped migrations up to `059`, so
  no release database records the squash point, and databases built from `main` are recreated
  (Clarifications).
- Tests that exercise one pre-squash migration leave with the history: the `groups.domain_id`
  conversion (`072`), the SQLite rebuild of `080`, and the endpoint-hostname rewrite (`069`, its
  script and Make target).
- Developers recreate the shared local PostgreSQL test database once (`make db-down && make
  db-up`). The SQLite test files are created afresh on every run already.
- No API, configuration, Helm chart or Compose change is needed. The binary migrates at startup,
  and the image ships the whole `migrations/` folder, fragments included.

## Out of Scope

- Replacing golang-migrate.
- Changing the schema or the seed data, including the leftovers of one-time fixes. The squash
  changes nothing a database holds.
- Converting a database from one backend to the other.
- Carrying a database from before the squash over. It is recreated (FR-011, FR-013).
- An upgrade script for bare metal. Bare-metal installs are upgraded by hand (FR-013).
- Catching a stale database up fragment by fragment. Stale databases are recreated.
- Schema changes on an older release line, such as a maintenance branch. Releases come from `main`
  only.
- Explicit dependencies between fragments. Creation order is the dependency order, and CI catches
  the exception.
- The release process beyond the freeze step and the tag-push check: tagging, images and the
  O3K-era `v1.0.0` tag.
- A release binary refusing an open release migration at runtime. The tag-push check covers the
  case (FR-027).
- SQLite support itself (#595), which this feature builds on.

## Dependencies

- The squash is cut from `main` after the numbered migrations it should fold in have merged. If a
  migration merges after the baselines were generated, they are generated again.
- It lands before v0.7.0 is tagged, so v0.7.0 is the first post-squash release (Clarifications).
  Today no deployed database has to survive. Once v0.7.0 is out, its databases do, and they start
  at the baseline.
