package db

import (
	"context"
	"errors"
	"testing"
)

func countRows(t *testing.T, ex Executor) int {
	t.Helper()
	var n int
	if err := ex.QueryRowContext(t.Context(), "SELECT count(*) FROM items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTransactorCommitsAndRollsBack(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if _, err := d.ExecContext(t.Context(), "CREATE TABLE items(v TEXT)"); err != nil {
		t.Fatal(err)
	}
	tx := Transactor{DB: d}
	insert := func(ctx context.Context) error {
		_, err := Conn(ctx, d).ExecContext(ctx, "INSERT INTO items(v) VALUES ('x')")
		return err
	}

	if err := tx.InTx(t.Context(), insert); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if got := countRows(t, d); got != 1 {
		t.Fatalf("after commit: %d rows, want 1", got)
	}

	boom := errors.New("boom")
	err := tx.InTx(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx); err != nil {
			return err
		}
		if got := countRows(t, Conn(ctx, d)); got != 2 { // the transaction sees its own write
			t.Errorf("inside tx: %d rows, want 2", got)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx error = %v, want boom", err)
	}
	if got := countRows(t, d); got != 1 {
		t.Fatalf("after rollback: %d rows, want 1", got)
	}
}

func TestTransactorNestedCallsJoinTheOuterTransaction(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if _, err := d.ExecContext(t.Context(), "CREATE TABLE items(v TEXT)"); err != nil {
		t.Fatal(err)
	}
	tx := Transactor{DB: d}
	err := tx.InTx(t.Context(), func(ctx context.Context) error {
		// With one connection in the pool, a second BEGIN would deadlock; the nested call must reuse the tx.
		if err := tx.InTx(ctx, func(ctx context.Context) error {
			_, err := Conn(ctx, d).ExecContext(ctx, "INSERT INTO items(v) VALUES ('x')")
			return err
		}); err != nil {
			return err
		}
		return errors.New("roll back everything")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if got := countRows(t, d); got != 0 {
		t.Fatalf("nested write survived the outer rollback: %d rows", got)
	}
}

func TestConnWithoutTransactionUsesTheDB(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if Conn(t.Context(), d) != Executor(d) {
		t.Fatal("Conn without a transaction must return the DB")
	}
}
