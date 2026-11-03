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

// This file holds every job and attempt state transition. Each exported method
// is one transaction, so a crash cannot leave a job half transitioned.
//
// The rules that keep results correct:
//
//   - Only jobs.current_attempt may change a job. Anything else is fenced, and
//     the worker holding it is told to abort.
//   - Losing contact with a worker does not end its attempt. The attempt stays
//     "active" and is revived if the worker returns before it is replaced.
//   - An attempt that may have started is rerun only if its capability opted in
//     with retry_if_lost, because repeating the work may not be safe.

// RetryPolicy holds the limits used by state transitions.
type RetryPolicy struct {
	Lease         time.Duration
	LostGrace     time.Duration
	MaxAttempts   int
	DeadlineGrace time.Duration
}

// AttemptRef names an attempt that a worker should be told to abort.
type AttemptRef struct {
	NodeID    string
	AttemptID string
	Reason    string
}

type attemptInfo struct {
	id, jobID, nodeID, status string
	lastSeq                   int64
	retry                     bool
	started                   sql.NullInt64
	jobStatus                 string
	current                   sql.NullString
	jobAttempts               int
	queueTimeoutMS            int64
}

const attemptInfoSQL = `
	SELECT a.id, a.job_id, a.node_id, a.status, a.last_seq, a.retry_if_lost, a.started_ms,
	       j.status, j.current_attempt, j.attempts, j.queue_timeout_ms
	FROM attempts a JOIN jobs j ON j.id = a.job_id `

func scanAttempt(scan func(...any) error) (attemptInfo, error) {
	var a attemptInfo
	var retry int
	err := scan(&a.id, &a.jobID, &a.nodeID, &a.status, &a.lastSeq, &retry, &a.started,
		&a.jobStatus, &a.current, &a.jobAttempts, &a.queueTimeoutMS)
	a.retry = retry != 0
	return a, err
}

func (a attemptInfo) isCurrent() bool {
	return a.current.Valid && a.current.String == a.id
}

