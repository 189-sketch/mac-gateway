package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	toolJobWait    = "fleet_job_wait"
	toolJobCancel  = "fleet_job_cancel"
	toolStatus     = "fleet_status"
	toolFileUpload = "fleet_file_upload"

	maxMCPToolName = 64
	// Pictures up to this size are shown inline, up to the total.
	maxInlineImage  = 5 << 20
	maxInlineImages = 10 << 20
	// Text files up to this size are shown inline.
	maxInlineText = 100 << 10
	// A successful job shows the end of its output; asking gives all the
	// gateway kept.
	successOutputBytes = 8 << 10
	maxJobWait         = 10 * time.Minute
	mcpPollInterval    = 250 * time.Millisecond
	maxUploadContent   = 20 << 20
)

var mcpNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// mcpTool is one tool as MCP clients see it.
type mcpTool struct {
	name        string
	title       string
	description string
	schema      map[string]any
	annotations map[string]any

	capability string // Empty for the gateway's own tools.
	filesField string // The argument that carries input file ids, if any.
	inputFiles []string
}

func (t mcpTool) definition() map[string]any {
	d := map[string]any{"name": t.name, "description": t.description, "inputSchema": t.schema}
	if t.title != "" {
		d["title"] = t.title
	}
	if t.annotations != nil {
		d["annotations"] = t.annotations
	}
	return d
}

// mcpToolName makes a capability name safe for every client. MCP allows dots,
// but some clients pass tool names to models that don't.
func mcpToolName(capability string) string {
	name := mcpNameUnsafe.ReplaceAllString(capability, "_")
	if len(name) > maxMCPToolName {
		name = name[:maxMCPToolName-7] + "_" + shortHash(capability)
	}
	return name
}

func shortHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:3])
}

// mcpToolList lists the tools the key may call, in the same order each time so
// clients can cache the list.
func (g *Gateway) mcpToolList(ctx context.Context, key *APIKey) ([]mcpTool, error) {
	rows, err := g.store.CapabilityRows(ctx, "")
	if err != nil {
		return nil, err
	}
	type offer struct {
		schema      string
		description string
		inputFiles  []string
	}
	offers := map[string]*offer{}
	var names []string
	for _, row := range rows {
		if !keyAllows(key, row.Name) {
			continue
		}
		o := offers[row.Name]
		if o == nil {
			o = &offer{schema: row.InputSchema, inputFiles: row.InputFiles}
			offers[row.Name] = o
			names = append(names, row.Name)
		}
		if o.description == "" {
			o.description = row.Description
		}
	}
	sort.Strings(names)

	builtins := g.builtinTools(len(names) > 0 && anyTakesFiles(offers, func(o *offer) []string { return o.inputFiles }))
	taken := map[string]bool{}
	for _, t := range builtins {
		taken[t.name] = true
	}
	tools := make([]mcpTool, 0, len(names)+len(builtins))
	for _, name := range names {
		o := offers[name]
		toolName := mcpToolName(name)
		if taken[toolName] {
			toolName = toolName[:min(len(toolName), maxMCPToolName-7)] + "_" + shortHash(name)
		}
		taken[toolName] = true
		tool := mcpTool{name: toolName, title: name, capability: name, inputFiles: o.inputFiles}
		tool.schema, tool.filesField = withFileField(o.schema, o.inputFiles)
		tool.description = g.capabilityDescription(name, o.description, tool.filesField, o.inputFiles)
		tools = append(tools, tool)
	}
	return append(tools, builtins...), nil
}

func anyTakesFiles[T any](offers map[string]T, files func(T) []string) bool {
	for _, o := range offers {
		if len(files(o)) > 0 {
			return true
		}
	}
	return false
}

func (g *Gateway) capabilityDescription(name, description, filesField string, inputFiles []string) string {
	var b strings.Builder
	if description == "" {
		description = fmt.Sprintf("Runs %s on one of the user's machines.", name)
	}
	b.WriteString(strings.TrimSpace(description))
	fmt.Fprintf(&b, "\n\nThis runs on one of the user's machines (as %s). If it takes longer than %s, the answer is a job_id instead of the result: pass it to %s.", name, g.cfg.MCPWait.Round(time.Second), toolJobWait)
	if filesField != "" {
		fmt.Fprintf(&b, " It needs these files, given as file ids in %s: %s. A file id comes from an earlier tool's result or from %s.", filesField, strings.Join(inputFiles, ", "), toolFileUpload)
	}
	return b.String()
}

