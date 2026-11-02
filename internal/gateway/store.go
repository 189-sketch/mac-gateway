package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // SQLite driver

	"mactunnel/internal/protocol"
)

// Job statuses. "lost" is not terminal: the worker may still come back.
const (
	StatusQueued    = "queued"
	StatusAssigned  = "assigned"
	StatusRunning   = "running"
	StatusLost      = "lost"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
)

// Attempt statuses.
const (
	attemptActive   = "active"   // The worker may be running it.
	attemptFinished = "finished" // The worker reported a final event.
	attemptFenced   = "fenced"   // Replaced or abandoned; results are rejected.
)

// Gateway-generated error codes.
const (
	CodeNodeLost      = "NODE_LOST"
	CodeNoNodeOnline  = "NO_NODE_ONLINE"
	CodeQueueTimeout  = "QUEUE_TIMEOUT"
	CodeDeadline      = "DEADLINE_EXCEEDED"
	CodeArtifactError = "ARTIFACT_MISMATCH"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS nodes (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL UNIQUE,
	labels TEXT NOT NULL DEFAULT '{}',
	resources TEXT NOT NULL DEFAULT '{}',
	max_jobs INTEGER NOT NULL DEFAULT 1,
	os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	last_seen_ms INTEGER,
	revoked_ms INTEGER
);
CREATE TABLE IF NOT EXISTS capabilities (
	node_id TEXT NOT NULL REFERENCES nodes(id),
	name TEXT NOT NULL,
	input_schema TEXT NOT NULL,
	requires TEXT NOT NULL DEFAULT '{}',
	timeout_seconds INTEGER NOT NULL,
	retry_if_lost INTEGER NOT NULL,
	PRIMARY KEY (node_id, name)
);
CREATE TABLE IF NOT EXISTS api_keys (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	key_hash TEXT NOT NULL UNIQUE,
	capabilities TEXT NOT NULL,
	revoked_ms INTEGER
);
CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY,
	api_key_id TEXT NOT NULL REFERENCES api_keys(id),
	capability TEXT NOT NULL,
	input TEXT NOT NULL,
	selector TEXT NOT NULL DEFAULT '{}',
	idempotency_key TEXT,
	request_hash TEXT NOT NULL,
	status TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	current_attempt TEXT,
	progress TEXT,
	error_code TEXT,
	error_message TEXT,
	queue_timeout_ms INTEGER NOT NULL,
	queue_deadline_ms INTEGER NOT NULL,
	lost_deadline_ms INTEGER,
	created_ms INTEGER NOT NULL,
	started_ms INTEGER,
	finished_ms INTEGER,
	UNIQUE (api_key_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status, created_ms);
CREATE TABLE IF NOT EXISTS attempts (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL REFERENCES jobs(id),
	node_id TEXT NOT NULL REFERENCES nodes(id),
	status TEXT NOT NULL,
	last_seq INTEGER NOT NULL DEFAULT 0,
	retry_if_lost INTEGER NOT NULL,
	lease_expires_ms INTEGER NOT NULL,
	hard_deadline_ms INTEGER NOT NULL,
	started_ms INTEGER
);
CREATE INDEX IF NOT EXISTS attempts_node ON attempts(node_id, status);
CREATE TABLE IF NOT EXISTS job_events (
	attempt_id TEXT NOT NULL REFERENCES attempts(id),
	seq INTEGER NOT NULL,
	job_id TEXT NOT NULL REFERENCES jobs(id),
	type TEXT NOT NULL,
	payload TEXT,
	created_ms INTEGER NOT NULL,
	PRIMARY KEY (attempt_id, seq)
);
CREATE INDEX IF NOT EXISTS job_events_job ON job_events(job_id, created_ms);
CREATE TABLE IF NOT EXISTS artifacts (
	id TEXT PRIMARY KEY,
	attempt_id TEXT NOT NULL REFERENCES attempts(id),
	job_id TEXT NOT NULL REFERENCES jobs(id),
	name TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	size INTEGER NOT NULL,
	expires_ms INTEGER NOT NULL,
	UNIQUE (attempt_id, name)
);
CREATE INDEX IF NOT EXISTS artifacts_expiry ON artifacts(expires_ms);
`

// Store is the gateway's SQLite persistence layer. It uses a single database
// connection, so every transaction is serialized. Callers must never start a
// query while iterating rows or inside another transaction's scope.
type Store struct {
	db *sql.DB
}

// OpenStore opens or creates the database in dataDir.
func OpenStore(dataDir string) (*Store, error) {
	dsn := url.Values{}
	dsn.Set("_txlock", "immediate")
	dsn.Add("_pragma", "journal_mode(WAL)")
	dsn.Add("_pragma", "synchronous(FULL)")
	dsn.Add("_pragma", "busy_timeout(10000)")
	dsn.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "gateway.db")+"?"+dsn.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable.
	}
	return prefix + hex.EncodeToString(b)
}

// newSecret returns a 256-bit credential and the hash stored in the database.
func newSecret(prefix string) (secret, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	secret = prefix + hex.EncodeToString(b)
	return secret, hashSecret(secret)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func nowMS() int64 { return time.Now().UnixMilli() }

// --- Credentials ---

// AddNode registers a node and returns its token, which is shown only once.
func (s *Store) AddNode(ctx context.Context, name string) (string, error) {
	secret, hash := newSecret("fn_")
	_, err := s.db.ExecContext(ctx, `INSERT INTO nodes (id, name, token_hash) VALUES (?, ?, ?)`, newID("node_"), name, hash)
	if err != nil {
		return "", fmt.Errorf("add node %q: %w", name, err)
	}
	return secret, nil
}

// RevokeNode invalidates a node's token.
func (s *Store) RevokeNode(ctx context.Context, name string) error {
	return requireOneRow(s.db.ExecContext(ctx, `UPDATE nodes SET revoked_ms = ? WHERE name = ? AND revoked_ms IS NULL`, nowMS(), name))
}

// AddAPIKey creates an agent key limited to capability glob patterns.
func (s *Store) AddAPIKey(ctx context.Context, name string, patterns []string) (string, error) {
	secret, hash := newSecret("fk_")
	encoded, err := json.Marshal(patterns)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, capabilities) VALUES (?, ?, ?, ?)`, newID("key_"), name, hash, string(encoded))
	if err != nil {
		return "", fmt.Errorf("add key %q: %w", name, err)
	}
	return secret, nil
}