// queryAttempts reads all matching rows before returning, because a transaction
// cannot run another statement on its only connection while rows are open.
func queryAttempts(tx *sql.Tx, where string, args ...any) ([]attemptInfo, error) {
	rows, err := tx.Query(attemptInfoSQL+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attemptInfo
	for rows.Next() {
		a, err := scanAttempt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func finishJob(tx *sql.Tx, jobID, status, code, message string) error {
	var errCode, errMessage any
	if code != "" {
		errCode, errMessage = code, message
	}
	_, err := tx.Exec(`UPDATE jobs SET status=?, error_code=?, error_message=?, finished_ms=?, lost_deadline_ms=NULL WHERE id=?`,
		status, errCode, errMessage, nowMS(), jobID)
	return err
}

func requeueJob(tx *sql.Tx, a attemptInfo) error {
	_, err := tx.Exec(`UPDATE jobs SET status=?, queue_deadline_ms=?, lost_deadline_ms=NULL WHERE id=?`,
		StatusQueued, nowMS()+a.queueTimeoutMS, a.jobID)
	return err
}

func setAttemptStatus(tx *sql.Tx, attemptID, status string) error {
	_, err := tx.Exec(`UPDATE attempts SET status=? WHERE id=?`, status, attemptID)
	return err
}

// revive restores a job whose current attempt turned out to be alive.
func revive(tx *sql.Tx, a attemptInfo, lease time.Duration) error {
	status := StatusAssigned
	if a.started.Valid {
		status = StatusRunning
	}
	if _, err := tx.Exec(`UPDATE jobs SET status=?, lost_deadline_ms=NULL WHERE id=?`, status, a.jobID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE attempts SET lease_expires_ms=? WHERE id=?`, nowMS()+lease.Milliseconds(), a.id)
	return err
}

// --- Worker connection ---

// Reconcile compares what a reconnecting worker holds with what the gateway
// expects. It returns the acked sequence for attempts the worker should keep
// and the IDs of those it should abort.
func (s *Store) Reconcile(ctx context.Context, nodeID string, reported []protocol.AttemptState, p RetryPolicy) (acks map[string]int64, abort []string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		acks = map[string]int64{}
		abort = nil
		held := make(map[string]bool, len(reported))

		for _, r := range reported {
			held[r.AttemptID] = true
			matches, err := queryAttempts(tx, `WHERE a.id=? AND a.node_id=?`, r.AttemptID, nodeID)
			if err != nil {
				return err
			}
			if len(matches) == 0 {
				abort = append(abort, r.AttemptID)
				continue
			}
			a := matches[0]
			switch {
			case !a.isCurrent() || a.status == attemptFenced:
				abort = append(abort, a.id)
			case a.status == attemptFinished:
				acks[a.id] = a.lastSeq // The worker just needs this ack to clean up.
			case isTerminal(a.jobStatus):
				abort = append(abort, a.id)
			default:
				if a.jobStatus == StatusQueued || a.jobStatus == StatusLost {
					if err := revive(tx, a, p.Lease); err != nil {
						return err
					}
				}
				acks[a.id] = a.lastSeq
			}
		}

		// Attempts the gateway thinks are on this node but the worker doesn't
		// have. The worker persists an attempt before running it, so a missing
		// attempt that never reported "started" has definitely not run.
		active, err := queryAttempts(tx, `WHERE a.node_id=? AND a.status=? AND j.current_attempt=a.id`, nodeID, attemptActive)
		if err != nil {
			return err
		}
		for _, a := range active {
			if held[a.id] || isTerminal(a.jobStatus) {
				continue
			}
			if err := setAttemptStatus(tx, a.id, attemptFenced); err != nil {
				return err
			}
			canRetry := (a.retry || !a.started.Valid) && a.jobAttempts < p.MaxAttempts
			if canRetry {
				err = requeueJob(tx, a)
			} else {
				err = finishJob(tx, a.jobID, StatusFailed, CodeNodeLost, "the worker restarted without this job's state")
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return acks, abort, err
}

// Heartbeat renews the node's leases, revives any job whose lease lapsed while
// the connection stayed up, and reports whether the node is still authorized.
func (s *Store) Heartbeat(ctx context.Context, nodeID string, p RetryPolicy) (authorized bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var revoked sql.NullInt64
		if err := tx.QueryRow(`SELECT revoked_ms FROM nodes WHERE id=?`, nodeID).Scan(&revoked); err != nil {
			return err
		}
		authorized = !revoked.Valid
		if !authorized {
			return nil
		}
		now := nowMS()
		if _, err := tx.Exec(`UPDATE nodes SET last_seen_ms=? WHERE id=?`, now, nodeID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE attempts SET lease_expires_ms=? WHERE node_id=? AND status=?`, now+p.Lease.Milliseconds(), nodeID, attemptActive); err != nil {
			return err
		}
		lapsed, err := queryAttempts(tx, `WHERE a.node_id=? AND a.status=? AND j.current_attempt=a.id AND j.status IN (?,?)`,
			nodeID, attemptActive, StatusQueued, StatusLost)
		if err != nil {
			return err
		}
		for _, a := range lapsed {
			if err := revive(tx, a, p.Lease); err != nil {
				return err
			}
		}
		return nil
	})
	return authorized, err
}

// EventResult tells the connection handler how to answer a worker event.
type EventResult struct {
	Ack     int64
	Abort   bool
	Changed bool // The job reached a state where the scheduler should rerun.
}

// ApplyEvent stores one worker event. Events are applied strictly in sequence,
// so resending after a reconnect is harmless.
func (s *Store) ApplyEvent(ctx context.Context, nodeID string, ev protocol.Event, p RetryPolicy) (EventResult, error) {
	var res EventResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		matches, err := queryAttempts(tx, `WHERE a.id=? AND a.node_id=?`, ev.AttemptID, nodeID)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			res.Abort = true
			return nil
		}
		a := matches[0]
		switch {
		case !a.isCurrent() || a.status == attemptFenced:
			res.Abort = true
			return nil
		case a.status == attemptFinished:
			res.Ack = a.lastSeq
			return nil
		case isTerminal(a.jobStatus):
			res.Abort = true
			return nil
		case ev.Seq != a.lastSeq+1:
			// A duplicate from a resend, or out of order: don't store it, just say
			// how far the gateway has got.
			res.Ack = a.lastSeq
			return nil
		}

		now := nowMS()
		if _, err := tx.Exec(`INSERT INTO job_events (attempt_id, seq, job_id, type, payload, created_ms) VALUES (?,?,?,?,?,?)`,
			a.id, ev.Seq, a.jobID, ev.Type, nullableJSON(ev.Payload), now); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE attempts SET last_seq=?, lease_expires_ms=? WHERE id=?`, ev.Seq, now+p.Lease.Milliseconds(), a.id); err != nil {
			return err
		}
		res.Ack = ev.Seq

		if a.jobStatus == StatusQueued || a.jobStatus == StatusLost {
			if err := revive(tx, a, p.Lease); err != nil {
				return err
			}
		}

		switch ev.Type {
		case protocol.EventStarted:
			if _, err := tx.Exec(`UPDATE attempts SET started_ms=? WHERE id=?`, now, a.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE jobs SET status=?, started_ms=COALESCE(started_ms, ?) WHERE id=?`, StatusRunning, now, a.jobID); err != nil {
				return err
			}
		case protocol.EventProgress:
			if _, err := tx.Exec(`UPDATE jobs SET progress=? WHERE id=?`, nullableJSON(ev.Payload), a.jobID); err != nil {
				return err
			}
		case protocol.EventCompleted:
			res.Changed = true
			return applyCompleted(tx, a, ev.Payload)
		case protocol.EventFailed:
			res.Changed = true
			return applyFailed(tx, a, ev.Payload, p)
		}
		return nil
	})
	return res, err
}