// withFileField adds an argument for the tool's input files to its schema.
func withFileField(schemaText string, inputFiles []string) (map[string]any, string) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaText), &schema); err != nil || schema == nil {
		schema = map[string]any{"type": "object"}
	}
	if len(inputFiles) == 0 {
		return schema, ""
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	field := "input_files"
	if _, clash := props[field]; clash {
		field = "fleet_input_files"
	}
	fileProps := map[string]any{}
	required := make([]any, 0, len(inputFiles))
	for _, name := range inputFiles {
		fileProps[name] = map[string]any{"type": "string", "description": "A file id, such as file_..."}
		required = append(required, name)
	}
	props[field] = map[string]any{
		"type":                 "object",
		"description":          "The files this tool needs, each as a file id from an earlier result or from " + toolFileUpload + ".",
		"properties":           fileProps,
		"required":             required,
		"additionalProperties": false,
	}
	schema["properties"] = props
	existing, _ := schema["required"].([]any)
	schema["required"] = append(existing, field)
	if schema["type"] == nil {
		schema["type"] = "object"
	}
	return schema, field
}

func (g *Gateway) builtinTools(files bool) []mcpTool {
	waitSeconds := int(g.cfg.MCPWait / time.Second)
	jobID := map[string]any{"type": "string", "description": "The job_id a tool answered with, such as job_..."}
	tools := []mcpTool{
		{
			name:  toolJobWait,
			title: "Wait for a job",
			description: "Waits for a job started by another tool to finish, then gives its result, output and files, as the tool would have. " +
				"Use it when a tool answered with a job_id because the job was still running. If the job is still going when the wait ends, call this again.",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":      jobID,
					"seconds":     map[string]any{"type": "integer", "minimum": 0, "maximum": int(maxJobWait / time.Second), "description": fmt.Sprintf("How long to wait for it to finish. 0 checks without waiting. The default is %d.", waitSeconds)},
					"full_output": map[string]any{"type": "boolean", "description": "Give all the output that was kept, the last 64 KB, rather than its last 8 KB."},
				},
				"required":             []any{"job_id"},
				"additionalProperties": false,
			},
			annotations: map[string]any{"readOnlyHint": true},
		},
		{
			name:        toolJobCancel,
			title:       "Stop a job",
			description: "Stops a job that's queued or running, and frees its machine. A job that has already finished is left as it is. Give the job_id a tool answered with.",
			schema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"job_id": jobID},
				"required":             []any{"job_id"},
				"additionalProperties": false,
			},
			annotations: map[string]any{"destructiveHint": true, "idempotentHint": true},
		},
		{
			name:  toolStatus,
			title: "Machines and tools",
			description: "Shows which of the user's machines are online, and for each tool, which machines can run it now or why they can't. " +
				"Use it when a tool says no machine can run it, or to see what's available.",
			schema:      map[string]any{"type": "object", "additionalProperties": false},
			annotations: map[string]any{"readOnlyHint": true},
		},
	}
	if files {
		tools = append(tools, mcpTool{
			name:  toolFileUpload,
			title: "Upload a file",
			description: fmt.Sprintf("Stores a file for a tool to use, and gives its file id. Give content_base64 for a file up to %d MB. "+
				"Without it, you get a link to upload a file of any size, such as with curl -T, which works for an hour. Files are kept for %s.", maxUploadContent>>20, g.cfg.ArtifactTTL),
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":           map[string]any{"type": "string", "pattern": storedFileName.String(), "description": "The file's name, such as voice.wav. The tool sees this name."},
					"content_base64": map[string]any{"type": "string", "description": "The file's contents, base64-encoded."},
				},
				"required":             []any{"name"},
				"additionalProperties": false,
			},
		})
	}
	return tools
}