// RevokeAPIKey invalidates an agent key.
func (s *Store) RevokeAPIKey(ctx context.Context, name string) error {
	return requireOneRow(s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_ms = ? WHERE name = ? AND revoked_ms IS NULL`, nowMS(), name))
}

func requireOneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("no matching active credential")
	}
	return nil
}

// Node is an authenticated worker identity.
type Node struct {
	ID   string
	Name string
}

// NodeByToken returns the active node owning a token.
func (s *Store) NodeByToken(ctx context.Context, token string) (*Node, error) {
	var n Node
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM nodes WHERE token_hash = ? AND revoked_ms IS NULL`, hashSecret(token)).Scan(&n.ID, &n.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &n, err
}

// APIKey is an authenticated agent identity.
type APIKey struct {
	ID       string
	Patterns []string
}

// APIKeyBySecret returns the active key matching a secret.
func (s *Store) APIKeyBySecret(ctx context.Context, secret string) (*APIKey, error) {
	var k APIKey
	var patterns string
	err := s.db.QueryRowContext(ctx, `SELECT id, capabilities FROM api_keys WHERE key_hash = ? AND revoked_ms IS NULL`, hashSecret(secret)).Scan(&k.ID, &patterns)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(patterns), &k.Patterns); err != nil {
		return nil, err
	}
	return &k, nil
}

// --- Capabilities ---

