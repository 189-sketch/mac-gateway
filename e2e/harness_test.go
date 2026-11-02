// Package e2e tests the real gateway and worker binaries as separate processes.
// A TCP proxy between them simulates pulling the network cable.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var gatewayBin, workerBin, fleetBin, fakeTool string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fleet-e2e-bin")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	gatewayBin = filepath.Join(dir, "fleet-gateway")
	workerBin = filepath.Join(dir, "fleet-worker")
	fleetBin = filepath.Join(dir, "fleet")
	for bin, pkg := range map[string]string{gatewayBin: "mactunnel/cmd/fleet-gateway", workerBin: "mactunnel/cmd/fleet-worker", fleetBin: "mactunnel/cmd/fleet"} {
		if out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
			panic(fmt.Sprintf("build %s: %v\n%s", pkg, err, out))
		}
	}
	fakeTool, err = filepath.Abs("testdata/fake-tool.sh")
	if err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// harness owns one gateway and any number of workers.
type harness struct {
	t       *testing.T
	dir     string
	port    int
	url     string
	apiKey  string
	gateway *exec.Cmd
	logs    *os.File
	flags   []string
}

func newHarness(t *testing.T, extraFlags ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	logs, err := os.Create(filepath.Join(dir, "processes.log"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, port: freePort(t), logs: logs}
	h.url = fmt.Sprintf("http://127.0.0.1:%d", h.port)
	h.flags = append([]string{
		"--listen", fmt.Sprintf("127.0.0.1:%d", h.port),
		"--data", filepath.Join(dir, "gateway"),
		"--heartbeat", "200ms",
		"--lease", "3s",
		"--lost-grace", "60s",
		"--tick", "100ms",
	}, extraFlags...)
	t.Cleanup(func() {
		if t.Failed() {
			if raw, err := os.ReadFile(logs.Name()); err == nil {
				t.Logf("process logs:\n%s", raw)
			}
		}
		logs.Close()
	})
	h.apiKey = h.credential("key", "add", "agent", "--capabilities", "*")
	h.startGateway()
	t.Cleanup(h.stopGateway)
	return h
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (h *harness) credential(kind, action, name string, flags ...string) string {
	h.t.Helper()
	args := append([]string{kind, action, name, "--data", filepath.Join(h.dir, "gateway")}, flags...)
	out, err := exec.Command(gatewayBin, args...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *harness) startGateway() {
	h.t.Helper()
	cmd := exec.Command(gatewayBin, append([]string{"serve"}, h.flags...)...)
	cmd.Stdout, cmd.Stderr = h.logs, h.logs
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.gateway = cmd
	eventually(h.t, 10*time.Second, "gateway to start", func() bool {
		resp, err := http.Get(h.url + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
}

func (h *harness) stopGateway() {
	if h.gateway != nil && h.gateway.Process != nil {
		h.gateway.Process.Signal(syscall.SIGTERM)
		h.gateway.Wait()
		h.gateway = nil
	}
}

func (h *harness) killGateway() {
	h.gateway.Process.Kill()
	h.gateway.Wait()
	h.gateway = nil
}

// proxy forwards TCP to the gateway and can simulate an unplugged network.
type proxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	down     bool
	conns    map[net.Conn]bool
	// bytesPerSecond limits each direction of each connection when above zero, so
	// that a cut can land in the middle of a transfer.
	bytesPerSecond atomic.Int64
}

func (h *harness) newProxy() *proxy {
	h.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	p := &proxy{listener: l, target: fmt.Sprintf("127.0.0.1:%d", h.port), conns: map[net.Conn]bool{}}
	go p.serve()
	h.t.Cleanup(func() { l.Close(); p.cut() })
	return p
}

func (p *proxy) url() string { return "http://" + p.listener.Addr().String() }

func (p *proxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := p.down
		p.mu.Unlock()
		if down {
			client.Close()
			continue
		}
		upstream, err := net.Dial("tcp", p.target)
		if err != nil {
			client.Close()
			continue
		}
		p.mu.Lock()
		p.conns[client], p.conns[upstream] = true, true
		p.mu.Unlock()
		go p.pipe(client, upstream)
		go p.pipe(upstream, client)
	}
}

func (p *proxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
			if limit := p.bytesPerSecond.Load(); limit > 0 {
				time.Sleep(time.Duration(n) * time.Second / time.Duration(limit))
			}
		}
		if err != nil {
			return
		}
	}
}

// cut drops every connection and refuses new ones until restore.
func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]bool{}
}