// --- Calling tools ---

func toolError(format string, args ...any) map[string]any {
	return map[string]any{"content": []any{textBlock(fmt.Sprintf(format, args...))}, "isError": true}
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func (req *mcpRequest) callTool(ctx context.Context, name string, args json.RawMessage) (map[string]any, *rpcError) {
	tools, err := req.g.mcpToolList(ctx, req.key)
	if err != nil {
		req.g.logError(ctx, "list MCP tools", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	var tool *mcpTool
	for i := range tools {
		if tools[i].name == name {
			tool = &tools[i]
			break
		}
	}
	if tool == nil {
		return nil, newRPCError(http.StatusOK, rpcInvalidParams, "Unknown tool: %s", name)
	}
	arguments := map[string]json.RawMessage{}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return toolError("The arguments must be a JSON object."), nil
		}
	}
	switch tool.name {
	case toolJobWait:
		return req.jobWait(ctx, arguments)
	case toolJobCancel:
		return req.jobCancel(ctx, arguments)
	case toolStatus:
		return req.status(ctx)
	case toolFileUpload:
		return req.fileUpload(ctx, arguments)
	}
	return req.runCapability(ctx, tool, arguments)
}

func (req *mcpRequest) runCapability(ctx context.Context, tool *mcpTool, arguments map[string]json.RawMessage) (map[string]any, *rpcError) {
	g := req.g
	files := map[string]string{}
	if tool.filesField != "" {
		raw := arguments[tool.filesField]
		delete(arguments, tool.filesField)
		var refs map[string]string
		if err := json.Unmarshal(raw, &refs); err != nil {
			return toolError("%s must map each of %s to a file id.", tool.filesField, strings.Join(tool.inputFiles, ", ")), nil
		}
		for field, ref := range refs {
			id, err := g.reuseFile(ctx, req.key.ID, ref)
			if err != nil {
				return toolError("%s.%s: %v", tool.filesField, field, err), nil
			}
			files[field] = id
		}
	}
	input, err := json.Marshal(arguments)
	if err != nil {
		return toolError("The arguments can't be read: %v", err), nil
	}
	// A job no machine can take now would only wait in the queue, so say why
	// instead, while the agent can still do something about it.
	if !g.onlineNodes().offers(tool.capability, nil, "") {
		return toolError("%s", g.unavailableReason(ctx, tool.capability)), nil
	}
	view, _, failure := g.submitJob(ctx, req.key, createJobRequest{Capability: tool.capability, Input: input, Files: files})
	if failure != nil {
		if failure.err != nil {
			g.logError(ctx, failure.message, failure.err)
			return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
		}
		return toolError("The gateway refused the call (%s): %s", failure.code, failure.message), nil
	}
	g.log.Info("MCP tool call", "capability", tool.capability, "job", view.ID)
	return req.waitAndRender(ctx, view.ID, g.cfg.MCPWait, false, true)
}

// unavailableReason says why no machine can run a capability now.
func (g *Gateway) unavailableReason(ctx context.Context, capability string) string {
	rows, err := g.store.CapabilityRows(ctx, capability)
	if err != nil || len(rows) == 0 {
		return fmt.Sprintf("No machine offers %s any more.", capability)
	}
	online := g.onlineNodes()
	var reasons []string
	for _, row := range rows {
		switch {
		case !online.ids[row.NodeID]:
			reasons = append(reasons, row.NodeName+" is offline")
		case row.NotReady != "":
			reasons = append(reasons, fmt.Sprintf("%s isn't ready to run it (%s)", row.NodeName, row.NotReady))
		}
	}
	return fmt.Sprintf("No machine can run %s right now: %s. Nothing was started. Try again once a machine is back, or check %s.", capability, strings.Join(reasons, "; "), toolStatus)
}

// waitAndRender waits for a job to finish, or for the wait to end, and
// describes it. A modern client that goes away while waiting for the job it
// started has cancelled the call, so the job stops too.
func (req *mcpRequest) waitAndRender(ctx context.Context, jobID string, wait time.Duration, fullOutput, ownsJob bool) (map[string]any, *rpcError) {
	g := req.g
	deadline := time.Now().Add(wait)
	var lastProgress string
	var lastSent float64
	for {
		view, err := g.store.GetJob(ctx, jobID, req.key.ID, false)
		if err != nil && ctx.Err() == nil {
			g.logError(ctx, "read job", err)
			return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
		}
		if err == nil {
			if view == nil {
				return toolError("There's no job %s for this key. It may have been removed after finishing.", jobID), nil
			}
			if view.Terminal || !time.Now().Before(deadline) {
				return req.render(ctx, view, fullOutput)
			}
			if p := string(view.Progress); p != lastProgress && p != "" {
				lastProgress = p
				lastSent = req.sendJobProgress(view.Progress, lastSent)
			}
		}
		if req.stream != nil {
			req.stream.keepAlive()
		}
		select {
		case <-ctx.Done():
			if ownsJob && req.modern && g.clientGone(ctx) {
				if _, err := g.cancelJob(context.WithoutCancel(ctx), jobID, req.key.ID); err != nil {
					g.logError(ctx, "cancel job of a dropped MCP call", err)
				} else {
					g.log.Info("MCP client went away, so its job was cancelled", "job", jobID)
				}
			}
			return nil, newRPCError(http.StatusServiceUnavailable, rpcInternalError, "the request ended")
		case <-time.After(mcpPollInterval):
		}
	}
}

type jobProgress struct {
	Stage   string   `json:"stage"`
	Percent *float64 `json:"percent"`
	Current string   `json:"current"`
	Total   string   `json:"total"`
	Message string   `json:"message"`
}

func (p jobProgress) text() string {
	var parts []string
	if p.Stage != "" && p.Stage != "running" {
		parts = append(parts, p.Stage)
	}
	if p.Percent != nil {
		parts = append(parts, fmt.Sprintf("%.0f%%", *p.Percent))
	}
	if p.Current != "" && p.Total != "" {
		parts = append(parts, p.Current+" of "+p.Total)
	} else if p.Current != "" {
		parts = append(parts, p.Current)
	}
	if p.Message != "" {
		parts = append(parts, p.Message)
	}
	return strings.Join(parts, ", ")
}

// sendJobProgress passes a job's progress on. MCP progress must only go up,
// so a percentage that went down is sent as a small step instead.
func (req *mcpRequest) sendJobProgress(raw json.RawMessage, last float64) float64 {
	var p jobProgress
	if json.Unmarshal(raw, &p) != nil {
		return last
	}
	next, total := last+0.01, 0.0
	if p.Percent != nil && *p.Percent > last {
		next, total = *p.Percent, 100
	}
	req.progress(next, total, p.text())
	return next
}

// render describes a job as a tool result.
func (req *mcpRequest) render(ctx context.Context, view *JobView, fullOutput bool) (map[string]any, *rpcError) {
	structured := map[string]any{"job_id": view.ID, "tool": view.Capability, "status": view.Status}
	if view.Node != "" {
		structured["machine"] = view.Node
	}
	where := ""
	if view.Node != "" {
		where = " on " + view.Node
	}
	if !view.Terminal {
		text := fmt.Sprintf("Job %s is still %s%s", view.ID, view.Status, where)
		var p jobProgress
		if len(view.Progress) > 0 && json.Unmarshal(view.Progress, &p) == nil && p.text() != "" {
			text += " (" + p.text() + ")"
			structured["progress"] = view.Progress
		}
		text += fmt.Sprintf(", so there's no result yet. Call %s with job_id %q to wait for it, or %s to stop it.", toolJobWait, view.ID, toolJobCancel)
		return map[string]any{"content": []any{textBlock(text)}, "structuredContent": structured}, nil
	}
	if view.ExitCode != nil {
		structured["exit_code"] = *view.ExitCode
	}
	took := ""
	if start, err1 := time.Parse(time.RFC3339Nano, view.StartedAt); err1 == nil {
		if end, err2 := time.Parse(time.RFC3339Nano, view.FinishedAt); err2 == nil {
			took = " in " + end.Sub(start).Round(100*time.Millisecond).String()
		}
	}

	content := []any{}
	if view.Status != StatusSucceeded {
		structured["error"] = view.Error
		message := fmt.Sprintf("Job %s %s%s%s.", view.ID, strings.ReplaceAll(view.Status, "_", " "), where, took)
		if view.Error != nil {
			message += fmt.Sprintf("\n%s: %s", view.Error.Code, view.Error.Message)
			if view.Error.Retryable {
				message += "\nTrying again may work."
			}
		}
		if view.ExitCode != nil {
			message += fmt.Sprintf("\nExit code %d.", *view.ExitCode)
		}
		content = append(content, textBlock(message))
		if view.Output != "" {
			content = append(content, textBlock("What it printed:\n"+view.Output))
			structured["output"] = view.Output
		}
		return map[string]any{"content": content, "structuredContent": structured, "isError": true}, nil
	}

	if len(view.Result) > 0 {
		structured["result"] = view.Result
		var text string
		if json.Unmarshal(view.Result, &text) != nil {
			var pretty bytes.Buffer
			if json.Indent(&pretty, view.Result, "", "  ") == nil {
				text = pretty.String()
			} else {
				text = string(view.Result)
			}
		}
		content = append(content, textBlock(text))
	}

	summary := fmt.Sprintf("Job %s finished%s%s.", view.ID, where, took)
	if view.ExitCode != nil {
		summary += fmt.Sprintf(" Exit code %d.", *view.ExitCode)
	}
	files, blocks := req.renderFiles(ctx, view.Artifacts)
	if len(files) > 0 {
		structured["files"] = files
		summary += "\n\nFiles (each link downloads without credentials until it expires; pass a file id in another tool's input_files to use the file there):"
		for _, f := range files {
			summary += fmt.Sprintf("\n- %s (%s, %s), id %s: %s (until %s)", f["name"], humanSize(f["size"].(int64)), f["mime_type"], f["id"], f["url"], f["expires_at"])
		}
	}
	content = append(content, textBlock(summary))
	content = append(content, blocks...)

	if view.Output != "" {
		output := view.Output
		label := "What it printed:\n"
		if !fullOutput && len(output) > successOutputBytes {
			output = tailText(output, successOutputBytes)
			label = fmt.Sprintf("The end of what it printed (%s with full_output gives all of it):\n", toolJobWait)
			structured["output_truncated"] = true
		}
		content = append(content, textBlock(label+output))
		structured["output"] = output
	}
	return map[string]any{"content": content, "structuredContent": structured}, nil
}

// renderFiles links each result file and shows small pictures and text inline.
func (req *mcpRequest) renderFiles(ctx context.Context, artifacts []ArtifactView) ([]map[string]any, []any) {
	var files []map[string]any
	var blocks []any
	inlined := int64(0)
	for _, a := range artifacts {
		expires, _ := time.Parse(time.RFC3339Nano, a.ExpiresAt)
		link, until := req.g.downloadLink(req.base, req.key.ID, a.ID, expires)
		mimeType := mimeTypeFor(a.Name)
		files = append(files, map[string]any{
			"id": a.ID, "name": a.Name, "size": a.Size, "sha256": a.SHA256, "mime_type": mimeType,
			"url": link, "expires_at": until.UTC().Format(time.RFC3339),
		})
		path := filepath.Join(req.g.cfg.filesDir(), a.ID)
		switch {
		case isInlineImage(mimeType) && a.Size <= maxInlineImage && inlined+a.Size <= maxInlineImages:
			if data, err := os.ReadFile(path); err == nil {
				blocks = append(blocks, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": mimeType})
				inlined += a.Size
			}
		case isTextType(mimeType) && a.Size <= maxInlineText:
			if data, err := os.ReadFile(path); err == nil && utf8.Valid(data) {
				blocks = append(blocks, textBlock(fmt.Sprintf("Contents of %s:\n%s", a.Name, data)))
			}
		}
		if req.supportsResourceLinks() {
			blocks = append(blocks, map[string]any{
				"type": "resource_link", "uri": link, "name": a.Name, "mimeType": mimeType, "size": a.Size,
				"description": "A result file. The link downloads it without credentials until " + until.UTC().Format(time.RFC3339) + ".",
			})
		}
	}
	return files, blocks
}

var extraMimeTypes = map[string]string{
	".wav": "audio/wav", ".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aiff": "audio/aiff", ".aif": "audio/aiff", ".flac": "audio/flac", ".ogg": "audio/ogg",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm", ".mkv": "video/x-matroska",
	".srt": "text/plain", ".vtt": "text/vtt", ".txt": "text/plain", ".log": "text/plain", ".md": "text/markdown", ".csv": "text/csv",
	".json": "application/json", ".yaml": "text/yaml", ".yml": "text/yaml", ".xml": "application/xml",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".zip": "application/zip", ".pdf": "application/pdf",
}

func mimeTypeFor(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := extraMimeTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if mediaType, _, err := mime.ParseMediaType(t); err == nil {
			return mediaType
		}
	}
	return "application/octet-stream"
}

func isInlineImage(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func isTextType(mimeType string) bool {
	return strings.HasPrefix(mimeType, "text/") || mimeType == "application/json" || mimeType == "application/xml"
}

func humanSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
}

// tailText keeps the end of a text, starting at a whole character and line.
func tailText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[len(text)-limit:]
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return cut
}

