package common

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgUniqueViolation is Postgres SQLSTATE 23505. Matching the code rather than the error
// text is what makes the check stable: the text depends on the server's locale and names
// a specific constraint, so it changes when a constraint is renamed.
const pgUniqueViolation = "23505"

// IsUniqueViolation reports whether err is a Postgres unique-constraint violation, so a
// name collision can be answered with 409 instead of a generic 500.
//
// It uses errors.As rather than a type assertion because a wrapped error — anything that
// has been through fmt.Errorf("...: %w", err) on its way up — fails the assertion and
// would silently downgrade the conflict back to a server fault.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
