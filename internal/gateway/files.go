package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mactunnel/internal/protocol"
)

// ErrFileUnavailable means a job named an input file the key can't use.
var ErrFileUnavailable = errors.New("input file is unavailable")

// FileView is the agent-facing description of an uploaded input file.
type FileView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ExpiresAt string `json:"expires_at"`
}

// AddInputFile records a file an agent uploaded.
func (s *Store) AddInputFile(ctx context.Context, id, apiKeyID, name, sum string, size int64, ttl time.Duration) (FileView, error) {
	expires := nowMS() + ttl.Milliseconds()
	_, err := s.db.ExecContext(ctx, `INSERT INTO input_files (id, api_key_id, name, sha256, size, expires_ms) VALUES (?,?,?,?,?,?)`,
		id, apiKeyID, name, sum, size, expires)
	return FileView{ID: id, Name: name, Size: size, SHA256: sum, ExpiresAt: formatMS(expires)}, err
}

// InputFileFor returns an input file if the API key owns it and it's unexpired.
func (s *Store) InputFileFor(ctx context.Context, fileID, apiKeyID string) (*ArtifactDownload, error) {
	var f ArtifactDownload
	err := s.db.QueryRowContext(ctx, `SELECT id, name, sha256 FROM input_files WHERE id=? AND api_key_id=? AND expires_ms > ?`,
		fileID, apiKeyID, nowMS()).Scan(&f.ID, &f.Name, &f.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

// checkAndAttachFiles links a new job to its input files. A file referenced by
// an unfinished job isn't deleted, even after it passes its expiry.
func checkAndAttachFiles(tx *sql.Tx, j NewJob, jobID string) error {
	for field, fileID := range j.Files {
		var found int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM input_files WHERE id=? AND api_key_id=? AND expires_ms > ?`,
			fileID, j.APIKeyID, nowMS()).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			return fmt.Errorf("%w: %s", ErrFileUnavailable, field)
		}
		if _, err := tx.Exec(`INSERT INTO job_files (job_id, field, file_id) VALUES (?,?,?)`, jobID, field, fileID); err != nil {
			return err
		}
	}
	return nil
}

// JobInputFiles lists what a worker must download before running a job.
func (s *Store) JobInputFiles(ctx context.Context, jobID string) ([]protocol.InputFile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT jf.field, f.name, f.sha256, f.size FROM job_files jf
		JOIN input_files f ON f.id = jf.file_id
		WHERE jf.job_id=? ORDER BY jf.field`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.InputFile
	for rows.Next() {
		var f protocol.InputFile
		if err := rows.Scan(&f.Field, &f.Name, &f.SHA256, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AttemptInputFile returns an input file of the attempt's job.
func (s *Store) AttemptInputFile(ctx context.Context, attemptID, field string) (*ArtifactDownload, error) {
	var f ArtifactDownload
	err := s.db.QueryRowContext(ctx, `
		SELECT f.id, f.name, f.sha256 FROM attempts a
		JOIN job_files jf ON jf.job_id = a.job_id AND jf.field = ?
		JOIN input_files f ON f.id = jf.file_id
		WHERE a.id=?`, field, attemptID).Scan(&f.ID, &f.Name, &f.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

// ArtifactStored reports whether this exact file has already been recorded, so
// a worker that lost the response to its upload doesn't have to send it again.
func (s *Store) ArtifactStored(ctx context.Context, attemptID, name, sum string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE attempt_id=? AND name=? AND sha256=?`, attemptID, name, sum).Scan(&found)
	return found > 0, err
}

// RemoveDeadInputFiles deletes expired input files that no unfinished job needs.
// It returns the file IDs to remove from disk.
func (s *Store) RemoveDeadInputFiles(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		ids = nil
		rows, err := tx.Query(`
			SELECT f.id FROM input_files f
			WHERE f.expires_ms < ? AND NOT EXISTS (
				SELECT 1 FROM job_files jf JOIN jobs j ON j.id = jf.job_id
				WHERE jf.file_id = f.id AND j.status NOT IN (?,?,?,?)
			)`, nowMS(), StatusSucceeded, StatusFailed, StatusTimedOut, StatusCancelled)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`DELETE FROM input_files WHERE id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	return ids, err
}

// CancelJob stops a job the API key owns. Cancelling a finished job changes
// nothing, so it's always safe to repeat. It returns found=false for a job the
// key can't see, and the attempt a worker should stop, if any.
func (s *Store) CancelJob(ctx context.Context, jobID, apiKeyID string) (found bool, abort *AttemptRef, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		found, abort = false, nil
		var status string
		e := tx.QueryRow(`SELECT status FROM jobs WHERE id=? AND api_key_id=?`, jobID, apiKeyID).Scan(&status)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		found = true
		if isTerminal(status) {
			return nil
		}
		held, e := queryAttempts(tx, `WHERE a.job_id=? AND a.status=? AND j.current_attempt=a.id`, jobID, attemptActive)
		if e != nil {
			return e
		}
		for _, a := range held {
			if e := setAttemptStatus(tx, a.id, attemptFenced); e != nil {
				return e
			}
			abort = &AttemptRef{NodeID: a.nodeID, AttemptID: a.id, Reason: "cancelled"}
		}
		return finishJob(tx, jobID, StatusCancelled, "", "")
	})
	return found, abort, err
}

// JobEventsAfter returns a job's events after a cursor, oldest first. The cursor
// is the event's id, which is unique and increasing within the database.
func (s *Store) JobEventsAfter(ctx context.Context, jobID string, after int64) ([]EventView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, attempt_id, seq, type, payload, created_ms FROM job_events
		WHERE job_id=? AND rowid > ? ORDER BY rowid`, jobID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventView
	for rows.Next() {
		var e EventView
		var payload sql.NullString
		var at int64
		if err := rows.Scan(&e.ID, &e.Attempt, &e.Seq, &e.Type, &payload, &at); err != nil {
			return nil, err
		}
		if payload.Valid {
			e.Payload = json.RawMessage(payload.String)
		}
		e.At = formatMS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ensureColumn adds a column to a database created by an older version.
func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	exists := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		exists = exists || name == column
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition))
	return err
}
