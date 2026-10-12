package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
)

// countingQuery has no arguments, so a run of a statement still running
// would carry on from where it was rather than fail to bind.
const countingQuery = `SELECT value FROM json_each('[1, 2, 3]')`

func readRest(t *testing.T, rows *sql.Rows) []int {
	t.Helper()
	var values []int
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return values
}

func readCounting(t *testing.T, tx *sql.Tx) []int {
	t.Helper()
	rows, err := tx.Query(countingQuery)
	if err != nil {
		t.Fatal(err)
	}
	return readRest(t, rows)
}

// beginCounting starts a transaction on one read connection, runs
// countingQuery there once so the connection caches it, and then starts
// it again and reads its first row.
func beginCounting(t *testing.T, store *Store) (*sql.Tx, *sql.Rows) {
	t.Helper()
	tx, err := store.reads.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	readCounting(t, tx)
	rows, err := tx.Query(countingQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no first row: %v", rows.Err())
	}
	return tx, rows
}

func TestQueryRunsAgainWhileItsRowsAreOpen(t *testing.T) {
	store := openTestStore(t)
	tx, outer := beginCounting(t, store)

	if got := readCounting(t, tx); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("inner run read %v, want [1 2 3]", got)
	}
	if got := readRest(t, outer); !slices.Equal(got, []int{2, 3}) {
		t.Errorf("outer run read %v after its first row, want [2 3]", got)
	}
	if got := readCounting(t, tx); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("run after both read %v, want [1 2 3]", got)
	}
}

func TestFullStatementCacheSparesRunningStatement(t *testing.T) {
	store := openTestStore(t)
	tx, running := beginCounting(t, store)

	for n := range maxCachedStatements + 1 {
		var got int
		if err := tx.QueryRow(fmt.Sprintf(`SELECT %d`, n)).Scan(
			&got,
		); err != nil {
			t.Fatal(err)
		}
	}
	if got := readRest(t, running); !slices.Equal(got, []int{2, 3}) {
		t.Errorf("running query read %v after its first row, want [2 3]",
			got)
	}
	if got := readCounting(t, tx); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("run after the cache filled read %v, want [1 2 3]", got)
	}
}
