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
	"strings"
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
	StatusCancelled = "cancelled"
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
CREATE INDEX IF NOT EXISTS jobs_created ON jobs(created_ms, id);
CREATE INDEX IF NOT EXISTS jobs_finished ON jobs(finished_ms);
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
CREATE TABLE IF NOT EXISTS input_files (
	id TEXT PRIMARY KEY,
	api_key_id TEXT NOT NULL REFERENCES api_keys(id),
	name TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	size INTEGER NOT NULL,
	expires_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS input_files_expiry ON input_files(expires_ms);
CREATE TABLE IF NOT EXISTS job_files (
	job_id TEXT NOT NULL REFERENCES jobs(id),
	field TEXT NOT NULL,
	file_id TEXT NOT NULL,
	PRIMARY KEY (job_id, field)
);
CREATE INDEX IF NOT EXISTS job_files_file ON job_files(file_id);
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
	if _, err := db.Exec(schemaSQL + pairingSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := ensureColumn(db, "capabilities", "input_files", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade schema: %w", err)
	}
	if err := ensureColumn(db, "capabilities", "not_ready", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade schema: %w", err)
	}
	if err := ensureColumn(db, "api_keys", "dashboard", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade schema: %w", err)
	}
	if err := ensureColumn(db, "capabilities", "test_input", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade schema: %w", err)
	}
	if err := ensureColumn(db, "capabilities", "description", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade schema: %w", err)
	}
	if err := ensureColumn(db, "jobs", "test_node", "TEXT"); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade schema: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS jobs_test ON jobs(test_node, capability, created_ms) WHERE test_node IS NOT NULL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade schema: %w", err)
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

// AddDashboardKey creates a key that can view the dashboard and do nothing else.
func (s *Store) AddDashboardKey(ctx context.Context, name string) (string, error) {
	secret, hash := newSecret("fd_")
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, capabilities, dashboard) VALUES (?, ?, ?, '[]', 1)`, newID("key_"), name, hash)
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
	ID        string
	Patterns  []string
	Dashboard bool // Can view the dashboard, and can't use the agent API.
	// Node is set on the short-lived key a machine uses to test its own connection.
	// Such a key sees and runs only that machine's tools.
	Node string
}

// APIKeyBySecret returns the active key matching a secret.
func (s *Store) APIKeyBySecret(ctx context.Context, secret string) (*APIKey, error) {
	var k APIKey
	var patterns string
	err := s.db.QueryRowContext(ctx, `SELECT id, capabilities, dashboard FROM api_keys WHERE key_hash = ? AND revoked_ms IS NULL`, hashSecret(secret)).Scan(&k.ID, &patterns, &k.Dashboard)
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
			inputFiles, err := json.Marshal(orEmptySlice(c.InputFiles))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO capabilities (node_id, name, input_schema, requires, timeout_seconds, retry_if_lost, input_files, not_ready, test_input, description) VALUES (?,?,?,?,?,?,?,?,?,?)`,
				nodeID, c.Name, string(c.InputSchema), string(requires), c.TimeoutSeconds, boolInt(c.RetryIfLost), string(inputFiles), cleanReason(c.NotReady), string(c.TestInput), cleanDescription(c.Description)); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetCapabilityStatus records which of a node's capabilities passed their latest
// checks. Names the node never declared are ignored.
func (s *Store) SetCapabilityStatus(ctx context.Context, nodeID string, notReady map[string]string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE capabilities SET not_ready='' WHERE node_id=?`, nodeID); err != nil {
			return err
		}
		for name, reason := range notReady {
			if _, err := tx.Exec(`UPDATE capabilities SET not_ready=? WHERE node_id=? AND name=?`, cleanReason(reason), nodeID, name); err != nil {
				return err
			}
		}
		return nil
	})
}

// cleanDescription bounds a tool's description, which agents read to pick a tool.
func cleanDescription(text string) string {
	return strings.ToValidUTF8(truncate(strings.TrimSpace(text), 2000), "")
}

// cleanReason bounds what a worker can make the gateway store and show.
func cleanReason(reason string) string {
	return strings.ToValidUTF8(truncate(reason, 300), "")
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
	InputFiles     []string
	NotReady       string // Why the node's own check says this can't run. Empty means ready.
	TestInput      string // What the node says to run to test this. Empty means it can't be tested.
	Description    string
}

// CapabilityRows lists capabilities for active nodes. An empty name lists all.
func (s *Store) CapabilityRows(ctx context.Context, name string) ([]CapabilityRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.node_id, n.name, c.name, c.input_schema, c.requires, c.timeout_seconds, c.retry_if_lost, n.labels, n.resources, n.max_jobs, c.input_files, c.not_ready, c.test_input, c.description
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
		var requires, labels, resources, inputFiles string
		var retry int
		if err := rows.Scan(&c.NodeID, &c.NodeName, &c.Name, &c.InputSchema, &requires, &c.TimeoutSeconds, &retry, &labels, &resources, &c.MaxJobs, &inputFiles, &c.NotReady, &c.TestInput, &c.Description); err != nil {
			return nil, err
		}
		c.RetryIfLost = retry != 0
		for _, field := range []struct {
			raw string
			dst any
		}{{requires, &c.Requires}, {labels, &c.Labels}, {resources, &c.Resources}, {inputFiles, &c.InputFiles}} {
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
	Files          map[string]string // Input field name to file ID.
	TestNode       string            // For a test, the only node that may run it.
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
		var testNode any
		if j.TestNode != "" {
			testNode = j.TestNode
		}
		_, err := tx.Exec(`INSERT INTO jobs (id, api_key_id, capability, input, selector, idempotency_key, request_hash, status, queue_timeout_ms, queue_deadline_ms, created_ms, test_node)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, j.APIKeyID, j.Capability, j.Input, j.Selector, key, j.RequestHash, StatusQueued,
			j.QueueTimeout.Milliseconds(), now+j.QueueTimeout.Milliseconds(), now, testNode)
		if err != nil {
			return err
		}
		return checkAndAttachFiles(tx, j, id)
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
	ID      int64           `json:"id"`
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
	Test       bool            `json:"test,omitempty"`
	Attempts   int             `json:"attempts"`
	Node       string          `json:"node,omitempty"`
	Progress   json.RawMessage `json:"progress,omitempty"`
	Error      *JobError       `json:"error,omitempty"`
	Artifacts  []ArtifactView  `json:"artifacts"`
	Result     json.RawMessage `json:"result,omitempty"`
	// ExitCode and Output are a command's exit code and the end of what it
	// printed, whether the job succeeded or failed.
	ExitCode   *int        `json:"exit_code,omitempty"`
	Output     string      `json:"output,omitempty"`
	Events     []EventView `json:"events,omitempty"`
	CreatedAt  string      `json:"created_at"`
	StartedAt  string      `json:"started_at,omitempty"`
	FinishedAt string      `json:"finished_at,omitempty"`
}

func isTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusTimedOut || status == StatusCancelled
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
		SELECT j.id, j.capability, j.status, j.attempts, n.name, j.progress, j.error_code, j.error_message, j.created_ms, j.started_ms, j.finished_ms, j.test_node IS NOT NULL
		FROM jobs j
		LEFT JOIN attempts a ON a.id = j.current_attempt
		LEFT JOIN nodes n ON n.id = a.node_id
		WHERE j.id = ? AND j.api_key_id = ?`, jobID, apiKeyID).
		Scan(&v.ID, &v.Capability, &v.Status, &v.Attempts, &node, &progress, &errCode, &errMsg, &created, &started, &finished, &v.Test)
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
	if v.Terminal {
		if err := s.jobOutcome(ctx, &v); err != nil {
			return nil, err
		}
	}
	if withEvents {
		if v.Events, err = s.JobEventsAfter(ctx, jobID, 0); err != nil {
			return nil, err
		}
	}
	return &v, nil
}

// jobOutcome reads the result, exit code and output from the event that ended
// the job's current attempt. A job that failed on the gateway, such as one that
// timed out in the queue, has no such event.
func (s *Store) jobOutcome(ctx context.Context, v *JobView) error {
	var eventType string
	var payload sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT e.type, e.payload FROM job_events e JOIN jobs j ON j.current_attempt = e.attempt_id
		WHERE e.job_id = ? AND e.type IN (?, ?)`, v.ID, protocol.EventCompleted, protocol.EventFailed).Scan(&eventType, &payload)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !payload.Valid) {
		return nil
	}
	if err != nil {
		return err
	}
	if eventType == protocol.EventCompleted {
		var done protocol.Completion
		if json.Unmarshal([]byte(payload.String), &done) == nil && v.Status == StatusSucceeded {
			v.Result, v.ExitCode, v.Output = done.Result, done.ExitCode, done.Output
		}
		return nil
	}
	var failed protocol.Failure
	if json.Unmarshal([]byte(payload.String), &failed) == nil {
		v.ExitCode, v.Output = failed.ExitCode, failed.Output
	}
	return nil
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

// isRetryable says whether an agent can reasonably resubmit after this error.
func isRetryable(code string) bool {
	switch code {
	case CodeNodeLost, CodeNoNodeOnline, CodeQueueTimeout, CodeDeadline, protocol.CodeWorkerRestarted, protocol.CodeUploadFailed, protocol.CodeDownloadFailed, protocol.CodeServiceUnavailable:
		return true
	}
	return false
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
