package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type testCapabilityRequest struct {
	Node           string `json:"node"`
	QueueTimeout   string `json:"queue_timeout"`
	IdempotencyKey string `json:"idempotency_key"`
}

type startedTest struct {
	Node string   `json:"node"`
	Job  *JobView `json:"job"`
}

type skippedTest struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// handleTestCapability runs a capability's test on each machine that's connected,
// ready and declares one. What a test runs comes only from the worker, so this lets
// a caller trigger nothing beyond what the worker offered as safe to repeat. It's an
// ordinary job, so it's followed and cancelled like any other.
func (g *Gateway) handleTestCapability(w http.ResponseWriter, r *http.Request, key *APIKey) {
	capability := r.PathValue("name")
	var req testCapabilityRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body: "+err.Error(), false)
		return
	}
	if !keyAllows(key, capability) {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "this API key may not call "+capability, false)
		return
	}
	if len(req.IdempotencyKey) > 100 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "idempotency_key is longer than 100 characters", false)
		return
	}
	queueTimeout, err := parseQueueTimeout(req.QueueTimeout)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), false)
		return
	}
	offers, err := g.store.CapabilityRows(r.Context(), capability)
	if err != nil {
		g.internalError(w, r, "look up capability", err)
		return
	}
	if len(offers) == 0 {
		writeError(w, http.StatusNotFound, "UNKNOWN_CAPABILITY", "no node has registered "+capability, false)
		return
	}
	if req.Node != "" {
		offers = onNode(offers, req.Node)
		if len(offers) == 0 {
			writeError(w, http.StatusNotFound, "UNKNOWN_NODE", fmt.Sprintf("no node named %s offers %s", req.Node, capability), false)
			return
		}
	}

	online := g.onlineNodes()
	started := []startedTest{}
	skipped := []skippedTest{}
	anyCreated, couldBeReady := false, false
	for _, offer := range offers {
		reason, waitable := testBlocked(offer, online)
		if reason != "" {
			skipped = append(skipped, skippedTest{Node: offer.NodeName, Reason: reason})
			couldBeReady = couldBeReady || waitable
			continue
		}
		view, created, err := g.startTest(r.Context(), key, offer, req.IdempotencyKey, queueTimeout)
		if errors.Is(err, ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error(), false)
			return
		}
		if err != nil {
			g.internalError(w, r, "start test", err)
			return
		}
		anyCreated = anyCreated || created
		started = append(started, startedTest{Node: offer.NodeName, Job: view})
	}

	if len(started) == 0 {
		reasons := make([]string, len(skipped))
		for i, skip := range skipped {
			reasons[i] = skip.Node + ": " + skip.Reason
		}
		message := strings.Join(reasons, "; ")
		if couldBeReady {
			writeError(w, http.StatusConflict, "NOT_READY", message, true)
		} else {
			writeError(w, http.StatusUnprocessableEntity, "NO_TEST", message, false)
		}
		return
	}
	status := http.StatusOK
	if anyCreated {
		status = http.StatusAccepted
		g.wakeScheduler()
	}
	writeJSON(w, status, map[string]any{"tests": started, "skipped": skipped})
}

func onNode(offers []CapabilityRow, name string) []CapabilityRow {
	var out []CapabilityRow
	for _, offer := range offers {
		if offer.NodeName == name {
			out = append(out, offer)
		}
	}
	return out
}

// testBlocked says why this offer can't be tested now, and whether that could change
// without anyone editing the worker's manifest.
func testBlocked(offer CapabilityRow, online onlineSet) (reason string, waitable bool) {
	switch {
	case offer.TestInput == "":
		return "it doesn't declare a test", false
	case !online.ids[offer.NodeID]:
		return "it isn't connected", true
	case offer.NotReady != "":
		return "it isn't ready: " + offer.NotReady, true
	}
	// The worker checked this too, but the gateway never trusts that for a job.
	if _, _, message := checkOffers([]CapabilityRow{offer}, nil, []byte(offer.TestInput), nil); message != "" {
		return "its test can't run: " + message, false
	}
	return "", false
}

func (g *Gateway) startTest(ctx context.Context, key *APIKey, offer CapabilityRow, idempotencyKey string, queueTimeout time.Duration) (*JobView, bool, error) {
	var input bytes.Buffer
	if err := json.Compact(&input, []byte(offer.TestInput)); err != nil {
		return nil, false, fmt.Errorf("stored test input: %w", err)
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "test\x00%s\x00%s\x00%s", offer.Name, offer.NodeID, input.Bytes())
	if idempotencyKey != "" {
		// A single request can start one job on each machine.
		idempotencyKey += "/" + offer.NodeID
	}
	id, created, err := g.store.CreateJob(ctx, NewJob{
		APIKeyID:       key.ID,
		Capability:     offer.Name,
		Input:          input.String(),
		Selector:       "{}",
		IdempotencyKey: idempotencyKey,
		RequestHash:    hex.EncodeToString(hash.Sum(nil)),
		QueueTimeout:   queueTimeout,
		TestNode:       offer.NodeID,
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		g.log.Info("capability test started", "job", id, "capability", offer.Name, "node", offer.NodeName)
	}
	view, err := g.store.GetJob(ctx, id, key.ID, false)
	return view, created, err
}

// LastTest is the outcome of the latest test of a capability on one machine.
type LastTest struct {
	JobID     string `json:"job_id"`
	Status    string `json:"status"`
	Finished  bool   `json:"finished"`
	ErrorCode string `json:"error_code,omitempty"`
	At        string `json:"at"`
}

type testKey struct{ nodeID, capability string }

// LastTests returns the latest test per machine and capability. SQLite takes the
// other columns from the row with the highest created_ms.
func (s *Store) LastTests(ctx context.Context) (map[testKey]*LastTest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT test_node, capability, id, status, error_code, MAX(created_ms), finished_ms
		FROM jobs WHERE test_node IS NOT NULL GROUP BY test_node, capability`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[testKey]*LastTest{}
	for rows.Next() {
		var key testKey
		var test LastTest
		var code sql.NullString
		var created int64
		var finished sql.NullInt64
		if err := rows.Scan(&key.nodeID, &key.capability, &test.JobID, &test.Status, &code, &created, &finished); err != nil {
			return nil, err
		}
		test.ErrorCode = code.String
		test.Finished = isTerminal(test.Status)
		test.At = formatMS(created)
		if finished.Valid {
			test.At = formatMS(finished.Int64)
		}
		out[key] = &test
	}
	return out, rows.Err()
}
