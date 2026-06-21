// Package store persists backup run history in SQLite (pure-Go driver, no cgo).
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database holding run history.
type Store struct {
	db *sql.DB
}

// Run is one recorded execution of a job.
type Run struct {
	ID              int64
	Job             string
	StartedAt       time.Time
	FinishedAt      time.Time
	Status          string // "success" | "failed"
	SnapshotID      string
	FilesNew        int64
	FilesChanged    int64
	FilesUnmodified int64
	DataAddedBytes  int64
	BytesProcessed  int64
	ExitCode        int
	ErrMsg          string
}

// Open opens (creating if needed) the SQLite database at path and applies schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    job              TEXT    NOT NULL,
    started_at       TEXT    NOT NULL,
    finished_at      TEXT    NOT NULL,
    status           TEXT    NOT NULL,
    snapshot_id      TEXT    NOT NULL DEFAULT '',
    files_new        INTEGER NOT NULL DEFAULT 0,
    files_changed    INTEGER NOT NULL DEFAULT 0,
    files_unmodified INTEGER NOT NULL DEFAULT 0,
    data_added_bytes INTEGER NOT NULL DEFAULT 0,
    bytes_processed  INTEGER NOT NULL DEFAULT 0,
    exit_code        INTEGER NOT NULL DEFAULT 0,
    err_msg          TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_job_started ON runs(job, started_at DESC);`)
	return err
}

// RecordRun inserts a finished run and returns its row ID.
func (s *Store) RecordRun(r Run) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO runs (job, started_at, finished_at, status, snapshot_id,
                  files_new, files_changed, files_unmodified,
                  data_added_bytes, bytes_processed, exit_code, err_msg)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Job, r.StartedAt.UTC().Format(time.RFC3339), r.FinishedAt.UTC().Format(time.RFC3339),
		r.Status, r.SnapshotID, r.FilesNew, r.FilesChanged, r.FilesUnmodified,
		r.DataAddedBytes, r.BytesProcessed, r.ExitCode, r.ErrMsg)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecentRuns returns up to limit runs, newest first. Empty job means all jobs.
func (s *Store) RecentRuns(job string, limit int) ([]Run, error) {
	q := `SELECT id, job, started_at, finished_at, status, snapshot_id,
                 files_new, files_changed, files_unmodified,
                 data_added_bytes, bytes_processed, exit_code, err_msg
          FROM runs`
	args := []any{}
	if job != "" {
		q += " WHERE job = ?"
		args = append(args, job)
	}
	q += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		var started, finished string
		if err := rows.Scan(&r.ID, &r.Job, &started, &finished, &r.Status, &r.SnapshotID,
			&r.FilesNew, &r.FilesChanged, &r.FilesUnmodified,
			&r.DataAddedBytes, &r.BytesProcessed, &r.ExitCode, &r.ErrMsg); err != nil {
			return nil, err
		}
		r.StartedAt, _ = time.Parse(time.RFC3339, started)
		r.FinishedAt, _ = time.Parse(time.RFC3339, finished)
		out = append(out, r)
	}
	return out, rows.Err()
}
