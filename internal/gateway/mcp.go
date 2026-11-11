package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// The gateway is an MCP server at /mcp, using the Streamable HTTP transport.
// Every tool a machine offers becomes an MCP tool, so any MCP client can use
// them with nothing installed on the machine it runs on.
//
// It speaks both eras of the protocol. Modern requests (2026-07-28) carry their
// version in _meta and are served statelessly. Older clients open with an
// initialize handshake; they're served too, still without sessions, which those
// versions allow.

const (
	mcpModernVersion = "2026-07-28"
	mcpServerName    = "fleet-gateway"
	// maxMCPBodyBytes leaves room for a file sent to fleet_file_upload as base64.
	maxMCPBodyBytes = 32 << 20
	// mcpKeepAlive keeps a quiet stream from being closed by a proxy.
	mcpKeepAlive = 20 * time.Second

	metaProtocolVersion     = "io.modelcontextprotocol/protocolVersion"
	metaClientCapabilities  = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo          = "io.modelcontextprotocol/serverInfo"
	metaProgressToken       = "progressToken"
	rpcParseError           = -32700
	rpcInvalidRequest       = -32600
	rpcMethodNotFound       = -32601
	rpcInvalidParams        = -32602
	rpcInternalError        = -32603
	rpcHeaderMismatch       = -32020
	rpcUnsupportedVersion   = -32022
	mcpInstructionsTemplate = `These tools run on the user's own machines, through a gateway that passes each call to a machine that offers the tool.

- A call waits up to %s for its job. A longer job answers with its job_id instead: pass that to fleet_job_wait for the result, or to fleet_job_cancel to stop it.
- Results include what the program printed and its exit code. Result files come with download links that work without credentials until they expire, and pictures are shown inline.
- To give a tool a file, pass a file id in its input_files: the id of a file from an earlier result, or one from fleet_file_upload.
- fleet_status shows which machines are online and why a tool isn't ready.`
)

// mcpLegacyVersions are the handshake-based versions still served, newest first.
var mcpLegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
	status  int
}

func (e *rpcError) Error() string { return e.Message }

func newRPCError(status, code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...), status: status}
}

// mcpRequest is one request and what's needed to answer it.
type mcpRequest struct {
	g       *Gateway
	key     *APIKey
	modern  bool
	version string
	base    string // Where agents reach the gateway, for links.
	id      json.RawMessage

	progressToken json.RawMessage
	stream        *sseStream // Set once the answer has become a stream.
	wantsStream   bool
}

func (g *Gateway) handleMCPOtherMethod(w http.ResponseWriter, r *http.Request) {
	// There are no sessions to end and no standalone stream to open.
	w.Header().Set("Allow", "POST")
	writeRPCError(w, nil, newRPCError(http.StatusMethodNotAllowed, rpcInvalidRequest, "this MCP server only answers POST requests"))
}

