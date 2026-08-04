package common

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsUniqueViolation covers the three shapes the callers actually see, and one the
// implementation this replaced got wrong.
//
// internal/nova/flavors.go used `err.(*pgconn.PgError)`, a bare type assertion. That is
// correct only for an error handed straight back by pgx: the moment anything wraps it —
// one fmt.Errorf("...: %w", err) anywhere on the way up, which the project's own error
// convention asks for — the assertion fails and a duplicate name silently downgrades
// from 409 to a generic 500. errors.As unwraps, so the wrapped case below is the reason
// this helper exists rather than three copies of the assertion (SPEC-022 review SRC-7).
func TestIsUniqueViolation(t *testing.T) {
	unique := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"a unique violation", unique, true},
		{"a wrapped unique violation", fmt.Errorf("failed to create project: %w", unique), true},
		{"doubly wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", unique)), true},
		{"a different postgres error", &pgconn.PgError{Code: "23503"}, false},
		{"invalid text representation (22P02)", &pgconn.PgError{Code: "22P02"}, false},
		{"a plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUniqueViolation(tc.err); got != tc.want {
				t.Fatalf("IsUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
