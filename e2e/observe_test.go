package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testedCapability = `
  - name: voice.tested
    adapter: cli
    command: [@TOOL@, normal, "{{workdir}}/out.wav", "{{input.steps}}"]
    stdin: "{{input.text}}"
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema:
      type: object
      required: [text, steps]
      properties:
        text: { type: string }
        steps: { type: integer }
    outputs: { audio: out.wav }
    timeout: 1m
    test:
      input: { text: fleet-test-sample, steps: 1 }
`

type testResponse struct {
	Tests []struct {
		Node string `json:"node"`
		Job  struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Test   bool   `json:"test"`
		} `json:"job"`
	} `json:"tests"`
	Skipped []struct {
		Node   string `json:"node"`
		Reason string `json:"reason"`
	} `json:"skipped"`
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func (h *harness) testCapability(name string, body map[string]any) (int, testResponse) {
	h.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	status, raw := h.request("POST", "/v1/capabilities/"+name+"/test", h.apiKey, body)
	var out testResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("%d %s: %v", status, raw, err)
	}
	return status, out
}

type offerTest struct {
	Name     string `json:"name"`
	Testable bool   `json:"testable"`
	LastTest *struct {
		JobID     string `json:"job_id"`
		Status    string `json:"status"`
		ErrorCode string `json:"error_code"`
	} `json:"last_test"`
}

