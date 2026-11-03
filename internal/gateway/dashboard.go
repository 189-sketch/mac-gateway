package gateway

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mactunnel/internal/protocol"
)

const (
	defaultDashboardJobs = 50
	maxDashboardJobs     = 200
	dashboardRecentSpan  = 24 * time.Hour
)

//go:embed web
var dashboardFiles embed.FS

// The page makes no use of inline code or other origins, so a worker or service
// that manages to get markup into a job's error message has nothing to run.
const dashboardCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func dashboardHandler() http.Handler {
	files, err := fs.Sub(dashboardFiles, "web")
	if err != nil {
		panic(err) // The embedded directory is checked at build time.
	}
	server := http.StripPrefix("/dashboard/", http.FileServerFS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", dashboardCSP)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-cache")
		server.ServeHTTP(w, r)
	})
}

// withDashboardKey admits only keys made for the dashboard. They're a separate
// kind of key, and withKey refuses them: one can see every job's status, but it
// can't run anything or read anyone's input or results.
func (g *Gateway) withDashboardKey(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		secret := bearerToken(r)
		if secret == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing dashboard key", false)
			return
		}
		key, err := g.store.APIKeyBySecret(r.Context(), secret)
		if err != nil {
			g.internalError(w, r, "check dashboard key", err)
			return
		}
		if key == nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid dashboard key", false)
			return
		}
		if !key.Dashboard {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "this is an agent key; create a dashboard key with: fleet-gateway key add NAME --dashboard", false)
			return
		}
		next(w, r)
	}
}

// --- Views ---

// DashboardNode is one machine as an operator sees it.
type DashboardNode struct {
	Name         string                 `json:"name"`
	Online       bool                   `json:"online"`
	OS           string                 `json:"os,omitempty"`
	Arch         string                 `json:"arch,omitempty"`
	Labels       map[string]string      `json:"labels,omitempty"`
	Jobs         ResourceUse            `json:"jobs"`
	Resources    map[string]ResourceUse `json:"resources"`
	Capabilities []string               `json:"capabilities"`
	// NotReady lists capabilities that failed the worker's own check, with why.
	NotReady   map[string]string `json:"not_ready,omitempty"`
	LastSeenAt string            `json:"last_seen_at,omitempty"`
}

// ResourceUse is how much of something is taken.
type ResourceUse struct {
	Used  int `json:"used"`
	Total int `json:"total"`
}

// DashboardOverview is the summary at the top of the dashboard.
type DashboardOverview struct {
	GeneratedAt  string          `json:"generated_at"`
	Nodes        []DashboardNode `json:"nodes"`
	Active       map[string]int  `json:"active"`
	Recent       map[string]int  `json:"recent"`
	Capabilities []string        `json:"capabilities"`
	Storage      ResourceUse64   `json:"storage"`
}

// ResourceUse64 is ResourceUse for sizes in bytes.
type ResourceUse64 struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// DashboardJob is what an operator may see of a job. It's its own type, rather
// than a JobView with fields left empty, so that input and results can't appear
// here by accident: there's nowhere to put them.
type DashboardJob struct {
	ID          string          `json:"id"`
	Capability  string          `json:"capability"`
	Status      string          `json:"status"`
	Terminal    bool            `json:"terminal"`
	Attempts    int             `json:"attempts"`
	Node        string          `json:"node,omitempty"`
	Progress    json.RawMessage `json:"progress,omitempty"`
	Error       *JobError       `json:"error,omitempty"`
	SubmittedBy string          `json:"submitted_by"`
	CreatedAt   string          `json:"created_at"`
	StartedAt   string          `json:"started_at,omitempty"`
	FinishedAt  string          `json:"finished_at,omitempty"`

	createdMS int64
}

// DashboardJobDetail adds a job's history to the list view.
type DashboardJobDetail struct {
	DashboardJob
	Artifacts []DashboardArtifact `json:"artifacts"`
	Events    []EventView         `json:"events"`
}

// DashboardArtifact names a result file without offering it for download.
type DashboardArtifact struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	ExpiresAt string `json:"expires_at"`
}

// --- Handlers ---

func (g *Gateway) handleDashboardOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view, err := g.store.dashboardOverview(ctx, g.onlineNodes())
	if err != nil {
		g.internalError(w, r, "build dashboard overview", err)
		return
	}
	view.Storage.Total = g.cfg.MaxArtifactBytes
	writeJSON(w, http.StatusOK, view)
}