func applyCompleted(tx *sql.Tx, a attemptInfo, payload json.RawMessage) error {
	if err := setAttemptStatus(tx, a.id, attemptFinished); err != nil {
		return err
	}
	var done protocol.Completion
	if err := json.Unmarshal(payload, &done); err != nil {
		return finishJob(tx, a.jobID, StatusFailed, CodeArtifactError, "the worker sent an unreadable completion")
	}
	var stored int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE attempt_id=?`, a.id).Scan(&stored); err != nil {
		return err
	}
	if stored != len(done.Artifacts) {
		return finishJob(tx, a.jobID, StatusFailed, CodeArtifactError, fmt.Sprintf("the worker declared %d artifacts but %d are stored", len(done.Artifacts), stored))
	}
	for _, ref := range done.Artifacts {
		var sum string
		var size int64
		err := tx.QueryRow(`SELECT sha256, size FROM artifacts WHERE attempt_id=? AND name=?`, a.id, ref.Name).Scan(&sum, &size)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (sum != ref.SHA256 || size != ref.Size)) {
			return finishJob(tx, a.jobID, StatusFailed, CodeArtifactError, "artifact does not match the upload: "+ref.Name)
		}
		if err != nil {
			return err
		}
	}
	return finishJob(tx, a.jobID, StatusSucceeded, "", "")
}

func applyFailed(tx *sql.Tx, a attemptInfo, payload json.RawMessage, p RetryPolicy) error {
	if err := setAttemptStatus(tx, a.id, attemptFinished); err != nil {
		return err
	}
	var failure protocol.Failure
	if err := json.Unmarshal(payload, &failure); err != nil || failure.Code == "" {
		failure = protocol.Failure{Code: protocol.CodeCommandFailed, Message: "the worker sent an unreadable failure"}
	}
	switch failure.Code {
	case protocol.CodeWorkerRestarted:
		// The work died with the worker, so this is the same as losing the node.
		if a.retry && a.jobAttempts < p.MaxAttempts {
			return requeueJob(tx, a)
		}
		return finishJob(tx, a.jobID, StatusFailed, failure.Code, failure.Message)
	case protocol.CodeTimedOut:
		return finishJob(tx, a.jobID, StatusTimedOut, failure.Code, failure.Message)
	default:
		return finishJob(tx, a.jobID, StatusFailed, failure.Code, failure.Message)
	}
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// --- Maintenance ---

// ExtendActiveLeases gives workers time to reconnect after a gateway restart,
// so a redeploy doesn't look like every node vanished at once.
func (s *Store) ExtendActiveLeases(ctx context.Context, p RetryPolicy) error {
	_, err := s.db.ExecContext(ctx, `UPDATE attempts SET lease_expires_ms=? WHERE status=? AND lease_expires_ms < ?`,
		nowMS()+p.Lease.Milliseconds(), attemptActive, nowMS()+p.Lease.Milliseconds())
	return err
}

// ExpireLeases handles attempts whose worker has gone quiet. The attempt stays
// active so it can be revived if the worker returns first.
func (s *Store) ExpireLeases(ctx context.Context, p RetryPolicy) (changed int, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		changed = 0
		expired, err := queryAttempts(tx, `WHERE a.status=? AND j.current_attempt=a.id AND j.status IN (?,?) AND a.lease_expires_ms < ?`,
			attemptActive, StatusAssigned, StatusRunning, nowMS())
		if err != nil {
			return err
		}
		for _, a := range expired {
			if a.retry && a.jobAttempts < p.MaxAttempts {
				err = requeueJob(tx, a)
			} else {
				_, err = tx.Exec(`UPDATE jobs SET status=?, lost_deadline_ms=? WHERE id=?`, StatusLost, nowMS()+p.LostGrace.Milliseconds(), a.jobID)
			}
			if err != nil {
				return err
			}
			changed++
		}
		return nil
	})
	return changed, err
}

// ExpireLost fails jobs whose worker did not return within the grace period.
func (s *Store) ExpireLost(ctx context.Context) ([]AttemptRef, error) {
	return s.fenceAndFinish(ctx,
		`WHERE a.status=? AND j.current_attempt=a.id AND j.status=? AND j.lost_deadline_ms < ?`,
		[]any{attemptActive, StatusLost, nowMS()},
		StatusFailed, CodeNodeLost, "the worker did not come back in time")
}

// ExpireHardDeadlines is the backstop for workers that never report an outcome.
func (s *Store) ExpireHardDeadlines(ctx context.Context) ([]AttemptRef, error) {
	return s.fenceAndFinish(ctx,
		`WHERE a.status=? AND j.current_attempt=a.id AND j.status IN (?,?,?) AND a.hard_deadline_ms < ?`,
		[]any{attemptActive, StatusAssigned, StatusRunning, StatusLost, nowMS()},
		StatusTimedOut, CodeDeadline, "the job passed its deadline without a result")
}

func (s *Store) fenceAndFinish(ctx context.Context, where string, args []any, status, code, message string) ([]AttemptRef, error) {
	var aborts []AttemptRef
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		aborts = nil
		matches, err := queryAttempts(tx, where, args...)
		if err != nil {
			return err
		}
		for _, a := range matches {
			if err := setAttemptStatus(tx, a.id, attemptFenced); err != nil {
				return err
			}
			if err := finishJob(tx, a.jobID, status, code, message); err != nil {
				return err
			}
			aborts = append(aborts, AttemptRef{NodeID: a.nodeID, AttemptID: a.id, Reason: code})
		}
		return nil
	})
	return aborts, err
}

// ExpireQueued fails queued jobs that waited too long. eligibleOnline says
// whether a capable node is connected, which separates "all nodes are busy"
// from "nothing can run this".
func (s *Store) ExpireQueued(ctx context.Context, eligibleOnline func(capability string, selector map[string]string) bool) ([]AttemptRef, error) {
	type expiredJob struct {
		id, capability string
		selector       map[string]string
	}
	var aborts []AttemptRef
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		aborts = nil
		rows, err := tx.Query(`SELECT id, capability, selector FROM jobs WHERE status=? AND queue_deadline_ms < ?`, StatusQueued, nowMS())
		if err != nil {
			return err
		}
		var expired []expiredJob
		for rows.Next() {
			var j expiredJob
			var selector string
			if err := rows.Scan(&j.id, &j.capability, &selector); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal([]byte(selector), &j.selector); err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, j)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, j := range expired {
			code, message := CodeNoNodeOnline, "no node that can run this capability was online and ready in time"
			if eligibleOnline(j.capability, j.selector) {
				code, message = CodeQueueTimeout, "capable nodes stayed busy for the whole queue timeout"
			}
			if err := finishJob(tx, j.id, StatusFailed, code, message); err != nil {
				return err
			}
			held, err := queryAttempts(tx, `WHERE a.job_id=? AND a.status=? AND j.current_attempt=a.id`, j.id, attemptActive)
			if err != nil {
				return err
			}
			for _, a := range held {
				if err := setAttemptStatus(tx, a.id, attemptFenced); err != nil {
					return err
				}
				aborts = append(aborts, AttemptRef{NodeID: a.nodeID, AttemptID: a.id, Reason: code})
			}
		}
		return nil
	})
	return aborts, err
}

// --- Scheduling ---

// QueuedJob is a job waiting for a node.
type QueuedJob struct {
	ID, Capability, Input string
	Selector              map[string]string
}

// QueuedJobs returns the oldest waiting jobs.
func (s *Store) QueuedJobs(ctx context.Context, limit int) ([]QueuedJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, capability, input, selector FROM jobs WHERE status=? ORDER BY created_ms LIMIT ?`, StatusQueued, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedJob
	for rows.Next() {
		var j QueuedJob
		var selector string
		if err := rows.Scan(&j.ID, &j.Capability, &j.Input, &selector); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(selector), &j.Selector); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// NodeLoad is what a node's current jobs are using.
type NodeLoad struct {
	Jobs      int
	Resources map[string]int
}

// NodeLoads returns current usage per node.
func (s *Store) NodeLoads(ctx context.Context) (map[string]*NodeLoad, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.node_id, COALESCE(c.requires, '{}')
		FROM attempts a
		JOIN jobs j ON j.current_attempt = a.id
		LEFT JOIN capabilities c ON c.node_id = a.node_id AND c.name = j.capability
		WHERE a.status = ? AND j.status IN (?, ?)`, attemptActive, StatusAssigned, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	loads := map[string]*NodeLoad{}
	for rows.Next() {
		var nodeID, requires string
		if err := rows.Scan(&nodeID, &requires); err != nil {
			return nil, err
		}
		load := loads[nodeID]
		if load == nil {
			load = &NodeLoad{Resources: map[string]int{}}
			loads[nodeID] = load
		}
		load.Jobs++
		var need map[string]int
		if err := json.Unmarshal([]byte(requires), &need); err != nil {
			return nil, err
		}
		for k, v := range need {
			load.Resources[k] += v
		}
	}
	return loads, rows.Err()
}

// CreateAttempt assigns a queued job to a node, fencing any previous attempt.
// It returns ok=false if the job stopped being queued in the meantime.
func (s *Store) CreateAttempt(ctx context.Context, job QueuedJob, c CapabilityRow, p RetryPolicy) (attemptID string, previous *AttemptRef, ok bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		previous, ok = nil, false
		var status string
		var current sql.NullString
		if err := tx.QueryRow(`SELECT status, current_attempt FROM jobs WHERE id=?`, job.ID).Scan(&status, &current); err != nil {
			return err
		}
		if status != StatusQueued {
			return nil
		}
		if current.Valid {
			var nodeID, attemptStatus string
			if err := tx.QueryRow(`SELECT node_id, status FROM attempts WHERE id=?`, current.String).Scan(&nodeID, &attemptStatus); err != nil {
				return err
			}
			if attemptStatus == attemptActive {
				if err := setAttemptStatus(tx, current.String, attemptFenced); err != nil {
					return err
				}
				previous = &AttemptRef{NodeID: nodeID, AttemptID: current.String, Reason: "replaced by a new attempt"}
			}
		}
		now := nowMS()
		attemptID = newID("att_")
		hardDeadline := now + (time.Duration(c.TimeoutSeconds)*time.Second + p.DeadlineGrace).Milliseconds()
		if _, err := tx.Exec(`INSERT INTO attempts (id, job_id, node_id, status, retry_if_lost, lease_expires_ms, hard_deadline_ms) VALUES (?,?,?,?,?,?,?)`,
			attemptID, job.ID, c.NodeID, attemptActive, boolInt(c.RetryIfLost), now+p.Lease.Milliseconds(), hardDeadline); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE jobs SET status=?, current_attempt=?, attempts=attempts+1, progress=NULL WHERE id=?`, StatusAssigned, attemptID, job.ID); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return attemptID, previous, ok, err
}