// --- The gateway's own tools ---

func stringArg(arguments map[string]json.RawMessage, name string) (string, bool) {
	var value string
	if err := json.Unmarshal(arguments[name], &value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

func (req *mcpRequest) jobWait(ctx context.Context, arguments map[string]json.RawMessage) (map[string]any, *rpcError) {
	jobID, ok := stringArg(arguments, "job_id")
	if !ok {
		return toolError("Give the job_id a tool answered with."), nil
	}
	wait := req.g.cfg.MCPWait
	if raw, ok := arguments["seconds"]; ok {
		var seconds float64
		if err := json.Unmarshal(raw, &seconds); err != nil || seconds < 0 {
			return toolError("seconds must be a number from 0 to %d.", int(maxJobWait/time.Second)), nil
		}
		wait = min(time.Duration(seconds*float64(time.Second)), maxJobWait)
	}
	var fullOutput bool
	if raw, ok := arguments["full_output"]; ok {
		_ = json.Unmarshal(raw, &fullOutput)
	}
	return req.waitAndRender(ctx, jobID, wait, fullOutput, false)
}

func (req *mcpRequest) jobCancel(ctx context.Context, arguments map[string]json.RawMessage) (map[string]any, *rpcError) {
	jobID, ok := stringArg(arguments, "job_id")
	if !ok {
		return toolError("Give the job_id a tool answered with."), nil
	}
	found, err := req.g.cancelJob(ctx, jobID, req.key.ID)
	if err != nil {
		req.g.logError(ctx, "cancel job", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	if !found {
		return toolError("There's no job %s for this key.", jobID), nil
	}
	view, err := req.g.store.GetJob(ctx, jobID, req.key.ID, false)
	if err != nil || view == nil {
		return map[string]any{"content": []any{textBlock("Job " + jobID + " was stopped.")}}, nil
	}
	if view.Status == StatusCancelled {
		return map[string]any{"content": []any{textBlock(fmt.Sprintf("Job %s was stopped.", jobID))}, "structuredContent": map[string]any{"job_id": jobID, "status": view.Status}}, nil
	}
	return map[string]any{"content": []any{textBlock(fmt.Sprintf("Job %s had already finished (%s), so nothing was stopped.", jobID, view.Status))}, "structuredContent": map[string]any{"job_id": jobID, "status": view.Status}}, nil
}

func (req *mcpRequest) status(ctx context.Context) (map[string]any, *rpcError) {
	rows, err := req.g.store.CapabilityRows(ctx, "")
	if err != nil {
		req.g.logError(ctx, "list capabilities", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	online := req.g.onlineNodes()
	type machine struct {
		Name   string            `json:"name"`
		Online bool              `json:"online"`
		Ready  []string          `json:"ready"`
		Not    map[string]string `json:"not_ready,omitempty"`
	}
	byNode := map[string]*machine{}
	var order []string
	for _, row := range rows {
		if !keyAllows(req.key, row.Name) {
			continue
		}
		m := byNode[row.NodeName]
		if m == nil {
			m = &machine{Name: row.NodeName, Online: online.ids[row.NodeID], Ready: []string{}, Not: map[string]string{}}
			byNode[row.NodeName] = m
			order = append(order, row.NodeName)
		}
		if row.NotReady == "" {
			m.Ready = append(m.Ready, row.Name)
		} else {
			m.Not[row.Name] = row.NotReady
		}
	}
	sort.Strings(order)
	var b strings.Builder
	machines := make([]*machine, 0, len(order))
	if len(order) == 0 {
		b.WriteString("No machine offers a tool this key may use.")
	}
	for _, name := range order {
		m := byNode[name]
		machines = append(machines, m)
		state := "online"
		if !m.Online {
			state = "offline, so none of its tools can run until it's back"
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Name, state)
		if len(m.Ready) > 0 {
			fmt.Fprintf(&b, "  ready: %s\n", strings.Join(m.Ready, ", "))
		}
		names := make([]string, 0, len(m.Not))
		for tool := range m.Not {
			names = append(names, tool)
		}
		sort.Strings(names)
		for _, tool := range names {
			fmt.Fprintf(&b, "  not ready: %s (%s)\n", tool, m.Not[tool])
		}
	}
	return map[string]any{"content": []any{textBlock(strings.TrimSpace(b.String()))}, "structuredContent": map[string]any{"machines": machines}}, nil
}

func (req *mcpRequest) fileUpload(ctx context.Context, arguments map[string]json.RawMessage) (map[string]any, *rpcError) {
	g := req.g
	name, ok := stringArg(arguments, "name")
	if !ok || !storedFileName.MatchString(name) {
		return toolError("name must use letters, digits, '.', '_' or '-' and start with a letter or digit, such as voice.wav."), nil
	}
	encoded, hasContent := stringArg(arguments, "content_base64")
	if !hasContent {
		link, until := g.uploadLink(req.base, req.key.ID, name)
		text := fmt.Sprintf("Upload the file with an HTTP PUT of its contents to this link, which works until %s:\n%s\n\nFor example: curl -sS -T path/to/%s '%s'\nThe answer is JSON whose id is the file id to give a tool.",
			until.UTC().Format(time.RFC3339), link, name, link)
		return map[string]any{"content": []any{textBlock(text)}, "structuredContent": map[string]any{"upload_url": link, "method": "PUT", "expires_at": until.UTC().Format(time.RFC3339)}}, nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(encoded); err != nil {
			return toolError("content_base64 isn't valid base64."), nil
		}
	}
	if len(data) > maxUploadContent {
		return toolError("The file is over %d MB. Call %s with just a name to get an upload link instead.", maxUploadContent>>20, toolFileUpload), nil
	}
	remaining, err := g.storageRemaining(ctx, "")
	if err != nil {
		g.logError(ctx, "measure storage", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	if int64(len(data)) > remaining {
		return toolError("The gateway's file storage is full."), nil
	}
	size, sum, tmpPath, err := g.receiveFile(ctx, bytes.NewReader(data))
	if err != nil {
		g.logError(ctx, "store uploaded file", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	defer os.Remove(tmpPath)
	fileID := newID("file_")
	target := filepath.Join(g.cfg.filesDir(), fileID)
	if err := os.Rename(tmpPath, target); err != nil {
		g.logError(ctx, "store uploaded file", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	view, err := g.store.AddInputFile(ctx, fileID, req.key.ID, name, sum, size, g.cfg.ArtifactTTL)
	if err != nil {
		os.Remove(target)
		g.logError(ctx, "record uploaded file", err)
		return nil, newRPCError(http.StatusInternalServerError, rpcInternalError, "internal error")
	}
	text := fmt.Sprintf("Stored %s (%s) as file id %s. Give that id in a tool's input_files.", name, humanSize(size), fileID)
	return map[string]any{"content": []any{textBlock(text)}, "structuredContent": view}, nil
}
