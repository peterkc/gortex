package main

import (
	"context"
	"database/sql/driver"
	libsql "github.com/tursodatabase/go-libsql"
	"path/filepath"
	"testing"
)

func TestLocalURIReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "uri.sqlite")
	db, closeDB, err := openDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "CREATE TABLE x(n)"); err != nil {
		t.Fatal(err)
	}
	if err = closeDB(); err != nil {
		t.Fatal(err)
	}
	c, err := new(libsql.Connector).Driver().(driver.DriverContext).OpenConnector(fileURI(path) + "?mode=ro&immutable=1")
	if err != nil {
		t.Log("URI read-only unavailable", err)
		return
	}
	defer c.(*libsql.Connector).Close()
	conn, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.(driver.ExecerContext).ExecContext(ctx, "INSERT INTO x VALUES (1)", nil)
	if err == nil {
		t.Fatal("URI read-only parameters ignored")
	}
	t.Log("URI read-only write rejected", err)
}
