package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestReadsNotBlockedByLongWrite is the regression guard for the production
// incident where SetMaxOpenConns(1) serialized every query behind the daily
// cn-sync write transaction, turning sync windows into multi-minute stalls.
//
// It opens a write transaction, holds it open, and asserts that a concurrent
// read still completes quickly. With a single-connection pool this read would
// block until the writer commits (or time out); with WAL + a real pool it
// returns immediately.
func TestReadsNotBlockedByLongWrite(t *testing.T) {
	d := openTemp(t)

	if _, err := d.Exec(`INSERT INTO entries (category, field, jp_key, cn_text) VALUES ('cards','prefix','jp','cn')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Begin a write transaction and keep it open (simulating a long cn-sync).
	tx, err := d.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE entries SET cn_text='changed' WHERE jp_key='jp'`); err != nil {
		t.Fatalf("tx write: %v", err)
	}

	// A concurrent reader must not be blocked by the open writer.
	done := make(chan error, 1)
	go func() {
		var n int
		done <- d.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("concurrent read failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent read blocked by open write transaction — connection pool is starved (SetMaxOpenConns too low?)")
	}
}

// TestConcurrentWritersQueueNotDeadlock verifies that with _txlock=immediate two
// writers serialize via busy_timeout rather than deadlocking on lock upgrade
// (the SQLITE_BUSY trap that appears once the pool allows more than one conn).
func TestConcurrentWritersQueueNotDeadlock(t *testing.T) {
	d := openTemp(t)

	write := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		tx, err := d.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO entries (category, field, jp_key, cn_text) VALUES ('cards','prefix',?, 'x')`,
			time.Now().UnixNano()); err != nil {
			tx.Rollback()
			return err
		}
		time.Sleep(50 * time.Millisecond) // hold the write lock briefly
		return tx.Commit()
	}

	errs := make(chan error, 2)
	go func() { errs <- write() }()
	go func() { errs <- write() }()

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("concurrent writer failed (deadlock or busy timeout?): %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent writers did not complete — possible deadlock")
		}
	}
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
