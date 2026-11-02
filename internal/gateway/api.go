package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mactunnel/internal/schema"
)

const (
	maxRequestBytes     = 1 << 20
	defaultQueueTimeout = 10 * time.Minute
	maxQueueTimeout     = 24 * time.Hour
	maxLongPoll         = 60 * time.Second
	longPollInterval    = 250 * time.Millisecond
)

func (g *Gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/capabilities", g.withKey(g.handleCapabilities))
	mux.HandleFunc("POST /v1/jobs", g.withKey(g.handleCreateJob))
	mux.HandleFunc("GET /v1/jobs/{id}", g.withKey(g.handleGetJob))
	mux.HandleFunc("GET /v1/files/{id}", g.withKey(g.handleGetFile))
	mux.HandleFunc("GET /v1/worker/connect", g.handleWorkerConnect)
	mux.HandleFunc("PUT /v1/worker/attempts/{attempt}/artifacts/{name}", g.handleUploadArtifact)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(w, status, map[string]JobError{"error": {Code: code, Message: message, Retryable: retryable}})
}

func bearerToken(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return token
}

func (g *Gateway) authNode(r *http.Request) (*Node, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, nil
	}
	return g.store.NodeByToken(r.Context(), token)
}

type keyHandler func(http.ResponseWriter, *http.Request, *APIKey)

func (g *Gateway) withKey(next keyHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := bearerToken(r)
		if secret == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing API key", false)
			return
		}
		key, err := g.store.APIKeyBySecret(r.Context(), secret)
		if err != nil {
			g.internalError(w, r, "check API key", err)
			return
		}
		if key == nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid API key", false)
			return
		}
		next(w, r, key)
	}
}

func (g *Gateway) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	g.logError(r.Context(), what, err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error", true)
}

func keyAllows(key *APIKey, capability string) bool {
	for _, pattern := range key.Patterns {
		if ok, err := path.Match(pattern, capability); err == nil && ok {
			return true
		}
	}
	return false
}

// --- Agent API ---

