package e2e

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A 1x1 PNG, so a tool can make a picture without any image software.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

const mcpTools = `
  - name: img.make
    description: Draws a picture and writes notes about it.
    adapter: cli
    command: [/bin/sh, -c, 'printf "%s" "$1" | base64 -d > out.png; printf "notes for %s\n" "$2" > notes.txt; printf "drew %s" "$2"', sh, "` + tinyPNG + `", "{{input.subject}}"]
    input_schema: { type: object, required: [subject], properties: { subject: { type: string } } }
    outputs: { picture: out.png, notes: notes.txt }
    result: { from: stdout }
    timeout: 1m
  - name: file.copy
    description: Copies a file.
    adapter: cli
    command: ["@TOOL@", copy, "{{workdir}}/out.bin", "{{files.doc}}"]
    env: { TOOL_NODE: "@NODE@", MARKER_DIR: "@MARKERS@" }
    input_files: [doc]
    input_schema: { type: object }
    outputs: { copy: out.bin }
    timeout: 1m
  - name: slow.fail
    adapter: cli
    command: ["@TOOL@", fail, "{{workdir}}/out", "1"]
    env: { TOOL_NODE: "@NODE@", MARKER_DIR: "@MARKERS@" }
    input_schema: { type: object }
    timeout: 1m
`

// mcpClient talks to the gateway's /mcp as an MCP client does.
type mcpClient struct {
	t       *testing.T
	url     string
	key     string
	version string // "" sends modern requests.
	next    int
}

type rpcReply struct {
	status int
	header http.Header
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	} `json:"error"`
}

