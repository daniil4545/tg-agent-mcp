package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("-short: integration tier skipped")
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := Open(ctx, url, 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS tx_probe"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE tx_probe (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	return pool
}

// An error inside a transaction must roll everything back: a half-applied
// change is worse than an unapplied one because it looks successful.
func TestInTxRollsBackOnError(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	boom := errors.New("boom")
	err := InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO tx_probe (id) VALUES (1)"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error was not returned to the caller: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tx_probe").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("rows left after rollback: %d", count)
	}
}

func TestInTxCommitsOnSuccess(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	if err := InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO tx_probe (id) VALUES (2)")
		return err
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tx_probe WHERE id = 2").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows after commit: %d, expected one", count)
	}
}

// A pool opened against an unreachable database must fail at once: without the
// ping the service comes up "healthy" and breaks on the first query.
func TestOpenFailsOnUnreachableDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: integration tier skipped")
	}
	_, err := Open(context.Background(), "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable", 1)
	if err == nil {
		t.Fatal("pool opened against an unreachable database")
	}
}
