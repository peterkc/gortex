package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

const fileIndexFailuresTableBody = ` (
    view_gen          INTEGER NOT NULL DEFAULT 0,
    repo_prefix       TEXT NOT NULL,
    file_path         TEXT NOT NULL,
    error             TEXT NOT NULL,
    permission_denied INTEGER NOT NULL DEFAULT 0,
    workspace_id      TEXT NOT NULL DEFAULT '',
    project_id        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (view_gen, repo_prefix, file_path)
) WITHOUT ROWID`

func createFileIndexFailuresTable(tx *sql.Tx) error {
	_, err := tx.Exec("CREATE TABLE IF NOT EXISTS file_index_failures" + fileIndexFailuresTableBody)
	return err
}

func (s *Store) FileIndexFailuresForRepo(repoPrefix string) ([]graph.FileIndexFailure, error) {
	rows, err := s.db.Query(`SELECT file_path, error, permission_denied, workspace_id, project_id
FROM file_index_failures WHERE view_gen = ? AND repo_prefix = ? ORDER BY file_path`, s.viewGen, repoPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []graph.FileIndexFailure{}
	for rows.Next() {
		failure := graph.FileIndexFailure{RepoPrefix: repoPrefix}
		if err := rows.Scan(&failure.Path, &failure.Error, &failure.PermissionDenied, &failure.WorkspaceID, &failure.ProjectID); err != nil {
			return nil, err
		}
		out = append(out, failure)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceFileIndexFailures(repoPrefix string, failures []graph.FileIndexFailure) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := tx.Exec(`DELETE FROM file_index_failures WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	// Seven parameters per row; share the bounded file-metadata batch size.
	for start := 0; start < len(failures); start += fileMetaChunk {
		batch := failures[start:min(start+fileMetaChunk, len(failures))]
		stmt := []byte("INSERT OR REPLACE INTO file_index_failures (view_gen, repo_prefix, file_path, error, permission_denied, workspace_id, project_id) VALUES ")
		args := make([]any, 0, len(batch)*7)
		for i, failure := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, repoPrefix, failure.Path, failure.Error, failure.PermissionDenied, failure.WorkspaceID, failure.ProjectID)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return s.commitPayloadTx(tx)
}

var (
	_ graph.FileIndexFailureReader = (*Store)(nil)
	_ graph.FileIndexFailureWriter = (*Store)(nil)
)
