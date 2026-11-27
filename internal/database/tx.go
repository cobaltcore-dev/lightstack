package database

import (
	"context"
	"fmt"
)

// WithTx executes fn within a transaction on db.
// If fn returns an error, the transaction is rolled back. Otherwise committed.
func WithTx(ctx context.Context, db DB, fn func(tx Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("rollback failed: %v (original: %w)", rbErr, err)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// AdvisoryLock takes the transaction-scoped advisory lock named key, held until tx commits or rolls
// back, so there is no unlock path to forget. It serialises a decision no row lock can: one about a row
// that may not exist yet, or a value read across rows and then claimed. On SQLite it is a registered
// no-op: an immediate transaction already holds the database's one write lock.
func AdvisoryLock(ctx context.Context, tx Tx, key string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
		return fmt.Errorf("take the lock %s: %w", key, err)
	}
	return nil
}
