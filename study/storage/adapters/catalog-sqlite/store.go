package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"gxstudy/adapter"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
)

type sqliteStore struct{ db *sql.DB }

func openStore(dir string) (storage, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "catalog.sqlite")}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS refs (repo TEXT PRIMARY KEY, current BLOB NOT NULL, previous BLOB)`); err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}
func (s *sqliteStore) point() string { return "SQLite WAL transaction COMMIT (synchronous=FULL)" }
func (s *sqliteStore) close() error  { return s.db.Close() }
func readSQL(ctx context.Context, tx *sql.Tx, repos []string) (state, error) {
	st := emptyState()
	if repos == nil {
		rows, err := tx.QueryContext(ctx, "SELECT repo,current,previous FROM refs")
		if err != nil {
			return st, err
		}
		defer rows.Close()
		for rows.Next() {
			var repo string
			var cur, prev []byte
			if err = rows.Scan(&repo, &cur, &prev); err != nil {
				return st, err
			}
			if err = decodeSQL(&st, repo, cur, prev); err != nil {
				return st, err
			}
		}
		return st, rows.Err()
	}
	for _, repo := range repos {
		var cur, prev []byte
		err := tx.QueryRowContext(ctx, "SELECT current,previous FROM refs WHERE repo=?", repo).Scan(&cur, &prev)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return st, err
		}
		if err = decodeSQL(&st, repo, cur, prev); err != nil {
			return st, err
		}
	}
	return st, nil
}
func decodeSQL(st *state, repo string, cur, prev []byte) error {
	var ref adapter.SegmentRef
	if err := json.Unmarshal(cur, &ref); err != nil {
		return err
	}
	st.Current[repo] = ref
	if len(prev) > 0 {
		if err := json.Unmarshal(prev, &ref); err != nil {
			return err
		}
		st.Previous[repo] = ref
	}
	return nil
}
func (s *sqliteStore) read(ctx context.Context, repos []string) (state, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return state{}, err
	}
	defer tx.Rollback()
	return readSQL(ctx, tx, repos)
}
func (s *sqliteStore) commit(ctx context.Context, set, old adapter.View) error {
	// A dedicated connection's BEGIN IMMEDIATE serializes the CAS with all writers.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	st := emptyState()
	for repo := range set {
		var cur, prev []byte
		err = conn.QueryRowContext(ctx, "SELECT current,previous FROM refs WHERE repo=?", repo).Scan(&cur, &prev)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if err = decodeSQL(&st, repo, cur, prev); err != nil {
			return err
		}
	}
	if _, err = applySet(st, set, old); err != nil {
		return err
	}
	adapter.Checkpoint("before-write")
	for repo, ref := range set {
		data, err := json.Marshal(ref)
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO refs(repo,current,previous) VALUES(?,?,NULL) ON CONFLICT(repo) DO UPDATE SET previous=refs.current,current=excluded.current`, repo, data); err != nil {
			return err
		}
	}
	adapter.Checkpoint("after-write")
	// SQLite performs its WAL flush inside COMMIT, not through a separate precommit API.
	adapter.Checkpoint("after-sync")
	adapter.Checkpoint("before-rename")
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	adapter.Checkpoint("after-rename")
	adapter.Checkpoint("after-dirsync")
	return nil
}
