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
