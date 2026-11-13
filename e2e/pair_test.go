package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// unpairedWorker starts a worker the way the installer leaves one that wasn't
// paired: no gateway, no token, and its page on.
func (h *harness) unpairedWorker(name string) (*worker, *uiClient) {
	h.t.Helper()
	dir := filepath.Join(h.dir, "worker-"+name)
	w := &worker{h: h, name: name, config: filepath.Join(dir, "worker.yaml"), stateDir: filepath.Join(dir, "state")}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	manifest := fmt.Sprintf(`# Pairing this machine fills in gateway_url and node.name.
gateway_url: ""
token_file: token
state_dir: state
ui_listen: 127.0.0.1:0

node:
  name: %s
  max_jobs: 2

capabilities:
  - name: %s.ping
    description: Answers pong.
    adapter: cli
    command: [/bin/sh, -c, 'printf "pong from %%s" "$1"', sh, "%s"]
    input_schema: { type: object, additionalProperties: false }
    result: { from: stdout }
    timeout: 1m
    test: { input: {} }
`, name, name, name)
	if err := os.WriteFile(w.config, []byte(manifest), 0o600); err != nil {
		h.t.Fatal(err)
	}
	w.start()
	h.t.Cleanup(w.stop)
	eventually(h.t, 10*time.Second, "the page's sign-in file", func() bool {
		_, err := os.Stat(filepath.Join(w.stateDir, "ui.json"))
		return err == nil
	})
	out, err := exec.Command(workerBin, "ui", "--config", w.config, "--no-open").Output()
	if err != nil {
		h.t.Fatalf("fleet-worker ui: %v", err)
	}
	link, err := url.Parse(strings.TrimSpace(string(out)))
	if err != nil || !strings.HasPrefix(link.Fragment, "code=") {
		h.t.Fatalf("link = %q", out)
	}
	base := link.Scheme + "://" + link.Host + "/"
	body, _ := json.Marshal(map[string]string{"code": strings.TrimPrefix(link.Fragment, "code=")})
	resp, err := http.Post(base+"api/sign-in", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct{ Token string }
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.Token == "" {
		h.t.Fatalf("sign in: %d %v", resp.StatusCode, err)
	}
	return w, &uiClient{t: h.t, base: base, token: got.Token}
}

var shownCode = regexp.MustCompile(`(?m)^ +([A-Z2-9]{4}-[A-Z2-9]{4})$`)

// serverPairing runs what a person runs on the server to see pairing codes.
func (h *harness) serverPairing(flags ...string) string {
	h.t.Helper()
	args := append([]string{"pair", "--data", filepath.Join(h.dir, "gateway")}, flags...)
	out, err := exec.Command(gatewayBin, args...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("fleet-gateway pair: %v\n%s", err, out)
	}
	return string(out)
}

func steps(report map[string]any) []string {
	var out []string
	list, _ := report["steps"].([]any)
	for _, raw := range list {
		step := raw.(map[string]any)
		out = append(out, fmt.Sprintf("%s: %s: %v", step["name"], step["state"], step["detail"]))
	}
	return out
}

// A machine installed with no gateway is paired from its page with the code the
// server shows, connects without a restart, and can then check for itself that
// agents reach its tools through MCP.
func TestPairingFromThePageWithTheServersCode(t *testing.T) {
	h := newHarness(t)
	w, page := h.unpairedWorker("fresh")

	_, status := page.call("GET", "api/status", nil)
	if status["paired"] != false || status["connected"] != false || status["gateway"] != "" {
		t.Fatalf("status before pairing = %v", status)
	}
	// Before pairing, the connection test says what's wrong rather than failing oddly.
	_, report := page.call("POST", "api/mcp-test", map[string]any{})
	if got := steps(report); report["ok"] != false || len(got) != 1 || !strings.Contains(got[0], "isn't paired with a gateway yet") {
		t.Fatalf("connection test before pairing = %v", got)
	}
	if out := h.serverPairing("--wait", "0"); !strings.Contains(out, "No machine is asking to pair") {
		t.Fatalf("before anyone asks, the server shows:\n%s", out)
	}

	// Someone types the server's address, as a person would, with no scheme.
	if code, body := page.call("POST", "api/pair/start", map[string]any{"gateway": "not an address", "name": ""}); code != http.StatusBadRequest {
		t.Fatalf("a bad address: %d %v", code, body)
	}
	if code, body := page.call("POST", "api/pair/start", map[string]any{"gateway": "http://127.0.0.1:1", "name": ""}); code != http.StatusBadGateway || !strings.Contains(fmt.Sprint(body["error"]), "refused the connection") {
		t.Fatalf("an address with no gateway: %d %v", code, body)
	}
	if code, body := page.call("POST", "api/pair/confirm", map[string]any{"code": "AAAA-AAAA"}); code != http.StatusConflict {
		t.Fatalf("a code before asking: %d %v", code, body)
	}
	code, started := page.call("POST", "api/pair/start", map[string]any{"gateway": h.url + "/", "name": ""})
	if code != http.StatusOK || started["name"] != "fresh" || started["gateway"] != h.url {
		t.Fatalf("start: %d %v", code, started)
	}
	if _, has := started["code"]; has {
		t.Fatal("the machine was sent its own code")
	}

	// The code appears only on the server.
	shown := h.serverPairing("--wait", "0")
	match := shownCode.FindStringSubmatch(shown)
	if match == nil || !strings.Contains(shown, "fresh (linux/") || !strings.Contains(shown, "at 127.0.0.1 is asking to pair") {
		t.Fatalf("the server shows:\n%s", shown)
	}
	pairingCode := match[1]

	code, wrong := page.call("POST", "api/pair/confirm", map[string]any{"code": "AAAA-AAAA"})
	if code != http.StatusForbidden || wrong["attempts_left"] != float64(4) {
		t.Fatalf("a wrong code: %d %v", code, wrong)
	}
	code, paired := page.call("POST", "api/pair/confirm", map[string]any{"code": strings.ToLower(pairingCode)})
	if code != http.StatusOK || paired["node"] != "fresh" || paired["gateway"] != h.url {
		t.Fatalf("the right code: %d %v", code, paired)
	}
	if _, has := paired["token"]; has {
		t.Fatal("the page was sent the node token")
	}

	eventually(t, 15*time.Second, "the paired worker to connect", func() bool {
		_, status = page.call("GET", "api/status", nil)
		return status["connected"] == true
	})
	if status["paired"] != true || status["gateway"] != h.url || status["node"] != "fresh" {
		t.Fatalf("status after pairing = %v", status)
	}
	h.waitForCapability("fresh.ping", 1)
	if j := h.runAndWait("fresh.ping", map[string]any{}); string(j.Result) != `"pong from fresh"` {
		t.Fatalf("an agent's job: %s %+v", j.Result, j.Error)
	}

	// The server says it paired and connected, which is what the gateway's
	// installer waits for.
	if out := h.serverPairing("--wait", "20s", "--until-connected"); !strings.Contains(out, "fresh is connected, offering 1 tool.") {
		t.Fatalf("the server shows:\n%s", out)
	}
	if out := h.serverPairing("--wait", "0"); strings.Contains(out, pairingCode) {
		t.Fatalf("the used code is still shown:\n%s", out)
	}

	// The pairing is on disk, with the rest of worker.yaml untouched.
	saved, _ := os.ReadFile(w.config)
	if !strings.Contains(string(saved), "gateway_url: "+h.url+"\n") || !strings.Contains(string(saved), "# Pairing this machine fills in") {
		t.Fatalf("worker.yaml:\n%s", saved)
	}
	token, _ := os.ReadFile(filepath.Join(filepath.Dir(w.config), "token"))
	if !strings.HasPrefix(string(token), "fn_") {
		t.Fatalf("token file = %q", token)
	}

	// The connection test does what an agent does, over the same address.
	code, report = page.call("POST", "api/mcp-test", map[string]any{})
	got := steps(report)
	if code != http.StatusOK || report["ok"] != true || len(got) != 4 {
		t.Fatalf("connection test: %d\n%s", code, strings.Join(got, "\n"))
	}
	for i, want := range []string{
		"Reach the gateway: ok",
		"Open an MCP session: ok: fleet-gateway",
		"List this machine's tools: ok: An agent would see 1 tool: fresh.ping.",
		"Call a tool through MCP: ok: fresh.ping ran on this machine and answered:\npong from fresh",
	} {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("step %d = %q, want it to start %q", i+1, got[i], want)
		}
	}
	if report["mcp_url"] != h.url+"/mcp" {
		t.Errorf("mcp_url = %v", report["mcp_url"])
	}

	// It survives a restart, like any paired worker.
	w.stop()
	w.start()
	h.waitForCapability("fresh.ping", 1)
}