func (g *Gateway) handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// A web page the user visits mustn't be able to drive the gateway with
	// credentials the browser happens to hold.
	if origin := r.Header.Get("Origin"); origin != "" && !strings.EqualFold(strings.TrimRight(origin, "/"), g.publicBase(r)) {
		writeRPCError(w, nil, newRPCError(http.StatusForbidden, rpcInvalidRequest, "requests from web pages on other sites aren't accepted"))
		return
	}
	key, failure := g.mcpKey(r)
	if failure != nil {
		if failure.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fleet"`)
		}
		writeRPCError(w, nil, failure)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBodyBytes))
	if err != nil {
		writeRPCError(w, nil, newRPCError(http.StatusRequestEntityTooLarge, rpcInvalidRequest, "the request is over %d MB; upload big files with a link from fleet_file_upload instead", maxMCPBodyBytes>>20))
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		writeRPCError(w, nil, newRPCError(http.StatusBadRequest, rpcInvalidRequest, "send one message per request; batches aren't supported"))
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		writeRPCError(w, nil, newRPCError(http.StatusBadRequest, rpcParseError, "the body isn't valid JSON"))
		return
	}
	if msg.JSONRPC != "2.0" || msg.Method == "" || string(msg.ID) == "null" {
		writeRPCError(w, msg.ID, newRPCError(http.StatusBadRequest, rpcInvalidRequest, "not a valid JSON-RPC 2.0 message"))
		return
	}
	if msg.ID == nil {
		// Notifications such as notifications/initialized need no answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	req := &mcpRequest{g: g, key: key, id: msg.ID, base: g.publicBase(r)}
	if failure := req.negotiate(r, msg); failure != nil {
		writeRPCError(w, msg.ID, failure)
		return
	}
	req.wantsStream = req.progressToken != nil && acceptsEventStream(r)

	result, failure := req.dispatch(r.Context(), w, msg)
	if failure != nil {
		if req.stream != nil {
			req.stream.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": failure})
			return
		}
		writeRPCError(w, msg.ID, failure)
		return
	}
	if req.modern {
		result["resultType"] = "complete"
		meta, _ := result["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta[metaServerInfo] = g.mcpServerInfo()
		result["_meta"] = meta
	}
	response := map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}
	if req.stream != nil {
		req.stream.send(response)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// mcpKey accepts agent keys only, as the rest of the agent API does.
func (g *Gateway) mcpKey(r *http.Request) (*APIKey, *rpcError) {
	secret := bearerToken(r)
	if secret == "" {
		return nil, newRPCError(http.StatusUnauthorized, rpcInvalidRequest, "send an API key as Authorization: Bearer <key>")
	}
	key, err := g.store.APIKeyBySecret(r.Context(), secret)
	if err != nil {
		g.logError(r.Context(), "check API key", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	if key == nil {
		return nil, newRPCError(http.StatusUnauthorized, rpcInvalidRequest, "invalid API key")
	}
	if key.Dashboard {
		return nil, newRPCError(http.StatusForbidden, rpcInvalidRequest, "a dashboard key can only view the dashboard")
	}
	return key, nil
}

func acceptsEventStream(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part)); err == nil && mediaType == "text/event-stream" {
			return true
		}
	}
	return false
}

// negotiate works out which era and version a request uses, and checks what a
// modern request must carry.
func (req *mcpRequest) negotiate(r *http.Request, msg rpcMessage) *rpcError {
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
		Name *string                    `json:"name"`
		URI  *string                    `json:"uri"`
	}
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return newRPCError(http.StatusBadRequest, rpcInvalidParams, "params must be an object")
		}
	}
	if token, ok := params.Meta[metaProgressToken]; ok && string(token) != "null" {
		req.progressToken = token
	}
	var metaVersion string
	if raw, ok := params.Meta[metaProtocolVersion]; ok {
		if err := json.Unmarshal(raw, &metaVersion); err != nil {
			return newRPCError(http.StatusBadRequest, rpcInvalidParams, "%s must be a string", metaProtocolVersion)
		}
	}
	header := r.Header.Get("MCP-Protocol-Version")
	supported := append([]string{mcpModernVersion}, mcpLegacyVersions...)

	// initialize always means the older, handshake-based protocol.
	if msg.Method == "initialize" {
		req.version = mcpLegacyVersions[0]
		return nil
	}
	if metaVersion == "" && header != "" && header != mcpModernVersion && !slices.Contains(mcpLegacyVersions, header) {
		return unsupportedVersion(header, supported)
	}
	if metaVersion == "" && header != mcpModernVersion {
		// An older client, after its handshake. Before 2025-06-18 there was no header.
		req.version = header
		if req.version == "" {
			req.version = "2025-03-26"
		}
		return nil
	}

	req.modern = true
	if metaVersion == "" {
		return newRPCError(http.StatusBadRequest, rpcInvalidParams, "each request must give its protocol version as _meta[%q]", metaProtocolVersion)
	}
	if metaVersion != mcpModernVersion {
		return unsupportedVersion(metaVersion, supported)
	}
	if header != metaVersion {
		return newRPCError(http.StatusBadRequest, rpcHeaderMismatch, "Header mismatch: MCP-Protocol-Version %q doesn't match the body's %q", header, metaVersion)
	}
	if _, ok := params.Meta[metaClientCapabilities]; !ok {
		return newRPCError(http.StatusBadRequest, rpcInvalidParams, "each request must give its client capabilities as _meta[%q]", metaClientCapabilities)
	}
	if got := r.Header.Get("Mcp-Method"); got != msg.Method {
		return newRPCError(http.StatusBadRequest, rpcHeaderMismatch, "Header mismatch: Mcp-Method %q doesn't match the body's %q", got, msg.Method)
	}
	var name *string
	switch msg.Method {
	case "tools/call", "prompts/get":
		name = params.Name
	case "resources/read":
		name = params.URI
	}
	if name != nil {
		got, ok := decodeHeaderValue(r.Header.Get("Mcp-Name"))
		if !ok || got != *name {
			return newRPCError(http.StatusBadRequest, rpcHeaderMismatch, "Header mismatch: Mcp-Name doesn't match the body's %q", *name)
		}
	}
	req.version = metaVersion
	return nil
}

func unsupportedVersion(requested string, supported []string) *rpcError {
	failure := newRPCError(http.StatusBadRequest, rpcUnsupportedVersion, "Unsupported protocol version")
	failure.Data = map[string]any{"supported": supported, "requested": requested}
	return failure
}

