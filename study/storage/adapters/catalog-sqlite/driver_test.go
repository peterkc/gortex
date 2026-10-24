package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSQLiteDriverDurabilitySettings(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(filepath.Join(t.TempDir(), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	db := store.(*sqliteStore).db
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for index, conn := range []*sql.Conn{first, second} {
		var version, mode string
		var synchronous, busy int
		if err = conn.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if version != "3.53.4" || mode != "wal" || synchronous != 2 || busy != 5000 {
			t.Fatal(index, version, mode, synchronous, busy)
		}
	}
}