func (g *Gateway) handleDashboardJobs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := defaultDashboardJobs
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "limit must be a positive number", false)
			return
		}
		limit = min(parsed, maxDashboardJobs)
	}
	filter := dashboardFilter{capability: query.Get("capability"), node: query.Get("node")}
	switch status := query.Get("status"); status {
	case "":
	case "active":
		filter.statuses = []string{StatusQueued, StatusAssigned, StatusRunning, StatusLost}
	case StatusQueued, StatusAssigned, StatusRunning, StatusLost, StatusSucceeded, StatusFailed, StatusTimedOut, StatusCancelled:
		filter.statuses = []string{status}
	default:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "unknown status", false)
		return
	}
	if raw := query.Get("before"); raw != "" {
		ms, id, ok := strings.Cut(raw, "_")
		parsed, err := strconv.ParseInt(ms, 10, 64)
		if !ok || err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "before isn't a valid cursor", false)
			return
		}
		filter.beforeMS, filter.beforeID = parsed, id
	}
	jobs, err := g.store.dashboardJobs(r.Context(), filter, limit+1)
	if err != nil {
		g.internalError(w, r, "list jobs", err)
		return
	}
	next := ""
	if len(jobs) > limit {
		jobs = jobs[:limit]
		last := jobs[limit-1]
		next = fmt.Sprintf("%d_%s", last.createdMS, last.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "next": next})
}