func (p *proxy) restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = false
}

// worker is one running fleet-worker with its own state and marker directories.
type worker struct {
	h          *harness
	name       string
	token      string
	config     string
	stateDir   string
	markerDir  string
	cmd        *exec.Cmd
	tokenInEnv bool
}

const defaultCapabilities = `
  - name: tts.synthesize
    adapter: cli
    command: [%[1]s, normal, "{{workdir}}/out.wav", "{{input.steps}}"]
    stdin: "{{input.text}}"
    env: { TOOL_NODE: %[2]s, MARKER_DIR: %[3]s }
    input_schema:
      type: object
      required: [text, steps]
      additionalProperties: false
      properties:
        text: { type: string, minLength: 1 }
        steps: { type: integer, minimum: 1 }
    outputs: { audio: out.wav }
    progress: { regex: "progress: (?P<percent>[0-9]+)%%" }
    timeout: 1m
    retry_if_lost: %[4]t
`

// startWorker writes a manifest for the given capabilities and starts a worker.
// With retry set, the default capability opts in to automatic reruns.
func (h *harness) startWorker(name, gatewayURL string, retry bool, extraCapabilities ...string) *worker {
	h.t.Helper()
	dir := filepath.Join(h.dir, "worker-"+name)
	w := &worker{
		h: h, name: name,
		token:     h.credential("node", "add", name),
		config:    filepath.Join(dir, "worker.yaml"),
		stateDir:  filepath.Join(dir, "state"),
		markerDir: filepath.Join(dir, "markers"),
	}
	if err := os.MkdirAll(w.markerDir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	var manifest strings.Builder
	fmt.Fprintf(&manifest, "gateway_url: %s\ntoken_file: token\nstate_dir: state\nupload_retry_window: 2m\nnode:\n  name: %s\n  max_jobs: 4\n  labels: { name: %s }\n  resources: { heavy: 1 }\ncapabilities:",
		gatewayURL, name, name)
	fmt.Fprintf(&manifest, defaultCapabilities, fakeTool, name, w.markerDir, retry)
	for _, extra := range extraCapabilities {
		manifest.WriteString(strings.NewReplacer("@TOOL@", fakeTool, "@NODE@", name, "@MARKERS@", w.markerDir).Replace(extra))
	}
	if err := os.WriteFile(w.config, []byte(manifest.String()), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(w.token+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	w.start()
	h.t.Cleanup(w.stop)
	return w
}

func (w *worker) start() {
	w.h.t.Helper()
	cmd := exec.Command(workerBin, "--config", w.config)
	cmd.Stdout, cmd.Stderr = w.h.logs, w.h.logs
	cmd.Env = append(os.Environ(), "FLEET_SENTINEL=present")
	if w.tokenInEnv {
		cmd.Env = append(cmd.Env, "FLEET_NODE_TOKEN="+w.token)
	}
	if err := cmd.Start(); err != nil {
		w.h.t.Fatal(err)
	}
	w.cmd = cmd
}

func (w *worker) stop() {
	if w.cmd != nil {
		w.cmd.Process.Signal(syscall.SIGTERM)
		w.cmd.Wait()
		w.cmd = nil
	}
}

func (w *worker) kill9() {
	w.cmd.Process.Kill()
	w.cmd.Wait()
	w.cmd = nil
}

func (w *worker) runs() int {
	raw, err := os.ReadFile(filepath.Join(w.markerDir, "runs"))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func (w *worker) marker(name string) int {
	raw, err := os.ReadFile(filepath.Join(w.markerDir, name))
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(string(raw), "%d", &pid)
	return pid
}

func (w *worker) heldAttempts() int {
	entries, err := os.ReadDir(filepath.Join(w.stateDir, "attempts"))
	if err != nil {
		return 0
	}
	return len(entries)
}

func processAlive(pid int) bool {
	if pid == 0 {
		return false
	}
	// A killed child can linger as a zombie, which still answers signal 0.
	out, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return false
	}
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}

// --- API client ---

type jobError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type jobEvent struct {
	ID      int64           `json:"id"`
	Attempt string          `json:"attempt_id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type job struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Terminal bool   `json:"terminal"`
	Attempts int    `json:"attempts"`
	Node     string `json:"node"`
	Progress *struct {
		Stage   string   `json:"stage"`
		Percent *float64 `json:"percent"`
	} `json:"progress"`
	Error     *jobError `json:"error"`
	Artifacts []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		URL    string `json:"url"`
	} `json:"artifacts"`
	Result json.RawMessage `json:"result"`
	Events []jobEvent      `json:"events"`
}

func (h *harness) request(method, path, token string, body any) (int, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.url+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// waitForCapability blocks until a node offering the capability is online.
func (h *harness) waitForCapability(name string, nodes int) {
	h.t.Helper()
	eventually(h.t, 15*time.Second, name+" to come online", func() bool {
		status, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
		if status != 200 {
			return false
		}
		var body struct {
			Capabilities []struct {
				Name        string `json:"name"`
				OnlineNodes int    `json:"online_nodes"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return false
		}
		for _, c := range body.Capabilities {
			if c.Name == name && c.OnlineNodes >= nodes {
				return true
			}
		}
		return false
	})
}