func (h *harness) offerTest(capability, node string) offerTest {
	h.t.Helper()
	_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
	var body struct {
		Capabilities []struct {
			Name  string      `json:"name"`
			Nodes []offerTest `json:"nodes"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		h.t.Fatal(err)
	}
	for _, c := range body.Capabilities {
		for _, offer := range c.Nodes {
			if c.Name == capability && offer.Name == node {
				return offer
			}
		}
	}
	h.t.Fatalf("%s isn't offered by %s: %s", capability, node, raw)
	return offerTest{}
}

// A test runs what the worker declared, as a real job, on the machine it's for.
func TestCapabilityTestRunsTheWorkersOwnTest(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, testedCapability)
	h.waitReady("voice.tested", 1)

	if got := h.offerTest("voice.tested", "mac"); !got.Testable || got.LastTest != nil {
		t.Errorf("before testing: %+v", got)
	}
	if got := h.offerTest("tts.synthesize", "mac"); got.Testable {
		t.Error("a capability with no test says it's testable")
	}

	status, started := h.testCapability("voice.tested", nil)
	if status != http.StatusAccepted || len(started.Tests) != 1 || len(started.Skipped) != 0 {
		t.Fatalf("%d %+v", status, started)
	}
	test := started.Tests[0]
	if test.Node != "mac" || !test.Job.Test {
		t.Errorf("started = %+v", test)
	}
	done := h.waitTerminal(test.Job.ID)
	if done.Status != "succeeded" || done.Node != "mac" {
		t.Fatalf("test job = %s on %s: %+v", done.Status, done.Node, done.Error)
	}
	if got := h.artifact(done); !strings.Contains(got, "fleet-test-sample") {
		t.Errorf("didn't run the declared input: %q", got)
	}
	last := h.offerTest("voice.tested", "mac").LastTest
	if last == nil || last.Status != "succeeded" || last.JobID != test.Job.ID {
		t.Errorf("last test = %+v", last)
	}

	// An ordinary job isn't a test, and doesn't replace the latest test's result.
	plain := h.waitTerminal(h.submit(map[string]any{"capability": "voice.tested", "input": map[string]any{"text": "x", "steps": 1}}).ID)
	if plain.Status != "succeeded" {
		t.Fatalf("ordinary job: %+v", plain)
	}
	_, raw := h.request("GET", "/v1/jobs/"+plain.ID, h.apiKey, nil)
	if strings.Contains(string(raw), `"test":`) {
		t.Errorf("an ordinary job is marked as a test: %s", raw)
	}
	if last := h.offerTest("voice.tested", "mac").LastTest; last.JobID != test.Job.ID {
		t.Errorf("an ordinary job counted as a test: %+v", last)
	}

	// The same key gives the same job, so a retried request can't run it twice.
	_, first := h.testCapability("voice.tested", map[string]any{"idempotency_key": "again"})
	status, second := h.testCapability("voice.tested", map[string]any{"idempotency_key": "again"})
	if status != http.StatusOK || second.Tests[0].Job.ID != first.Tests[0].Job.ID {
		t.Errorf("repeat = %d %+v, first = %+v", status, second, first)
	}
	h.waitTerminal(first.Tests[0].Job.ID)
}

func TestCapabilityTestRefusesWhatItCantRun(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, testedCapability, anyObject("svc.down", "http", `    request: { url: "http://127.0.0.1:9/run" }
    response: { result: json }
    check: { command: [/bin/false], every: 100ms }
    test: { input: {} }
`))
	h.waitForCapability("voice.tested", 1)
	eventually(t, 15*time.Second, "the failed check to be reported", func() bool {
		return h.capabilityStatus("svc.down").Nodes[0].NotReady != ""
	})

	expect := func(what, capability string, body map[string]any, wantStatus int, wantCode string, retryable bool, wantMessage string) {
		t.Helper()
		status, out := h.testCapability(capability, body)
		if status != wantStatus || out.Error.Code != wantCode || out.Error.Retryable != retryable || !strings.Contains(out.Error.Message, wantMessage) {
			t.Errorf("%s: %d %+v, want %d %s %q", what, status, out.Error, wantStatus, wantCode, wantMessage)
		}
	}
	expect("no test declared", "tts.synthesize", nil, 422, "NO_TEST", false, "mac: it doesn't declare a test")
	expect("unknown capability", "no.such", nil, 404, "UNKNOWN_CAPABILITY", false, "no.such")
	expect("unknown node", "voice.tested", map[string]any{"node": "pc"}, 404, "UNKNOWN_NODE", false, "pc")
	expect("not ready", "svc.down", nil, 409, "NOT_READY", true, "mac: it isn't ready: ")
	expect("bad timeout", "voice.tested", map[string]any{"queue_timeout": "soon"}, 400, "BAD_REQUEST", false, "queue_timeout")

	// Callers choose when it runs. They can't choose what it runs.
	expect("unknown field", "voice.tested", map[string]any{"input": map[string]any{"text": "mine"}}, 400, "BAD_REQUEST", false, "input")

	limited := h.credential("key", "add", "limited", "--capabilities", "video.*")
	if status, _ := h.request("POST", "/v1/capabilities/voice.tested/test", limited, map[string]any{}); status != http.StatusForbidden {
		t.Errorf("a key without this capability got %d", status)
	}
	if status, _ := h.request("POST", "/v1/capabilities/voice.tested/test", "", map[string]any{}); status != http.StatusUnauthorized {
		t.Errorf("no key got %d", status)
	}
	if status, _ := h.request("POST", "/v1/capabilities/voice.tested/test", h.dashboardKey(), map[string]any{}); status != http.StatusForbidden {
		t.Errorf("a dashboard key got %d", status)
	}
}

// With several machines, each is tested, and a test stays on its own machine even
// when another could run it sooner.
func TestCapabilityTestCoversEachMachine(t *testing.T) {
	h := newHarness(t)
	mac := h.startWorker("mac", h.url, false, testedCapability)
	h.startWorker("pc", h.url, false, testedCapability)
	h.waitReady("voice.tested", 2)

	status, started := h.testCapability("voice.tested", nil)
	if status != http.StatusAccepted || len(started.Tests) != 2 {
		t.Fatalf("%d %+v", status, started)
	}
	for _, test := range started.Tests {
		if done := h.waitTerminal(test.Job.ID); done.Status != "succeeded" || done.Node != test.Node {
			t.Errorf("test for %s ran on %s: %s", test.Node, done.Node, done.Status)
		}
	}

	for range 5 {
		_, one := h.testCapability("voice.tested", map[string]any{"node": "pc"})
		if len(one.Tests) != 1 || one.Tests[0].Node != "pc" {
			t.Fatalf("%+v", one)
		}
		if done := h.waitTerminal(one.Tests[0].Job.ID); done.Node != "pc" {
			t.Errorf("a test for pc ran on %s", done.Node)
		}
	}

	// A machine that's gone is reported, and the others are still tested.
	mac.stop()
	h.waitReady("voice.tested", 1)
	status, partial := h.testCapability("voice.tested", nil)
	if status != http.StatusAccepted || len(partial.Tests) != 1 || partial.Tests[0].Node != "pc" ||
		len(partial.Skipped) != 1 || partial.Skipped[0].Node != "mac" || partial.Skipped[0].Reason != "it isn't connected" {
		t.Errorf("%d %+v", status, partial)
	}
	h.waitTerminal(partial.Tests[0].Job.ID)
	status, gone := h.testCapability("voice.tested", map[string]any{"node": "mac"})
	if status != http.StatusConflict || gone.Error.Code != "NOT_READY" || !gone.Error.Retryable {
		t.Errorf("testing the missing machine: %d %+v", status, gone)
	}
}

func TestFleetTestCommand(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, testedCapability, `
  - name: voice.broken
    adapter: cli
    command: [@TOOL@, fail]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    timeout: 1m
    test: { input: {} }
`)
	h.waitReady("voice.tested", 1)
	dir := t.TempDir()

	result := h.fleet(dir, "test", "voice.tested")
	if result.code != 0 || !regexp.MustCompile(`mac: passed in `).MatchString(result.stderr) {
		t.Fatalf("exit %d\n%s\n%s", result.code, result.stdout, result.stderr)
	}
	var output struct {
		Tests []struct {
			Node string `json:"node"`
			Job  struct {
				Status string `json:"status"`
				Test   bool   `json:"test"`
			} `json:"job"`
		} `json:"tests"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &output); err != nil {
		t.Fatalf("%v: %s", err, result.stdout)
	}
	if len(output.Tests) != 1 || output.Tests[0].Job.Status != "succeeded" || !output.Tests[0].Job.Test {
		t.Errorf("output = %s", result.stdout)
	}

	failed := h.fleet(dir, "test", "voice.broken")
	if failed.code != 1 || !strings.Contains(failed.stderr, "mac: failed: ") {
		t.Errorf("a failing test: exit %d\n%s", failed.code, failed.stderr)
	}
	if last := h.offerTest("voice.broken", "mac").LastTest; last == nil || last.Status != "failed" || last.ErrorCode == "" {
		t.Errorf("last test = %+v", last)
	}

	if none := h.fleet(dir, "test", "tts.synthesize"); none.code != 1 || !strings.Contains(none.stderr, "doesn't declare a test") {
		t.Errorf("no test declared: exit %d\n%s", none.code, none.stderr)
	}
	if missing := h.fleet(dir, "test", "voice.tested", "--node", "pc"); missing.code != 1 {
		t.Errorf("unknown node: exit %d\n%s", missing.code, missing.stderr)
	}
	if usage := h.fleet(dir, "test"); usage.code != 2 {
		t.Errorf("no capability: exit %d", usage.code)
	}

	// The dashboard shows the outcome, with no way to read the test's input.
	var view struct {
		Nodes []struct {
			LastTests map[string]struct {
				Status string `json:"status"`
			} `json:"last_tests"`
		} `json:"nodes"`
	}
	dashboard := h.dashboardKey()
	h.dashboardGet(dashboard, "overview", &view)
	if len(view.Nodes) != 1 || view.Nodes[0].LastTests["voice.tested"].Status != "succeeded" || view.Nodes[0].LastTests["voice.broken"].Status != "failed" {
		t.Errorf("dashboard = %+v", view)
	}
	var jobs struct {
		Jobs []struct {
			Test bool `json:"test"`
		} `json:"jobs"`
	}
	h.dashboardGet(dashboard, "jobs", &jobs)
	if len(jobs.Jobs) == 0 || !jobs.Jobs[0].Test {
		t.Errorf("dashboard jobs = %+v", jobs)
	}
}

// startLoggedWorker runs a stopped harness worker with its own logging options.
func startLoggedWorker(t *testing.T, config string, stderr *os.File, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(workerBin, append([]string{"--config", config}, args...)...)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	})
	return cmd
}