// decodeHeaderValue undoes the =?base64?...?= form clients use for values that
// can't be sent as plain header text.
func decodeHeaderValue(value string) (string, bool) {
	inner, ok := strings.CutPrefix(value, "=?base64?")
	if !ok {
		return value, value != ""
	}
	inner, ok = strings.CutSuffix(inner, "?=")
	if !ok {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(inner)
	return string(decoded), err == nil
}

func (req *mcpRequest) dispatch(ctx context.Context, w http.ResponseWriter, msg rpcMessage) (map[string]any, *rpcError) {
	switch msg.Method {
	case "initialize":
		return req.initialize(msg.Params)
	case "server/discover":
		if req.modern {
			req.g.log.Info("MCP client connected", "protocol", req.version)
			return map[string]any{
				"supportedVersions": append([]string{mcpModernVersion}, mcpLegacyVersions...),
				"capabilities":      map[string]any{"tools": map[string]any{}},
				"instructions":      req.g.mcpInstructions(),
			}, nil
		}
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools, err := req.g.mcpToolList(ctx, req.key)
		if err != nil {
			req.g.logError(ctx, "list MCP tools", err)
			return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
		}
		out := make([]any, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.definition())
		}
		return map[string]any{"tools": out}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.Name == "" {
			return nil, newRPCError(http.StatusBadRequest, rpcInvalidParams, "tools/call needs a tool name")
		}
		if req.wantsStream {
			req.stream = startStream(w)
		}
		return req.callTool(ctx, params.Name, params.Arguments)
	}
	status := http.StatusOK
	if req.modern {
		status = http.StatusNotFound
	}
	return nil, newRPCError(status, rpcMethodNotFound, "Method not found: %s", msg.Method)
}

func (req *mcpRequest) initialize(raw json.RawMessage) (map[string]any, *rpcError) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &params)
	version := mcpLegacyVersions[0]
	if slices.Contains(mcpLegacyVersions, params.ProtocolVersion) {
		version = params.ProtocolVersion
	}
	req.version = version
	var client struct {
		ClientInfo struct{ Name, Version string } `json:"clientInfo"`
	}
	_ = json.Unmarshal(raw, &client)
	req.g.log.Info("MCP client connected", "client", client.ClientInfo.Name, "client_version", client.ClientInfo.Version, "protocol", version)
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      req.g.mcpServerInfo(),
		"instructions":    req.g.mcpInstructions(),
	}, nil
}

func (g *Gateway) mcpServerInfo() map[string]string {
	version := g.cfg.Version
	if version == "" {
		version = "dev"
	}
	return map[string]string{"name": mcpServerName, "title": "Fleet gateway", "version": version}
}

func (g *Gateway) mcpInstructions() string {
	return fmt.Sprintf(mcpInstructionsTemplate, g.cfg.MCPWait.Round(time.Second))
}

// supportsResourceLinks says whether the client knows resource_link content,
// which arrived in 2025-06-18.
func (req *mcpRequest) supportsResourceLinks() bool {
	return req.modern || req.version >= "2025-06-18"
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, failure *rpcError) {
	status := failure.status
	if status == 0 {
		status = http.StatusOK
	}
	response := map[string]any{"jsonrpc": "2.0", "error": failure}
	if id != nil {
		response["id"] = id
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

// --- Streaming answers ---

// sseStream answers one request as server-sent events: progress notifications,
// then the response.
type sseStream struct {
	mu   sync.Mutex
	w    http.ResponseWriter
	rc   *http.ResponseController
	last time.Time
}

func startStream(w http.ResponseWriter) *sseStream {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &sseStream{w: w, rc: http.NewResponseController(w), last: time.Now()}
	_ = s.rc.Flush()
	return s
}

func (s *sseStream) send(message any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "event: message\ndata: %s\n\n", data)
	_ = s.rc.Flush()
	s.last = time.Now()
}

// keepAlive sends a comment if nothing has been sent for a while.
func (s *sseStream) keepAlive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.last) < mcpKeepAlive {
		return
	}
	io.WriteString(s.w, ":\n\n")
	_ = s.rc.Flush()
	s.last = time.Now()
}

// progress tells the client how a job is going, if it asked to be told.
func (req *mcpRequest) progress(done, total float64, message string) {
	if req.stream == nil {
		return
	}
	params := map[string]any{"progressToken": req.progressToken, "progress": done}
	if total > 0 {
		params["total"] = total
	}
	if message != "" {
		params["message"] = message
	}
	req.stream.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": params})
}

// clientGone reports whether ctx ended because the client went away, rather
// than because the gateway is stopping.
func (g *Gateway) clientGone(ctx context.Context) bool {
	return ctx.Err() != nil && g.runCtx.Err() == nil && !errors.Is(context.Cause(ctx), context.DeadlineExceeded)
}
