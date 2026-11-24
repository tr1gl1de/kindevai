package db

import (
	"context"
	"database/sql"
	"fmt"
)

// Executor is what repositories run statements on: the *sql.DB or the current *sql.Tx.
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txKey struct{}

// Conn returns the transaction started by Transactor.InTx for ctx, or d when there is none. Repositories call it
// for every statement so a use case can make writes in several bounded contexts atomic without either context
// touching the other's tables (the pool has one connection, so bypassing the tx would also deadlock).
func Conn(ctx context.Context, d *sql.DB) Executor {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return tx
	}
	return d
}

// Transactor is the unit of work for application services (implements their TxRunner port).
type Transactor struct{ DB *sql.DB }

// InTx runs fn in one transaction (BEGIN IMMEDIATE via the _txlock DSN option) and commits if fn returns nil.
// A nested call joins the outer transaction.
func (t Transactor) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return fn(ctx)
	}
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit; releases the only connection if fn panics
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}
