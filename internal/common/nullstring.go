package common

import "database/sql"

// NullString renders a sql.NullString the way the OpenStack APIs do: null
// when the column is NULL, the string otherwise. Emitting ns.String
// unconditionally turns a NULL into "" — a value the APIs never produce for
// optional string fields, and one the CLI renders differently from a real
// empty string (SPEC-028).
func NullString(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}