// --- Artifacts ---

// ErrFenced means an attempt may no longer upload results.
var ErrFenced = errors.New("attempt is no longer current")

// CanUpload reports whether a node may still transfer files for an attempt.
func (s *Store) CanUpload(ctx context.Context, attemptID, nodeID string) (bool, error) {
	var allowed int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM attempts a JOIN jobs j ON j.id = a.job_id
		WHERE a.id=? AND a.node_id=? AND a.status=? AND j.current_attempt=a.id
		  AND j.status NOT IN (?,?,?,?)`,
		attemptID, nodeID, attemptActive, StatusSucceeded, StatusFailed, StatusTimedOut, StatusCancelled).Scan(&allowed)
	return allowed > 0, err
}

// TotalStoredBytes returns the space used by stored results and input files.
func (s *Store) TotalStoredBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(size), 0) FROM artifacts) + (SELECT COALESCE(SUM(size), 0) FROM input_files)`).Scan(&total)
	return total, err
}

// PutArtifact records an uploaded file, replacing an earlier upload of the same
// name. It returns the replaced file's ID, if any, so the caller can delete it.
func (s *Store) PutArtifact(ctx context.Context, attemptID, nodeID, fileID, name, sum string, size int64, ttl time.Duration) (replaced string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		replaced = ""
		matches, err := queryAttempts(tx, `WHERE a.id=? AND a.node_id=?`, attemptID, nodeID)
		if err != nil {
			return err
		}
		if len(matches) == 0 || !matches[0].isCurrent() || matches[0].status != attemptActive || isTerminal(matches[0].jobStatus) {
			return ErrFenced
		}
		err = tx.QueryRow(`SELECT id FROM artifacts WHERE attempt_id=? AND name=?`, attemptID, name).Scan(&replaced)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if replaced != "" {
			if _, err := tx.Exec(`DELETE FROM artifacts WHERE id=?`, replaced); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO artifacts (id, attempt_id, job_id, name, sha256, size, expires_ms) VALUES (?,?,?,?,?,?,?)`,
			fileID, attemptID, matches[0].jobID, name, sum, size, nowMS()+ttl.Milliseconds())
		return err
	})
	return replaced, err
}

// ArtifactDownload is what the file endpoint needs to serve a result.
type ArtifactDownload struct {
	ID, Name, SHA256 string
}

// ArtifactFor returns a result file if the API key owns its job and the file
// belongs to the attempt that produced the job's result.
func (s *Store) ArtifactFor(ctx context.Context, fileID, apiKeyID string) (*ArtifactDownload, error) {
	var a ArtifactDownload
	err := s.db.QueryRowContext(ctx, `
		SELECT f.id, f.name, f.sha256 FROM artifacts f
		JOIN jobs j ON j.id = f.job_id AND j.current_attempt = f.attempt_id
		WHERE f.id=? AND j.api_key_id=? AND j.status=? AND f.expires_ms > ?`,
		fileID, apiKeyID, StatusSucceeded, nowMS()).Scan(&a.ID, &a.Name, &a.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// RemoveDeadArtifacts deletes rows for expired files and for attempts that lost
// out to a newer one. It returns the file IDs to remove from disk.
func (s *Store) RemoveDeadArtifacts(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		ids = nil
		rows, err := tx.Query(`
			SELECT f.id FROM artifacts f JOIN jobs j ON j.id = f.job_id
			WHERE f.expires_ms < ? OR j.current_attempt IS NOT f.attempt_id`, nowMS())
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
			if _, err := tx.Exec(`DELETE FROM artifacts WHERE id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	return ids, err
}