// A machine can instead be connected with a token made on the server, pasted
// into its page. The gateway says which machine the token is for.
func TestPairingFromThePageWithAKey(t *testing.T) {
	h := newHarness(t)
	_, page := h.unpairedWorker("anything")
	token := h.credential("node", "add", "keyed")

	if code, body := page.call("POST", "api/pair/key", map[string]any{"gateway": h.url, "key": "fn_wrong"}); code != http.StatusBadGateway || !strings.Contains(fmt.Sprint(body["error"]), "doesn't know that key") {
		t.Fatalf("a wrong key: %d %v", code, body)
	}
	code, paired := page.call("POST", "api/pair/key", map[string]any{"gateway": h.url, "key": " " + token + "\n"})
	if code != http.StatusOK || paired["node"] != "keyed" {
		t.Fatalf("the right key: %d %v", code, paired)
	}
	var status map[string]any
	eventually(t, 15*time.Second, "the worker to connect", func() bool {
		_, status = page.call("GET", "api/status", nil)
		return status["connected"] == true
	})
	if status["node"] != "keyed" {
		t.Fatalf("status = %v", status)
	}
	// The tool keeps the name it was declared with; only the machine was renamed.
	h.waitForCapability("anything.ping", 1)
}

// The key a machine tests its connection with reaches that machine's tools and
// nothing else.
func TestConnectionTestKeyReachesOnlyItsOwnMachine(t *testing.T) {
	h := newHarness(t)
	mine := h.startWorker("mine", h.url, false)
	h.startWorker("other", h.url, false, `
  - name: other.only
    adapter: cli
    command: [/bin/echo, secret]
    input_schema: { type: object }
`)
	h.waitForCapability("other.only", 1)
	h.waitForCapability("tts.synthesize", 2)

	if status, _ := h.request("POST", "/v1/worker/mcp-test", "fn_wrong", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("a test key without a node token: %d", status)
	}
	if status, _ := h.request("POST", "/v1/worker/mcp-test", h.apiKey, map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("a test key for an agent key: %d", status)
	}
	status, raw := h.request("POST", "/v1/worker/mcp-test", mine.token, map[string]any{})
	var issued struct {
		Key    string `json:"key"`
		MCPURL string `json:"mcp_url"`
	}
	if err := json.Unmarshal(raw, &issued); err != nil || status != http.StatusOK || !strings.HasPrefix(issued.Key, "ft_") {
		t.Fatalf("issue: %d %s", status, raw)
	}

	client := &mcpClient{t: t, url: h.url + "/mcp", key: issued.Key, version: "2025-11-25"}
	list := client.call("tools/list", nil)
	var titles []string
	tools, _ := list.Result["tools"].([]any)
	for _, raw := range tools {
		titles = append(titles, raw.(map[string]any)["title"].(string))
	}
	joined := strings.Join(titles, ",")
	if list.status != 200 || strings.Contains(joined, "other.only") || !strings.Contains(joined, "tts.synthesize") || strings.Contains(joined, "fleet") {
		t.Fatalf("the test key lists: %s (status %d)", joined, list.status)
	}
	if reply := client.call("tools/call", map[string]any{"name": "other_only", "arguments": map[string]any{}}); reply.Error == nil || !strings.Contains(reply.Error.Message, "Unknown tool") {
		t.Fatalf("calling another machine's tool: %+v %v", reply.Error, reply.Result)
	}
	if reply := client.call("tools/call", map[string]any{"name": "fleet_status", "arguments": map[string]any{}}); reply.Error == nil {
		t.Fatalf("the test key can see the whole fleet: %v", reply.Result)
	}
	// A tool both machines offer runs on the one that asked.
	before := mine.runs()
	result := client.tool("tts_synthesize", map[string]any{"text": "from the test", "steps": 1})
	if result.IsError || mine.runs() != before+1 {
		t.Fatalf("the call ran %d times on its own machine: %s", mine.runs()-before, result.text())
	}

	// It isn't an agent key, and a newer one replaces it.
	if status, _ := h.request("GET", "/v1/capabilities", issued.Key, nil); status != http.StatusUnauthorized {
		t.Fatalf("the test key on the agent API: %d", status)
	}
	if status, _ := h.request("POST", "/v1/worker/mcp-test", mine.token, map[string]any{}); status != http.StatusOK {
		t.Fatalf("a second test: %d", status)
	}
	if reply := client.call("tools/list", nil); reply.status != http.StatusUnauthorized {
		t.Fatalf("the first key after a second test began: %d", reply.status)
	}
}
