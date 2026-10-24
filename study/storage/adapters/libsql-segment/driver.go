package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"

	libsql "github.com/tursodatabase/go-libsql"
)

const engineVersion = "github.com/tursodatabase/go-libsql v0.0.0-20260424063416-3051e37e6e04; bundled SQLite core 3.45.1"
const nativeVector = true
const vectorMode = "native DiskANN libsql_vector_idx; returned candidates rescored with float64 cosine"
const vectorIndexSQL = "CREATE INDEX vectors_ann ON vectors(libsql_vector_idx(embedding))"
const readOnlyNote = "mode=ro&immutable=1 plus query_only=ON per connection; 0444 file. URI write rejection is fixture-tested. Full-file SHA256 checked on every Open."

func (Backend) Name() string { return "libsql-segment" }

type servingConnector struct{ driver.Connector }

func (c servingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	ex := conn.(driver.ExecerContext)
	for _, q := range []string{"PRAGMA query_only=ON", "PRAGMA cache_size=-2048"} {
		if _, err = ex.ExecContext(ctx, q, nil); err != nil {
			conn.Close()
			return nil, err
		}
	}
	rows, err := conn.(driver.QueryerContext).QueryContext(ctx, "PRAGMA mmap_size=67108864", nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
func openDB(ctx context.Context, path string, readOnly bool) (*sql.DB, func() error, error) {
	drv := new(libsql.Connector).Driver().(driver.DriverContext)
	dsn := fileURI(path)
	if readOnly {
		dsn += "?mode=ro&immutable=1"
	}
	connector, err := drv.OpenConnector(dsn)
	if err != nil {
		return nil, nil, err
	}
	native := connector.(*libsql.Connector)
	var c driver.Connector = connector
	if readOnly {
		c = servingConnector{connector}
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	var once sync.Once
	var closeErr error
	closeDB := func() error { once.Do(func() { closeErr = errors.Join(db.Close(), native.Close()) }); return closeErr }
	if err = db.PingContext(ctx); err != nil {
		closeDB()
		return nil, nil, err
	}
	return db, closeDB, nil
}