// RegisterNode stores what a node advertised in its hello message.
func (s *Store) RegisterNode(ctx context.Context, nodeID string, h protocol.Hello) error {
	labels, err := json.Marshal(h.Labels)
	if err != nil {
		return err
	}
	resources, err := json.Marshal(h.Resources)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE nodes SET labels=?, resources=?, max_jobs=?, os=?, arch=?, last_seen_ms=? WHERE id=?`,
			string(labels), string(resources), h.MaxJobs, h.OS, h.Arch, nowMS(), nodeID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM capabilities WHERE node_id=?`, nodeID); err != nil {
			return err
		}
		for _, c := range h.Capabilities {
			requires, err := json.Marshal(c.Requires)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO capabilities (node_id, name, input_schema, requires, timeout_seconds, retry_if_lost) VALUES (?,?,?,?,?,?)`,
				nodeID, c.Name, string(c.InputSchema), string(requires), c.TimeoutSeconds, boolInt(c.RetryIfLost)); err != nil {
				return err
			}
		}
		return nil
	})
}

// CapabilityRow is one node's offer of a capability.
type CapabilityRow struct {
	NodeID         string
	NodeName       string
	Name           string
	InputSchema    string
	Requires       map[string]int
	TimeoutSeconds int64
	RetryIfLost    bool
	Labels         map[string]string
	Resources      map[string]int
	MaxJobs        int
}

// CapabilityRows lists capabilities for active nodes. An empty name lists all.
func (s *Store) CapabilityRows(ctx context.Context, name string) ([]CapabilityRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.node_id, n.name, c.name, c.input_schema, c.requires, c.timeout_seconds, c.retry_if_lost, n.labels, n.resources, n.max_jobs
		FROM capabilities c JOIN nodes n ON n.id = c.node_id
		WHERE n.revoked_ms IS NULL AND (? = '' OR c.name = ?)
		ORDER BY c.name, n.name`, name, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CapabilityRow
	for rows.Next() {
		var c CapabilityRow
		var requires, labels, resources string
		var retry int
		if err := rows.Scan(&c.NodeID, &c.NodeName, &c.Name, &c.InputSchema, &requires, &c.TimeoutSeconds, &retry, &labels, &resources, &c.MaxJobs); err != nil {
			return nil, err
		}
		c.RetryIfLost = retry != 0
		for _, field := range []struct {
			raw string
			dst any
		}{{requires, &c.Requires}, {labels, &c.Labels}, {resources, &c.Resources}} {
			if err := json.Unmarshal([]byte(field.raw), field.dst); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- Jobs ---

// NewJob is a validated job request.
type NewJob struct {
	APIKeyID       string
	Capability     string
	Input          string
	Selector       string
	IdempotencyKey string
	RequestHash    string
	QueueTimeout   time.Duration
}

// ErrIdempotencyConflict means a key was reused for a different request.
var ErrIdempotencyConflict = errors.New("idempotency key was used with a different request")

// CreateJob inserts a job. If the idempotency key was already used for an
// identical request, it returns the existing job ID with created=false.
func (s *Store) CreateJob(ctx context.Context, j NewJob) (id string, created bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if j.IdempotencyKey != "" {
			var existingID, existingHash string
			err := tx.QueryRow(`SELECT id, request_hash FROM jobs WHERE api_key_id=? AND idempotency_key=?`, j.APIKeyID, j.IdempotencyKey).Scan(&existingID, &existingHash)
			if err == nil {
				if existingHash != j.RequestHash {
					return ErrIdempotencyConflict
				}
				id = existingID
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		now := nowMS()
		id = newID("job_")
		created = true
		var key any
		if j.IdempotencyKey != "" {
			key = j.IdempotencyKey
		}
		_, err := tx.Exec(`INSERT INTO jobs (id, api_key_id, capability, input, selector, idempotency_key, request_hash, status, queue_timeout_ms, queue_deadline_ms, created_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, j.APIKeyID, j.Capability, j.Input, j.Selector, key, j.RequestHash, StatusQueued,
			j.QueueTimeout.Milliseconds(), now+j.QueueTimeout.Milliseconds(), now)
		return err
	})
	return id, created, err
}

// JobError is the agent-facing error shape.
type JobError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// ArtifactView is the agent-facing description of a result file.
type ArtifactView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// EventView is the agent-facing description of a job event.
type EventView struct {
	Attempt string          `json:"attempt_id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	At      string          `json:"at"`
}

// JobView is the agent-facing job representation.
type JobView struct {
	ID         string          `json:"id"`
	Capability string          `json:"capability"`
	Status     string          `json:"status"`
	Terminal   bool            `json:"terminal"`
	Attempts   int             `json:"attempts"`
	Node       string          `json:"node,omitempty"`
	Progress   json.RawMessage `json:"progress,omitempty"`
	Error      *JobError       `json:"error,omitempty"`
	Artifacts  []ArtifactView  `json:"artifacts"`
	Events     []EventView     `json:"events,omitempty"`
	CreatedAt  string          `json:"created_at"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
}

func isTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusTimedOut
}

func formatMS(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// GetJob returns a job if it belongs to the API key, or nil otherwise.
func (s *Store) GetJob(ctx context.Context, jobID, apiKeyID string, withEvents bool) (*JobView, error) {
	var v JobView
	var progress, node, errCode, errMsg sql.NullString
	var created int64
	var started, finished sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT j.id, j.capability, j.status, j.attempts, n.name, j.progress, j.error_code, j.error_message, j.created_ms, j.started_ms, j.finished_ms
		FROM jobs j
		LEFT JOIN attempts a ON a.id = j.current_attempt
		LEFT JOIN nodes n ON n.id = a.node_id
		WHERE j.id = ? AND j.api_key_id = ?`, jobID, apiKeyID).
		Scan(&v.ID, &v.Capability, &v.Status, &v.Attempts, &node, &progress, &errCode, &errMsg, &created, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.Terminal = isTerminal(v.Status)
	v.Node = node.String
	if progress.Valid {
		v.Progress = json.RawMessage(progress.String)
	}
	if errCode.Valid {
		v.Error = &JobError{Code: errCode.String, Message: errMsg.String, Retryable: isRetryable(errCode.String)}
	}
	v.CreatedAt = formatMS(created)
	if started.Valid {
		v.StartedAt = formatMS(started.Int64)
	}
	if finished.Valid {
		v.FinishedAt = formatMS(finished.Int64)
	}

	v.Artifacts = []ArtifactView{}
	if v.Status == StatusSucceeded {
		if v.Artifacts, err = s.jobArtifacts(ctx, jobID); err != nil {
			return nil, err
		}
	}
	if withEvents {
		if v.Events, err = s.jobEvents(ctx, jobID); err != nil {
			return nil, err
		}
	}
	return &v, nil
}

func (s *Store) jobArtifacts(ctx context.Context, jobID string) ([]ArtifactView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.name, f.size, f.sha256, f.expires_ms
		FROM artifacts f JOIN jobs j ON j.id = f.job_id AND j.current_attempt = f.attempt_id
		WHERE f.job_id = ? ORDER BY f.name`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArtifactView{}
	for rows.Next() {
		var a ArtifactView
		var expires int64
		if err := rows.Scan(&a.ID, &a.Name, &a.Size, &a.SHA256, &expires); err != nil {
			return nil, err
		}
		a.URL = "/v1/files/" + a.ID
		a.ExpiresAt = formatMS(expires)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) jobEvents(ctx context.Context, jobID string) ([]EventView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT attempt_id, seq, type, payload, created_ms FROM job_events WHERE job_id=? ORDER BY created_ms, attempt_id, seq`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventView
	for rows.Next() {
		var e EventView
		var payload sql.NullString
		var at int64
		if err := rows.Scan(&e.Attempt, &e.Seq, &e.Type, &payload, &at); err != nil {
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

// isRetryable says whether an agent can reasonably resubmit after this error.
func isRetryable(code string) bool {
	switch code {
	case CodeNodeLost, CodeNoNodeOnline, CodeQueueTimeout, CodeDeadline, protocol.CodeWorkerRestarted, protocol.CodeUploadFailed:
		return true
	}
	return false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