func (h *harness) submit(body map[string]any) job {
	h.t.Helper()
	status, raw := h.request("POST", "/v1/jobs", h.apiKey, body)
	if status != http.StatusAccepted {
		h.t.Fatalf("submit: status %d: %s", status, raw)
	}
	var j job
	if err := json.Unmarshal(raw, &j); err != nil {
		h.t.Fatal(err)
	}
	return j
}

func (h *harness) submitTTS(text string, steps int) job {
	h.t.Helper()
	return h.submit(map[string]any{"capability": "tts.synthesize", "input": map[string]any{"text": text, "steps": steps}})
}

func (h *harness) getJob(id string) job {
	h.t.Helper()
	status, raw := h.request("GET", "/v1/jobs/"+id+"?include=events", h.apiKey, nil)
	if status != 200 {
		h.t.Fatalf("get job: status %d: %s", status, raw)
	}
	var j job
	if err := json.Unmarshal(raw, &j); err != nil {
		h.t.Fatal(err)
	}
	return j
}

func (h *harness) waitStatus(id, status string) job {
	h.t.Helper()
	var last job
	eventually(h.t, 30*time.Second, "job to reach "+status, func() bool {
		last = h.getJob(id)
		return last.Status == status
	})
	return last
}

func (h *harness) waitTerminal(id string) job {
	h.t.Helper()
	var last job
	eventually(h.t, 45*time.Second, "job to finish", func() bool {
		last = h.getJob(id)
		return last.Terminal
	})
	return last
}

func (h *harness) artifact(j job) string {
	h.t.Helper()
	if len(j.Artifacts) != 1 {
		h.t.Fatalf("expected 1 artifact, got %d: %+v", len(j.Artifacts), j)
	}
	status, raw := h.request("GET", j.Artifacts[0].URL, h.apiKey, nil)
	if status != 200 {
		h.t.Fatalf("download artifact: status %d: %s", status, raw)
	}
	return string(raw)
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// assertSingleResult checks the guarantee agents rely on: every attempt's events
// are contiguous and unduplicated, and exactly one final event decided the job.
func assertSingleResult(t *testing.T, j job) {
	t.Helper()
	last := map[string]int64{}
	finals := 0
	for _, ev := range j.Events {
		if ev.Seq != last[ev.Attempt]+1 {
			t.Errorf("attempt %s: event %d follows %d", ev.Attempt, ev.Seq, last[ev.Attempt])
		}
		last[ev.Attempt] = ev.Seq
		if ev.Type == "completed" || ev.Type == "failed" {
			finals++
		}
	}
	if finals != 1 {
		t.Errorf("expected exactly 1 final event, got %d: %+v", finals, j.Events)
	}
}
