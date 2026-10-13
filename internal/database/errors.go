package database

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Backend-neutral error classes. Each reports whether err is, or wraps, a violation of
// that class on either backend, so a handler can answer a conflict with 409 instead of
// a generic 500 without knowing which database it runs on.
//
// Matching the code rather than the error text is what makes the checks stable: the
// text depends on the server's locale and names a specific constraint, so it changes
// when a constraint is renamed. errors.As (inside pgCode) unwraps, so an error that
// has been through fmt.Errorf("...: %w", err) on its way up is still recognised.

// IsUniqueViolation reports a duplicate key: a primary-key or unique-constraint
// violation (PostgreSQL 23505; SQLite SQLITE_CONSTRAINT_UNIQUE and _PRIMARYKEY).
func IsUniqueViolation(err error) bool {
	c := sqliteCode(err)
	return pgCode(err) == "23505" || c == sqlite3.SQLITE_CONSTRAINT_UNIQUE || c == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// IsForeignKeyViolation reports a reference to a missing row, or the deletion of a
// referenced one (PostgreSQL 23503; SQLite SQLITE_CONSTRAINT_FOREIGNKEY).
func IsForeignKeyViolation(err error) bool {
	return pgCode(err) == "23503" || sqliteCode(err) == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

// IsNotNullViolation reports a NULL in a NOT NULL column (PostgreSQL 23502; SQLite
// SQLITE_CONSTRAINT_NOTNULL).
func IsNotNullViolation(err error) bool {
	return pgCode(err) == "23502" || sqliteCode(err) == sqlite3.SQLITE_CONSTRAINT_NOTNULL
}

// IsCheckViolation reports a value a CHECK constraint refuses (PostgreSQL 23514; SQLite
// SQLITE_CONSTRAINT_CHECK).
func IsCheckViolation(err error) bool {
	return pgCode(err) == "23514" || sqliteCode(err) == sqlite3.SQLITE_CONSTRAINT_CHECK
}

// IsBusy reports that the statement lost a race for a lock: on PostgreSQL a
// serialization failure (40001), a deadlock (40P01) or a lock that was not available
// (55P03); on SQLite SQLITE_BUSY or SQLITE_LOCKED with any extended code, including
// SQLITE_BUSY_SNAPSHOT.
func IsBusy(err error) bool {
	switch pgCode(err) {
	case "40001", "40P01", "55P03":
		return true
	}
	primary := sqliteCode(err) & 0xff
	return primary == sqlite3.SQLITE_BUSY || primary == sqlite3.SQLITE_LOCKED
}

// sqliteCode returns err's extended SQLite result code when it is (or wraps) a SQLite
// error, or 0. Extended codes are on by default in the driver.
func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()
	}
	return 0
}