type capabilityView struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"input_schema"`
	Nodes       []nodeOffer     `json:"nodes"`
	OnlineNodes int             `json:"online_nodes"`
}

type nodeOffer struct {
	Name   string            `json:"name"`
	Online bool              `json:"online"`
	Labels map[string]string `json:"labels,omitempty"`
}

func (g *Gateway) handleCapabilities(w http.ResponseWriter, r *http.Request, key *APIKey) {
	rows, err := g.store.CapabilityRows(r.Context(), "")
	if err != nil {
		g.internalError(w, r, "list capabilities", err)
		return
	}
	online := g.onlineNodes()
	views := []capabilityView{}
	for _, row := range rows {
		if !keyAllows(key, row.Name) {
			continue
		}
		if len(views) == 0 || views[len(views)-1].Name != row.Name {
			views = append(views, capabilityView{Name: row.Name, InputSchema: json.RawMessage(row.InputSchema), Nodes: []nodeOffer{}})
		}
		view := &views[len(views)-1]
		isOnline := online.ids[row.NodeID]
		view.Nodes = append(view.Nodes, nodeOffer{Name: row.NodeName, Online: isOnline, Labels: row.Labels})
		if isOnline {
			view.OnlineNodes++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": views})
}

type createJobRequest struct {
	Capability     string            `json:"capability"`
	Input          json.RawMessage   `json:"input"`
	IdempotencyKey string            `json:"idempotency_key"`
	QueueTimeout   string            `json:"queue_timeout"`
	Selector       map[string]string `json:"selector"`
}

func (g *Gateway) handleCreateJob(w http.ResponseWriter, r *http.Request, key *APIKey) {
	var req createJobRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body: "+err.Error(), false)
		return
	}
	if !keyAllows(key, req.Capability) {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "this API key may not call "+req.Capability, false)
		return
	}
	if len(req.IdempotencyKey) > 200 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "idempotency_key is longer than 200 characters", false)
		return
	}
	queueTimeout := defaultQueueTimeout
	if req.QueueTimeout != "" {
		parsed, err := time.ParseDuration(req.QueueTimeout)
		if err != nil || parsed <= 0 || parsed > maxQueueTimeout {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "queue_timeout must be a duration between 0 and 24h", false)
			return
		}
		queueTimeout = parsed
	}

	input := []byte(req.Input)
	if len(input) == 0 {
		input = []byte("{}")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, input); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "input is not valid JSON", false)
		return
	}
	input = compact.Bytes()

	offers, err := g.store.CapabilityRows(r.Context(), req.Capability)
	if err != nil {
		g.internalError(w, r, "look up capability", err)
		return
	}
	if len(offers) == 0 {
		writeError(w, http.StatusNotFound, "UNKNOWN_CAPABILITY", "no node has registered "+req.Capability, false)
		return
	}
	if status, code, message := checkOffers(offers, req.Selector, input); code != "" {
		writeError(w, status, code, message, false)
		return
	}

	selector, err := json.Marshal(orEmpty(req.Selector))
	if err != nil {
		g.internalError(w, r, "encode selector", err)
		return
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%s", req.Capability, input, selector)

	id, created, err := g.store.CreateJob(r.Context(), NewJob{
		APIKeyID:       key.ID,
		Capability:     req.Capability,
		Input:          string(input),
		Selector:       string(selector),
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    hex.EncodeToString(hash.Sum(nil)),
		QueueTimeout:   queueTimeout,
	})
	if errors.Is(err, ErrIdempotencyConflict) {
		writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error(), false)
		return
	}
	if err != nil {
		g.internalError(w, r, "create job", err)
		return
	}
	view, err := g.store.GetJob(r.Context(), id, key.ID, false)
	if err != nil {
		g.internalError(w, r, "read job", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
		g.wakeScheduler()
	}
	writeJSON(w, status, view)
}

// checkOffers confirms at least one node could run this request.
func checkOffers(offers []CapabilityRow, selector map[string]string, input []byte) (status int, code, message string) {
	var inputErr error
	matched := false
	for _, offer := range offers {
		if !labelsMatch(selector, offer.Labels) {
			continue
		}
		matched = true
		compiled, err := schema.Compile([]byte(offer.InputSchema))
		if err != nil {
			inputErr = err
			continue
		}
		if err := schema.Validate(compiled, input); err != nil {
			inputErr = err
			continue
		}
		return 0, "", ""
	}
	if !matched {
		return http.StatusUnprocessableEntity, "NO_MATCHING_NODE", "no node offering this capability matches the selector"
	}
	return http.StatusUnprocessableEntity, "INVALID_INPUT", inputErr.Error()
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (g *Gateway) handleGetJob(w http.ResponseWriter, r *http.Request, key *APIKey) {
	id := r.PathValue("id")
	withEvents := r.URL.Query().Get("include") == "events"
	wait := time.Duration(0)
	if raw := r.URL.Query().Get("wait"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "wait must be a whole number of seconds", false)
			return
		}
		wait = min(time.Duration(seconds)*time.Second, maxLongPoll)
	}
	deadline := time.Now().Add(wait)
	for {
		view, err := g.store.GetJob(r.Context(), id, key.ID, withEvents)
		if err != nil {
			g.internalError(w, r, "read job", err)
			return
		}
		if view == nil {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found", false)
			return
		}
		if view.Terminal || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, view)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(longPollInterval):
		}
	}
}

func (g *Gateway) handleGetFile(w http.ResponseWriter, r *http.Request, key *APIKey) {
	artifact, err := g.store.ArtifactFor(r.Context(), r.PathValue("id"), key.ID)
	if err != nil {
		g.internalError(w, r, "read artifact", err)
		return
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "file not found or expired", false)
		return
	}
	file, err := os.Open(filepath.Join(g.cfg.filesDir(), artifact.ID))
	if err != nil {
		g.internalError(w, r, "open artifact", err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		g.internalError(w, r, "stat artifact", err)
		return
	}
	// Results are untrusted bytes, so never let a browser render them.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, artifact.Name))
	w.Header().Set("ETag", `"`+artifact.SHA256+`"`)
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// --- Worker uploads ---

var artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (g *Gateway) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	node, err := g.authNode(r)
	if err != nil {
		g.internalError(w, r, "check node token", err)
		return
	}
	if node == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid node token", false)
		return
	}
	attemptID, name := r.PathValue("attempt"), r.PathValue("name")
	wantSum := r.Header.Get("X-Content-SHA256")
	if !artifactName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid artifact name", false)
		return
	}
	if !sha256Hex.MatchString(wantSum) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "X-Content-SHA256 must be a lowercase hex digest", false)
		return
	}
	allowed, err := g.store.CanUpload(r.Context(), attemptID, node.ID)
	if err != nil {
		g.internalError(w, r, "check attempt", err)
		return
	}
	if !allowed {
		writeError(w, http.StatusGone, "ATTEMPT_FENCED", "this attempt can no longer upload", false)
		return
	}

	used, err := g.store.TotalArtifactBytes(r.Context())
	if err != nil {
		g.internalError(w, r, "measure storage", err)
		return
	}
	remaining := g.cfg.MaxArtifactBytes - used
	if r.ContentLength > remaining {
		writeError(w, http.StatusInsufficientStorage, "STORAGE_FULL", "artifact storage is full", false)
		return
	}

	size, gotSum, tmpPath, err := g.receiveFile(r.Context(), http.MaxBytesReader(w, r.Body, remaining))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusInsufficientStorage, "STORAGE_FULL", "artifact storage is full", false)
			return
		}
		// The usual cause is the network dropping mid-upload; the worker retries.
		writeError(w, http.StatusBadRequest, "UPLOAD_INTERRUPTED", "could not receive the upload", true)
		return
	}
	defer os.Remove(tmpPath) // A no-op once the file has been renamed.
	if gotSum != wantSum {
		writeError(w, http.StatusUnprocessableEntity, "CHECKSUM_MISMATCH", "upload does not match X-Content-SHA256", true)
		return
	}

	fileID := newID("file_")
	if err := os.Rename(tmpPath, filepath.Join(g.cfg.filesDir(), fileID)); err != nil {
		g.internalError(w, r, "store artifact", err)
		return
	}
	replaced, err := g.store.PutArtifact(r.Context(), attemptID, node.ID, fileID, name, gotSum, size, g.cfg.ArtifactTTL)
	if err != nil {
		os.Remove(filepath.Join(g.cfg.filesDir(), fileID))
		if errors.Is(err, ErrFenced) {
			writeError(w, http.StatusGone, "ATTEMPT_FENCED", "this attempt can no longer upload", false)
			return
		}
		g.internalError(w, r, "record artifact", err)
		return
	}
	if replaced != "" {
		os.Remove(filepath.Join(g.cfg.filesDir(), replaced))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": fileID, "size": size, "sha256": gotSum})
}

// receiveFile streams a body to a synced temporary file and hashes it.
func (g *Gateway) receiveFile(ctx context.Context, body io.Reader) (size int64, sum, tmpPath string, err error) {
	tmp, err := os.CreateTemp(g.cfg.tmpDir(), "upload-*")
	if err != nil {
		return 0, "", "", err
	}
	defer tmp.Close()
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	hash := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, hash), body)
	if err != nil {
		return 0, "", "", err
	}
	if err := ctx.Err(); err != nil {
		return 0, "", "", err
	}
	if err := tmp.Sync(); err != nil {
		return 0, "", "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), tmp.Name(), nil
}