// request builds a request the way a modern or older client would.
func (c *mcpClient) request(method string, params map[string]any) *http.Request {
	c.t.Helper()
	c.next++
	if params == nil {
		params = map[string]any{}
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json, text/event-stream")
	header.Set("Authorization", "Bearer "+c.key)
	if c.version == "" {
		meta, _ := params["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["io.modelcontextprotocol/protocolVersion"] = "2026-07-28"
		meta["io.modelcontextprotocol/clientCapabilities"] = map[string]any{}
		meta["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": "e2e", "version": "1"}
		params["_meta"] = meta
		header.Set("MCP-Protocol-Version", "2026-07-28")
		header.Set("Mcp-Method", method)
		if name, ok := params["name"].(string); ok {
			header.Set("Mcp-Name", name)
		}
	} else if method != "initialize" {
		header.Set("MCP-Protocol-Version", c.version)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	req, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header = header
	return req
}

func (c *mcpClient) send(req *http.Request) rpcReply {
	c.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	reply := rpcReply{status: resp.StatusCode, header: resp.Header}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &reply); err != nil {
			c.t.Fatalf("%s: %s", err, raw)
		}
	}
	return reply
}

func (c *mcpClient) call(method string, params map[string]any) rpcReply {
	c.t.Helper()
	return c.send(c.request(method, params))
}

// tool calls a tool and fails the test on a protocol error.
func (c *mcpClient) tool(name string, args map[string]any) toolResult {
	c.t.Helper()
	reply := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if reply.Error != nil || reply.status != 200 {
		c.t.Fatalf("%s: status %d, error %+v", name, reply.status, reply.Error)
	}
	return asToolResult(c.t, reply.Result)
}

type toolResult struct {
	Content    []map[string]any `json:"content"`
	Structured map[string]any   `json:"structuredContent"`
	IsError    bool             `json:"isError"`
	ResultType string           `json:"resultType"`
}

func asToolResult(t *testing.T, raw map[string]any) toolResult {
	t.Helper()
	data, _ := json.Marshal(raw)
	var out toolResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r toolResult) text() string {
	var parts []string
	for _, block := range r.Content {
		if block["type"] == "text" {
			parts = append(parts, block["text"].(string))
		}
	}
	return strings.Join(parts, "\n")
}

func (r toolResult) blocks(kind string) []map[string]any {
	var out []map[string]any
	for _, block := range r.Content {
		if block["type"] == kind {
			out = append(out, block)
		}
	}
	return out
}

func (r toolResult) file(t *testing.T, name string) map[string]any {
	t.Helper()
	files, _ := r.Structured["files"].([]any)
	for _, f := range files {
		if m := f.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no file %s in %v", name, r.Structured)
	return nil
}

func download(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestMCPServesWorkerTools(t *testing.T) {
	h := newHarness(t, "--mcp-wait", "4s")
	w := h.startWorker("mac", h.url, false, mcpTools)
	for _, name := range []string{"tts.synthesize", "img.make", "file.copy", "slow.fail"} {
		h.waitReady(name, 1)
	}

	// An older client opens with a handshake, and is answered without a session.
	old := &mcpClient{t: t, url: h.url + "/mcp", key: h.apiKey, version: "2025-06-18"}
	init := old.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "old", "version": "1"}})
	if init.Result["protocolVersion"] != "2025-06-18" || init.header.Get("Mcp-Session-Id") != "" || init.Result["instructions"] == nil {
		t.Fatalf("initialize = %+v %v", init.Result, init.header)
	}
	note, _ := http.NewRequest("POST", h.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	note.Header.Set("Authorization", "Bearer "+h.apiKey)
	note.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(note); err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: %v %v", resp, err)
	}
	listed := old.call("tools/list", nil)
	var names []string
	var copySchema map[string]any
	for _, raw := range listed.Result["tools"].([]any) {
		tool := raw.(map[string]any)
		names = append(names, tool["name"].(string))
		if tool["name"] == "file_copy" {
			copySchema = tool["inputSchema"].(map[string]any)
		}
	}
	if got, want := strings.Join(names, " "), "file_copy img_make slow_fail tts_synthesize fleet_job_wait fleet_job_cancel fleet_status fleet_file_upload"; got != want {
		t.Fatalf("tools = %s", got)
	}
	if req, _ := copySchema["required"].([]any); len(req) != 1 || req[0] != "input_files" {
		t.Fatalf("file_copy schema = %v", copySchema)
	}
	if _, ok := listed.Result["resultType"]; ok {
		t.Fatal("an older client got a modern field")
	}
	// Older clients still get files as links, in text.
	oldPicture := old.tool("img_make", map[string]any{"subject": "an old cat"})
	if len(oldPicture.blocks("resource_link")) == 0 || !strings.Contains(oldPicture.text(), "/v1/links/") {
		t.Fatalf("2025-06-18 result = %+v", oldPicture)
	}
	ancient := &mcpClient{t: t, url: h.url + "/mcp", key: h.apiKey, version: "2025-03-26"}
	if r := ancient.tool("img_make", map[string]any{"subject": "x"}); len(r.blocks("resource_link")) != 0 || !strings.Contains(r.text(), "/v1/links/") {
		t.Fatalf("2025-03-26 got resource links, which it doesn't know: %+v", r.Content)
	}

	c := &mcpClient{t: t, url: h.url + "/mcp", key: h.apiKey}
	discover := c.call("server/discover", nil)
	if discover.Result["resultType"] != "complete" || !strings.Contains(fmt.Sprint(discover.Result["supportedVersions"]), "2026-07-28") {
		t.Fatalf("discover = %+v", discover.Result)
	}

	// Pictures come inline; text files too; every file has a link that needs no key.
	picture := c.tool("img_make", map[string]any{"subject": "a cat"})
	if picture.IsError || picture.ResultType != "complete" || !strings.HasPrefix(picture.text(), "drew a cat") {
		t.Fatalf("img_make = %+v", picture)
	}
	images := picture.blocks("image")
	if len(images) != 1 || images[0]["data"] != tinyPNG || images[0]["mimeType"] != "image/png" {
		t.Fatalf("images = %v", images)
	}
	if !strings.Contains(picture.text(), "Contents of notes.txt:\nnotes for a cat") {
		t.Fatalf("notes weren't inline: %s", picture.text())
	}
	if links := picture.blocks("resource_link"); len(links) != 2 {
		t.Fatalf("resource links = %v", links)
	}
	notes := picture.file(t, "notes.txt")
	if status, body := download(t, notes["url"].(string)); status != 200 || body != "notes for a cat\n" {
		t.Fatalf("download: %d %q", status, body)
	}
	tampered := strings.Replace(notes["url"].(string), "/v1/links/", "/v1/links/x", 1)
	if status, _ := download(t, tampered); status != http.StatusForbidden {
		t.Fatalf("a changed link worked: %d", status)
	}

	// One tool's result file can be another's input.
	copied := c.tool("file_copy", map[string]any{"input_files": map[string]any{"doc": notes["id"]}})
	if status, body := download(t, copied.file(t, "copy.bin")["url"].(string)); copied.IsError || status != 200 || body != "notes for a cat\n" {
		t.Fatalf("chained copy: %+v %d %q", copied, status, body)
	}

	// Uploads: small files inline, any size through a link.
	uploaded := c.tool("fleet_file_upload", map[string]any{"name": "hello.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("hello from the agent"))})
	fileID := uploaded.Structured["id"].(string)
	copied = c.tool("file_copy", map[string]any{"input_files": map[string]any{"doc": fileID}})
	if _, body := download(t, copied.file(t, "copy.bin")["url"].(string)); body != "hello from the agent" {
		t.Fatalf("uploaded copy = %q", body)
	}
	linkResult := c.tool("fleet_file_upload", map[string]any{"name": "big.bin"})
	put, _ := http.NewRequest("PUT", linkResult.Structured["upload_url"].(string), strings.NewReader("sent by curl"))
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&stored)
	resp.Body.Close()
	copied = c.tool("file_copy", map[string]any{"input_files": map[string]any{"doc": stored.ID}})
	if _, body := download(t, copied.file(t, "copy.bin")["url"].(string)); body != "sent by curl" {
		t.Fatalf("link upload copy = %q (%d)", body, resp.StatusCode)
	}
	if bad := c.tool("file_copy", map[string]any{"input_files": map[string]any{"doc": "file_nope"}}); !bad.IsError {
		t.Fatalf("a missing file was accepted: %+v", bad)
	}

	// A failure is an error result the agent can read, with what was printed.
	failed := c.tool("slow_fail", nil)
	if !failed.IsError || !strings.Contains(failed.text(), "the engine exploded") || failed.Structured["exit_code"] != float64(3) {
		t.Fatalf("slow_fail = %+v", failed)
	}
	// Input the tool's schema refuses is an error result too.
	if bad := c.tool("tts_synthesize", map[string]any{"text": ""}); !bad.IsError || !strings.Contains(bad.text(), "INVALID_INPUT") {
		t.Fatalf("bad input = %+v", bad)
	}

	// A long job answers with its ID, and fleet_job_wait gets the result.
	long := c.tool("tts_synthesize", map[string]any{"text": "a long one", "steps": 24})
	jobID, _ := long.Structured["job_id"].(string)
	if long.IsError || long.Structured["status"] == "succeeded" || !strings.Contains(long.text(), "fleet_job_wait") || jobID == "" {
		t.Fatalf("long job = %+v", long)
	}
	done := c.tool("fleet_job_wait", map[string]any{"job_id": jobID, "seconds": 30})
	if done.Structured["status"] != "succeeded" || len(done.Structured["files"].([]any)) != 1 {
		t.Fatalf("waited = %+v", done)
	}
	if !strings.Contains(done.text(), "progress: 100%") {
		t.Fatalf("output missing: %s", done.text())
	}

	// With a progress token and a stream, progress arrives as it happens.
	req := c.request("tools/call", map[string]any{"name": "tts_synthesize", "arguments": map[string]any{"text": "streamed", "steps": 8}, "_meta": map[string]any{"progressToken": "p1"}})
	events := readSSE(t, req, -1)
	var progress int
	var final map[string]any
	for _, ev := range events {
		if ev["method"] == "notifications/progress" {
			progress++
			if ev["params"].(map[string]any)["progressToken"] != "p1" {
				t.Fatalf("progress = %v", ev)
			}
		}
		if ev["result"] != nil {
			final = ev["result"].(map[string]any)
		}
	}
	if progress == 0 || final == nil || asToolResult(t, final).Structured["status"] != "succeeded" {
		t.Fatalf("stream: %d progress events, final %v", progress, final)
	}

	// Stopping a job.
	long = c.tool("tts_synthesize", map[string]any{"text": "stop me", "steps": 200})
	stopped := c.tool("fleet_job_cancel", map[string]any{"job_id": long.Structured["job_id"]})
	if !strings.Contains(stopped.text(), "was stopped") || stopped.Structured["status"] != "cancelled" {
		t.Fatalf("cancel = %+v", stopped)
	}

	// A modern client that hangs up has cancelled its call, and the job stops.
	os.Remove(filepath.Join(w.markerDir, "pid"))
	req = c.request("tools/call", map[string]any{"name": "tts_synthesize", "arguments": map[string]any{"text": "hang up", "steps": 200}, "_meta": map[string]any{"progressToken": 7}})
	readSSE(t, req, 1)
	var pid int
	eventually(t, 10*time.Second, "the tool to start", func() bool {
		raw, _ := os.ReadFile(filepath.Join(w.markerDir, "pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		return pid > 0
	})
	eventually(t, 15*time.Second, "the abandoned job's tool to stop", func() bool {
		return syscall.Kill(pid, 0) != nil
	})

	if status := c.tool("fleet_status", nil); !strings.Contains(status.text(), "mac: online") || !strings.Contains(status.text(), "img.make") {
		t.Fatalf("status = %s", status.text())
	}

	// With the machine gone, a call says so at once and starts nothing.
	w.stop()
	eventually(t, 10*time.Second, "the worker to be offline", func() bool {
		return strings.Contains(c.tool("fleet_status", nil).text(), "mac: offline")
	})
	offline := c.tool("img_make", map[string]any{"subject": "nobody"})
	if !offline.IsError || !strings.Contains(offline.text(), "mac is offline") || offline.Structured["job_id"] != nil {
		t.Fatalf("offline = %+v", offline)
	}
}

// readSSE sends a request and reads its event stream. With stopAfter >= 0, it
// hangs up after that many events.
func readSSE(t *testing.T, req *http.Request, stopAfter int) []map[string]any {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Content-Type %q: %s", ct, body)
	}
	var events []map[string]any
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
		if stopAfter >= 0 && len(events) >= stopAfter {
			return events
		}
		if ev["result"] != nil || ev["error"] != nil {
			return events
		}
	}
	return events
}

func TestMCPRefusesWhatItShould(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, mcpTools)
	h.waitReady("img.make", 1)
	c := &mcpClient{t: t, url: h.url + "/mcp", key: h.apiKey}

	expect := func(what string, reply rpcReply, status, code int) {
		t.Helper()
		if reply.status != status || reply.Error == nil || reply.Error.Code != code {
			t.Errorf("%s: status %d, error %+v; want %d and %d", what, reply.status, reply.Error, status, code)
		}
	}
	expect("unknown tool", c.call("tools/call", map[string]any{"name": "nope"}), 200, -32602)
	expect("unknown method", c.call("resources/list", nil), 404, -32601)

	req := c.request("tools/call", map[string]any{"name": "img_make", "arguments": map[string]any{"subject": "x"}})
	req.Header.Set("Mcp-Name", "slow_fail")
	expect("name header differs", c.send(req), 400, -32020)
	req = c.request("tools/list", nil)
	req.Header.Set("Mcp-Method", "tools/call")
	expect("method header differs", c.send(req), 400, -32020)
	req = c.request("tools/list", nil)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	expect("version header differs", c.send(req), 400, -32020)

	future := &mcpClient{t: t, url: h.url + "/mcp", key: h.apiKey, version: "1900-01-01"}
	reply := future.call("tools/list", nil)
	expect("unknown version", reply, 400, -32022)
	if reply.Error != nil && !strings.Contains(fmt.Sprint(reply.Error.Data["supported"]), "2026-07-28") {
		t.Errorf("supported versions not given: %v", reply.Error.Data)
	}
	// A modern request must say what it supports.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	raw, _ := http.NewRequest("POST", h.url+"/mcp", strings.NewReader(body))
	raw.Header = c.request("tools/list", nil).Header
	expect("no client capabilities", c.send(raw), 400, -32602)

	noKey := c.request("tools/list", nil)
	noKey.Header.Del("Authorization")
	if r := c.send(noKey); r.status != 401 || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("no key: %d %v", r.status, r.header)
	}
	wrongKey := c.request("tools/list", nil)
	wrongKey.Header.Set("Authorization", "Bearer nope")
	if r := c.send(wrongKey); r.status != 401 {
		t.Errorf("wrong key: %d", r.status)
	}
	dashboard := c.request("tools/list", nil)
	dashboard.Header.Set("Authorization", "Bearer "+h.credential("key", "add", "ops", "--dashboard"))
	if r := c.send(dashboard); r.status != 403 {
		t.Errorf("dashboard key: %d", r.status)
	}
	fromPage := c.request("tools/list", nil)
	fromPage.Header.Set("Origin", "https://evil.example")
	if r := c.send(fromPage); r.status != 403 {
		t.Errorf("other origin: %d", r.status)
	}
	sameSite := c.request("tools/list", nil)
	sameSite.Header.Set("Origin", h.url)
	if r := c.send(sameSite); r.status != 200 {
		t.Errorf("own origin: %d", r.status)
	}
	for _, method := range []string{"GET", "DELETE"} {
		r, _ := http.NewRequest(method, h.url+"/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+h.apiKey)
		resp, err := http.DefaultClient.Do(r)
		if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: %v %v", method, resp, err)
		}
	}
	batch, _ := http.NewRequest("POST", h.url+"/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`))
	batch.Header = c.request("ping", nil).Header
	if r := c.send(batch); r.status != 400 {
		t.Errorf("batch: %d", r.status)
	}

	// A key sees only the tools it may call.
	narrow := &mcpClient{t: t, url: h.url + "/mcp", key: h.credential("key", "add", "images", "--capabilities", "img.*")}
	var names []string
	for _, tool := range narrow.call("tools/list", nil).Result["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, " "); got != "img_make fleet_job_wait fleet_job_cancel fleet_status" {
		t.Errorf("narrow key tools = %s", got)
	}
	expect("a tool the key can't use", narrow.call("tools/call", map[string]any{"name": "slow_fail"}), 200, -32602)

	// Links stop working when their key is revoked.
	picture := narrow.tool("img_make", map[string]any{"subject": "x"})
	link := picture.file(t, "picture.png")["url"].(string)
	if status, _ := download(t, link); status != 200 {
		t.Fatalf("link: %d", status)
	}
	h.credential("key", "revoke", "images")
	if status, _ := download(t, link); status != http.StatusForbidden {
		t.Errorf("a revoked key's link still works: %d", status)
	}
}
