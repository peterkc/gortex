package main

import (
	"context"
	"database/sql"
	_ "modernc.org/sqlite"
	"sync"
)

const engineVersion = "modernc.org/sqlite v1.60.1"
const vectorLibrary = "study vecindex flat little-endian float32 (pure Go, no ANN)"
const vectorStorage = "flat"
const vectorMode = "exact float64 cosine over flat float32 file read once at Open"
const readOnlyNote = "mode=ro&immutable=1 and query_only(1) on every connection. SQLite and vector file SHA256 checked on every Open; vector file decoded into read-only Go memory."

func (Backend) Name() string { return "sqlite-vecflat" }
func openDB(ctx context.Context, path string, readOnly bool) (*sql.DB, func() error, error) {
	dsn := fileURI(path)
	if readOnly {
		dsn += "?mode=ro&immutable=1&_pragma=query_only(1)&_pragma=mmap_size(67108864)&_pragma=cache_size(-2048)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	var once sync.Once
	var closeErr error
	closeDB := func() error { once.Do(func() { closeErr = db.Close() }); return closeErr }
	if err = db.PingContext(ctx); err != nil {
		closeDB()
		return nil, nil, err
	}
	return db, closeDB, nil
}