func (g *Gateway) handleDashboardJob(w http.ResponseWriter, r *http.Request) {
	detail, err := g.store.dashboardJobDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		g.internalError(w, r, "read job", err)
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found", false)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// --- Queries ---

func (s *Store) dashboardOverview(ctx context.Context, online onlineSet) (*DashboardOverview, error) {
	loads, err := s.NodeLoads(ctx)
	if err != nil {
		return nil, err
	}
	offers, err := s.CapabilityRows(ctx, "")
	if err != nil {
		return nil, err
	}
	offered := map[string][]string{}
	notReady := map[string]map[string]string{}
	var capabilities []string
	for _, offer := range offers {
		offered[offer.NodeID] = append(offered[offer.NodeID], offer.Name)
		if offer.NotReady != "" {
			if notReady[offer.NodeID] == nil {
				notReady[offer.NodeID] = map[string]string{}
			}
			notReady[offer.NodeID][offer.Name] = offer.NotReady
		}
		if len(capabilities) == 0 || capabilities[len(capabilities)-1] != offer.Name {
			capabilities = append(capabilities, offer.Name) // Rows are sorted by name.
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, name, labels, resources, max_jobs, os, arch, last_seen_ms FROM nodes WHERE revoked_ms IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	view := &DashboardOverview{
		GeneratedAt:  formatMS(nowMS()),
		Nodes:        []DashboardNode{},
		Active:       map[string]int{},
		Recent:       map[string]int{},
		Capabilities: orEmptySlice(capabilities),
	}
	for rows.Next() {
		var node DashboardNode
		var id, labels, resources string
		var lastSeen sql.NullInt64
		if err := rows.Scan(&id, &node.Name, &labels, &resources, &node.Jobs.Total, &node.OS, &node.Arch, &lastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		var totals map[string]int
		if err := json.Unmarshal([]byte(labels), &node.Labels); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(resources), &totals); err != nil {
			rows.Close()
			return nil, err
		}
		load := loads[id]
		if load == nil {
			load = &NodeLoad{}
		}
		node.Jobs.Used = load.Jobs
		node.Resources = make(map[string]ResourceUse, len(totals))
		for name, total := range totals {
			node.Resources[name] = ResourceUse{Used: load.Resources[name], Total: total}
		}
		node.Online = online.ids[id]
		node.Capabilities = orEmptySlice(offered[id])
		node.NotReady = notReady[id]
		if lastSeen.Valid {
			node.LastSeenAt = formatMS(lastSeen.Int64)
		}
		view.Nodes = append(view.Nodes, node)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.countStatuses(ctx, view.Active, `SELECT status, COUNT(*) FROM jobs WHERE status IN (?,?,?,?) GROUP BY status`,
		StatusQueued, StatusAssigned, StatusRunning, StatusLost); err != nil {
		return nil, err
	}
	if err := s.countStatuses(ctx, view.Recent, `SELECT status, COUNT(*) FROM jobs WHERE finished_ms > ? GROUP BY status`,
		nowMS()-dashboardRecentSpan.Milliseconds()); err != nil {
		return nil, err
	}
	if view.Storage.Used, err = s.TotalStoredBytes(ctx); err != nil {
		return nil, err
	}
	return view, nil
}

func (s *Store) countStatuses(ctx context.Context, into map[string]int, query string, args ...any) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		into[status] = count
	}
	return rows.Err()
}

type dashboardFilter struct {
	statuses   []string
	capability string
	node       string
	beforeMS   int64
	beforeID   string
}

const dashboardJobColumns = `
	SELECT j.id, j.capability, j.status, j.attempts, n.name, j.progress, j.error_code, j.error_message, k.name, j.created_ms, j.started_ms, j.finished_ms
	FROM jobs j
	JOIN api_keys k ON k.id = j.api_key_id
	LEFT JOIN attempts a ON a.id = j.current_attempt
	LEFT JOIN nodes n ON n.id = a.node_id`

func scanDashboardJob(scan func(...any) error) (DashboardJob, error) {
	var j DashboardJob
	var node, progress, errCode, errMessage sql.NullString
	var started, finished sql.NullInt64
	if err := scan(&j.ID, &j.Capability, &j.Status, &j.Attempts, &node, &progress, &errCode, &errMessage, &j.SubmittedBy, &j.createdMS, &started, &finished); err != nil {
		return j, err
	}
	j.Terminal = isTerminal(j.Status)
	j.Node = node.String
	if progress.Valid {
		j.Progress = json.RawMessage(progress.String)
	}
	if errCode.Valid {
		j.Error = &JobError{Code: errCode.String, Message: errMessage.String, Retryable: isRetryable(errCode.String)}
	}
	j.CreatedAt = formatMS(j.createdMS)
	if started.Valid {
		j.StartedAt = formatMS(started.Int64)
	}
	if finished.Valid {
		j.FinishedAt = formatMS(finished.Int64)
	}
	return j, nil
}

// dashboardJobs lists jobs newest first. The cursor is (created, id), because
// several jobs can share a millisecond and none may be skipped between pages.
func (s *Store) dashboardJobs(ctx context.Context, f dashboardFilter, limit int) ([]DashboardJob, error) {
	var where []string
	var args []any
	if len(f.statuses) > 0 {
		where = append(where, "j.status IN (?"+strings.Repeat(",?", len(f.statuses)-1)+")")
		for _, status := range f.statuses {
			args = append(args, status)
		}
	}
	if f.capability != "" {
		where = append(where, "j.capability = ?")
		args = append(args, f.capability)
	}
	if f.node != "" {
		where = append(where, "n.name = ?")
		args = append(args, f.node)
	}
	if f.beforeID != "" {
		where = append(where, "(j.created_ms < ? OR (j.created_ms = ? AND j.id < ?))")
		args = append(args, f.beforeMS, f.beforeMS, f.beforeID)
	}
	query := dashboardJobColumns
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY j.created_ms DESC, j.id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []DashboardJob{}
	for rows.Next() {
		job, err := scanDashboardJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) dashboardJobDetail(ctx context.Context, jobID string) (*DashboardJobDetail, error) {
	job, err := scanDashboardJob(s.db.QueryRowContext(ctx, dashboardJobColumns+" WHERE j.id = ?", jobID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	detail := &DashboardJobDetail{DashboardJob: job, Artifacts: []DashboardArtifact{}}
	if job.Status == StatusSucceeded {
		artifacts, err := s.jobArtifacts(ctx, jobID)
		if err != nil {
			return nil, err
		}
		for _, a := range artifacts {
			detail.Artifacts = append(detail.Artifacts, DashboardArtifact{Name: a.Name, Size: a.Size, ExpiresAt: a.ExpiresAt})
		}
	}
	events, err := s.JobEventsAfter(ctx, jobID, 0)
	if err != nil {
		return nil, err
	}
	for i := range events {
		events[i].Payload = withoutResult(events[i])
	}
	detail.Events = events
	if detail.Events == nil {
		detail.Events = []EventView{}
	}
	return detail, nil
}

// withoutResult removes a service's answer from a completed event, which is the
// one event that can carry a job's output.
func withoutResult(event EventView) json.RawMessage {
	if event.Type != protocol.EventCompleted {
		return event.Payload
	}
	var done protocol.Completion
	if err := json.Unmarshal(event.Payload, &done); err != nil {
		return nil
	}
	done.Result = nil
	clean, err := json.Marshal(done)
	if err != nil {
		return nil
	}
	return clean
}