// The log has to answer "what happened to that job?" without a debugger, and has to
// keep job input out unless someone turns on debug logging.
func TestWorkerLogShowsWhatHappened(t *testing.T) {
	h := newHarness(t)
	fail := `
  - name: voice.broken
    adapter: cli
    command: [@TOOL@, fail]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    timeout: 1m
`
	w := h.startWorker("mac", h.url, false, testedCapability, fail)
	w.stop()
	read := func(path string) string {
		raw, _ := os.ReadFile(path)
		return string(raw)
	}

	logPath := filepath.Join(h.dir, "info.log")
	stderr, err := os.Create(filepath.Join(h.dir, "info.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	cmd := startLoggedWorker(t, w.config, stderr, "--log-file", logPath)
	h.waitReady("voice.tested", 1)

	const secretInput = "secret-job-input-7731"
	ok := h.waitTerminal(h.submit(map[string]any{"capability": "voice.tested", "input": map[string]any{"text": secretInput, "steps": 1}}).ID)
	bad := h.waitTerminal(h.runCapability("voice.broken", map[string]any{}).ID)
	_, started := h.testCapability("voice.tested", nil)
	h.waitTerminal(started.Tests[0].Job.ID)
	if ok.Status != "succeeded" || bad.Status != "failed" {
		t.Fatalf("jobs: %s %s", ok.Status, bad.Status)
	}
	eventually(t, 5*time.Second, "the log to include the test", func() bool { return strings.Contains(read(logPath), "test=true") })
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()

	log := read(logPath)
	for what, pattern := range map[string]string{
		"startup":      `msg="worker starting" version=\S+ node=mac gateway=\S+ log_level=INFO`,
		"connection":   `msg="connected to gateway"`,
		"offer":        `msg="capability offered" capability=voice.tested adapter=cli`,
		"job start":    fmt.Sprintf(`msg="attempt started" job=%s .*capability=voice.tested test=false`, ok.ID),
		"job success":  fmt.Sprintf(`level=INFO msg="attempt completed" job=%s .*capability=voice.tested files=1 duration=\S+`, ok.ID),
		"upload":       `msg="uploading result" .*file=\S+ bytes=[0-9]+`,
		"job failure":  fmt.Sprintf(`level=WARN msg="attempt failed" job=%s .*capability=voice.broken code=\S+ message=.+ duration=\S+`, bad.ID),
		"test job":     `msg="attempt started" .*capability=voice.tested test=true`,
		"clean finish": `msg="worker stopped"`,
	} {
		if !regexp.MustCompile(pattern).MatchString(log) {
			t.Errorf("no %s line matching %s", what, pattern)
		}
	}
	for what, text := range map[string]string{"job input": secretInput, "node token": w.token, "command output": `msg="command output"`, "debug lines": "level=DEBUG"} {
		if strings.Contains(log, text) {
			t.Errorf("the info log contains %s", what)
		}
	}
	// With no one at a console, a second copy would only be an unrotated log.
	if got := read(stderr.Name()); got != "" {
		t.Errorf("wrote to a non-console stderr: %q", got)
	}

	// At debug level, it also shows what the command printed, and when the manifest
	// asks for it, without changing any flags. The flag still has the last word.
	manifest := read(w.config)
	if err := os.WriteFile(w.config, []byte("log_level: debug\n"+manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	debugPath := filepath.Join(h.dir, "debug.log")
	cmd = startLoggedWorker(t, w.config, nil, "--log-file", debugPath)
	h.waitReady("voice.tested", 1)
	h.waitTerminal(h.submit(map[string]any{"capability": "voice.tested", "input": map[string]any{"text": secretInput, "steps": 1}}).ID)
	eventually(t, 5*time.Second, "command output in the debug log", func() bool {
		return strings.Contains(read(debugPath), `msg="command output"`)
	})
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()
	debugLog := read(debugPath)
	if !strings.Contains(debugLog, `msg="capability checked"`) || !strings.Contains(debugLog, `msg="message from gateway"`) {
		t.Errorf("debug log is missing detail:\n%s", debugLog)
	}
	if strings.Contains(debugLog, w.token) {
		t.Error("the debug log contains the node token")
	}

	quietPath := filepath.Join(h.dir, "quiet.log")
	cmd = startLoggedWorker(t, w.config, nil, "--log-file", quietPath, "--log-level", "error")
	h.waitReady("voice.tested", 1)
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()
	if got := read(quietPath); strings.Contains(got, "level=INFO") || strings.Contains(got, "level=DEBUG") {
		t.Errorf("--log-level didn't override the manifest: %s", got)
	}
}

// A worker with no console still has to say why it won't start or why it crashed.
func TestWorkerLogExplainsStartupFailuresAndCrashes(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	w.stop()
	read := func(path string) string {
		raw, _ := os.ReadFile(path)
		return string(raw)
	}

	logPath := filepath.Join(h.dir, "broken.log")
	bad := filepath.Join(h.dir, "broken.yaml")
	if err := os.WriteFile(bad, []byte("gateway_url: not a url\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(workerBin, "--config", bad, "--log-file", logPath).Run(); err == nil {
		t.Error("started with a bad manifest")
	}
	if got := read(logPath); !strings.Contains(got, `level=ERROR msg="worker stopped"`) || !strings.Contains(got, "gateway_url") {
		t.Errorf("startup failure wasn't logged: %q", got)
	}

	levelPath := filepath.Join(h.dir, "level.log")
	if err := exec.Command(workerBin, "--config", w.config, "--log-file", levelPath, "--log-level", "loud").Run(); err == nil {
		t.Error("started with a bad log level")
	}
	if got := read(levelPath); !strings.Contains(got, "must be debug, info, warn or error") {
		t.Errorf("bad level wasn't logged: %q", got)
	}

	// A worker crashing on every start can't fill the disk with crash reports.
	fullPath := filepath.Join(h.dir, "full.log")
	if err := os.WriteFile(fullPath+".crash", make([]byte, 11<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(workerBin, "--config", bad, "--log-file", fullPath).Run(); err == nil {
		t.Error("started with a bad manifest")
	}
	if info, err := os.Stat(fullPath + ".crash"); err != nil || info.Size() != 0 {
		t.Errorf("a full crash log wasn't moved aside: %v %v", info, err)
	}
	if info, err := os.Stat(fullPath + ".crash.1"); err != nil || info.Size() != 11<<20 {
		t.Errorf("the previous crash log is missing: %v %v", info, err)
	}

	// SIGQUIT makes the Go runtime crash the same way a fatal error does.
	crashPath := filepath.Join(h.dir, "crash.log")
	cmd := exec.Command(workerBin, "--config", w.config, "--log-file", crashPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.waitReady("tts.synthesize", 1)
	cmd.Process.Signal(syscall.SIGQUIT)
	cmd.Wait()
	if got := read(crashPath + ".crash"); !strings.Contains(got, "goroutine ") {
		t.Errorf("the crash wasn't saved: %q", got)
	}
}
