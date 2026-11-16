package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
)

// An admin key is a dashboard key that can also manage agent keys, remove
// machines that have gone away, and start the tests machines declare. It can
// show any agent key, so it's as powerful as all of them together.

// withAdminKey admits only admin dashboard keys.
func (g *Gateway) withAdminKey(next keyHandler) http.HandlerFunc {
	return g.withDashboardKey(func(w http.ResponseWriter, r *http.Request, key *APIKey) {
		if !key.Admin {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "this dashboard key can only view; make an admin key with: fleet-gateway key add NAME --admin", false)
			return
		}
		next(w, r, key)
	})
}

// decodeAdminBody reads a small JSON body, refusing unknown fields.
func decodeAdminBody(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body: "+err.Error(), false)
		return false
	}
	return true
}

// --- Keys ---

func (g *Gateway) handleListKeys(w http.ResponseWriter, r *http.Request, key *APIKey) {
	keys, err := g.store.ListKeys(r.Context())
	if err != nil {
		g.internalError(w, r, "list keys", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys, "self": key.ID, "mcp_url": g.publicBase(r) + "/mcp"})
}

type keyScopeRequest struct {
	Name  string   `json:"name"`
	Tools []string `json:"tools"`
	Nodes []string `json:"nodes"`
}

func (g *Gateway) handleCreateKey(w http.ResponseWriter, r *http.Request, admin *APIKey) {
	var req keyScopeRequest
	if !decodeAdminBody(w, r, &req) {
		return
	}
	secret, err := g.store.AddScopedAPIKey(r.Context(), req.Name, req.Tools, req.Nodes)
	if errors.Is(err, errKeyNameTaken) {
		writeError(w, http.StatusConflict, "NAME_TAKEN", err.Error(), false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), false)
		return
	}
	created, err := g.store.APIKeyBySecret(r.Context(), secret)
	if err != nil || created == nil {
		g.internalError(w, r, "read new key", err)
		return
	}
	g.log.Info("API key created from the dashboard", "key", req.Name, "by", admin.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"id": created.ID, "name": req.Name, "secret": secret, "mcp_url": g.publicBase(r) + "/mcp"})
}

func (g *Gateway) handleKeySecret(w http.ResponseWriter, r *http.Request, admin *APIKey) {
	name, secret, viewable, err := g.store.KeySecret(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), false)
		return
	}
	if err != nil {
		g.internalError(w, r, "open key", err)
		return
	}
	if !viewable {
		writeError(w, http.StatusConflict, "NOT_VIEWABLE", "this key was made before keys were kept, so it can't be shown; make a new one to replace it", false)
		return
	}
	// Who looked is worth knowing, since the key works for whoever has it.
	g.log.Info("API key shown on the dashboard", "key", name, "by", admin.Name)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "secret": secret, "mcp_url": g.publicBase(r) + "/mcp"})
}

func (g *Gateway) handleUpdateKey(w http.ResponseWriter, r *http.Request, admin *APIKey) {
	var req keyScopeRequest
	if !decodeAdminBody(w, r, &req) {
		return
	}
	err := g.store.UpdateKeyScope(r.Context(), r.PathValue("id"), req.Tools, req.Nodes)
	if errors.Is(err, ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), false)
		return
	}
	g.log.Info("API key's access changed from the dashboard", "key", r.PathValue("id"), "by", admin.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) handleRevokeKey(w http.ResponseWriter, r *http.Request, admin *APIKey) {
	id := r.PathValue("id")
	if id == admin.ID {
		writeError(w, http.StatusConflict, "SELF", "you're signed in with this key; revoke it from another admin key or on the server", false)
		return
	}
	name, err := g.store.RevokeKeyByID(r.Context(), id)
	if errors.Is(err, ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), false)
		return
	}
	if err != nil {
		g.internalError(w, r, "revoke key", err)
		return
	}
	g.log.Info("key revoked from the dashboard", "key", name, "by", admin.Name)
	w.WriteHeader(http.StatusNoContent)
}

// --- Machines ---

func (g *Gateway) handleRemoveNode(w http.ResponseWriter, r *http.Request, admin *APIKey) {
	name := r.PathValue("name")
	for _, node := range g.onlineNodes().nodes {
		if node.name == name {
			writeError(w, http.StatusConflict, "NODE_ONLINE", name+" is connected; only a machine that's offline can be removed", false)
			return
		}
	}
	id, err := g.store.RemoveNode(r.Context(), name)
	if errors.Is(err, ErrNodeNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), false)
		return
	}
	if err != nil {
		g.internalError(w, r, "remove node", err)
		return
	}
	// It may have connected in the moment since it was checked. Its token no
	// longer works, so drop the connection rather than wait for a heartbeat.
	g.mu.Lock()
	conn := g.conns[id]
	g.mu.Unlock()
	if conn != nil {
		conn.cancel()
	}
	g.log.Info("machine removed from the dashboard", "node", name, "by", admin.Name)
	w.WriteHeader(http.StatusNoContent)
}

type nodeTestRequest struct {
	// Capability tests one tool. Empty tests every tool that declares a test.
	Capability string `json:"capability"`
}

type nodeTestStarted struct {
	Capability string   `json:"capability"`
	Job        *JobView `json:"job"`
	// Already says this test was running, so no new one was started.
	Already bool `json:"already,omitempty"`
}

type nodeTestSkipped struct {
	Capability string `json:"capability"`
	Reason     string `json:"reason"`
}

// handleTestNode runs the tests a machine declares for its tools: what runs
// comes from the worker, never from the dashboard.
func (g *Gateway) handleTestNode(w http.ResponseWriter, r *http.Request, admin *APIKey) {
	var req nodeTestRequest
	if r.ContentLength != 0 && !decodeAdminBody(w, r, &req) {
		return
	}
	nodeName := r.PathValue("name")
	rows, err := g.store.CapabilityRows(r.Context(), req.Capability)
	if err != nil {
		g.internalError(w, r, "look up capabilities", err)
		return
	}
	offers := onNode(rows, nodeName)
	if len(offers) == 0 {
		message := nodeName + " offers no tools"
		if req.Capability != "" {
			message = nodeName + " doesn't offer " + req.Capability
		}
		writeError(w, http.StatusNotFound, "NOT_FOUND", message, false)
		return
	}
	lastTests, err := g.store.LastTests(r.Context())
	if err != nil {
		g.internalError(w, r, "list tests", err)
		return
	}
	online := g.onlineNodes()
	if !online.ids[offers[0].NodeID] {
		writeError(w, http.StatusConflict, "NODE_OFFLINE", nodeName+" isn't connected, so nothing can be tested on it", true)
		return
	}
	sort.Slice(offers, func(i, j int) bool { return offers[i].Name < offers[j].Name })
	started := []nodeTestStarted{}
	skipped := []nodeTestSkipped{}
	anyCreated := false
	for _, offer := range offers {
		if reason, _ := testBlocked(offer, online); reason != "" {
			skipped = append(skipped, nodeTestSkipped{Capability: offer.Name, Reason: reason})
			continue
		}
		// A second click while a test is running follows that test.
		if last := lastTests[testKey{offer.NodeID, offer.Name}]; last != nil && !last.Finished {
			if view, err := g.store.GetJob(r.Context(), last.JobID, admin.ID, false); err == nil && view != nil {
				started = append(started, nodeTestStarted{Capability: offer.Name, Job: view, Already: true})
				continue
			}
		}
		view, created, err := g.startTest(r.Context(), admin, offer, "", defaultQueueTimeout)
		if err != nil {
			g.internalError(w, r, "start test", err)
			return
		}
		anyCreated = anyCreated || created
		started = append(started, nodeTestStarted{Capability: offer.Name, Job: view})
	}
	if anyCreated {
		g.wakeScheduler()
	}
	status := http.StatusAccepted
	if len(started) == 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"node": nodeName, "tests": started, "skipped": skipped})
}
