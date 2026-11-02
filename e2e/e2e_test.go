package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireSucceeded(t *testing.T, j job) {
	t.Helper()
	if j.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %s (error %+v)", j.Status, j.Error)
	}
}

func TestJobRunsFromAPICallToReturnedResult(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("hello world", 8)
	if j.Status != "queued" {
		t.Fatalf("new job status = %s", j.Status)
	}

	// The agent should see real progress while the job runs.
	sawProgress := false
	eventually(t, 15*time.Second, "progress", func() bool {
		current := h.getJob(j.ID)
		sawProgress = current.Progress != nil && current.Progress.Percent != nil
		return sawProgress || current.Terminal
	})
	if !sawProgress {
		t.Error("never saw a progress percentage")
	}

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Node != "mac" || done.Attempts != 1 {
		t.Errorf("node=%s attempts=%d", done.Node, done.Attempts)
	}
	if got := h.artifact(done); got != "node=mac text=hello world" {
		t.Errorf("artifact = %q", got)
	}
	if done.Artifacts[0].Name != "audio.wav" {
		t.Errorf("artifact name = %q", done.Artifacts[0].Name)
	}
	// The worker cleans up once the gateway has acknowledged the result.
	eventually(t, 5*time.Second, "worker cleanup", func() bool { return w.heldAttempts() == 0 })
}

// Handshakes run under a 30s timeout, which must not outlive them and kill the
// connection. Heartbeats here are short, so any unexpected drop shows in the log.
func TestConnectionOutlivesHandshakeTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes over 30 seconds")
	}
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	time.Sleep(35 * time.Second)

	done := runSimple(h, "tts.synthesize", map[string]any{"text": "still connected", "steps": 1})
	requireSucceeded(t, done)
	raw, err := os.ReadFile(h.logs.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "disconnected from gateway") {
		t.Errorf("the connection dropped during the test:\n%s", raw)
	}
}

func TestLongPollReturnsAsSoonAsJobFinishes(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	j := h.submitTTS("x", 2)

	start := time.Now()
	status, raw := h.request("GET", "/v1/jobs/"+j.ID+"?wait=30", h.apiKey, nil)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	var done job
	if err := json.Unmarshal(raw, &done); err != nil {
		t.Fatal(err)
	}
	requireSucceeded(t, done)
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("long poll took %s", elapsed)
	}
}

// The milestone test from the design: unplug the network during a job.
func TestBriefOutageDoesNotDisturbJob(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	w := h.startWorker("mac", p.url(), false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("survives", 16)
	h.waitStatus(j.ID, "running")
	p.cut()
	time.Sleep(time.Second) // Shorter than the 3s lease.
	if status := h.getJob(j.ID).Status; status != "running" {
		t.Errorf("status during brief outage = %s", status)
	}
	p.restore()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Attempts != 1 || w.runs() != 1 {
		t.Errorf("attempts=%d runs=%d, want 1 and 1", done.Attempts, w.runs())
	}
	if got := h.artifact(done); got != "node=mac text=survives" {
		t.Errorf("artifact = %q", got)
	}
}

func TestLongOutageMarksJobLostThenResumes(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	w := h.startWorker("mac", p.url(), false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("resumed", 32)
	h.waitStatus(j.ID, "running")
	p.cut()
	lost := h.waitStatus(j.ID, "lost")
	if lost.Terminal {
		t.Error("lost should not be terminal while the worker may return")
	}
	p.restore()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Attempts != 1 || w.runs() != 1 {
		t.Errorf("attempts=%d runs=%d: the command must not rerun", done.Attempts, w.runs())
	}
	if got := h.artifact(done); got != "node=mac text=resumed" {
		t.Errorf("artifact = %q", got)
	}
}

func TestJobFinishingDuringOutageDeliversResultAfterReconnect(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	w := h.startWorker("mac", p.url(), false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("finished offline", 4)
	h.waitStatus(j.ID, "running")
	p.cut()
	// Wait for the command to end while the upload has nowhere to go.
	eventually(t, 15*time.Second, "command to exit", func() bool { return !processAlive(w.marker("pid")) })
	time.Sleep(2 * time.Second)
	if h.getJob(j.ID).Terminal {
		t.Fatal("job finished while the worker was offline")
	}
	p.restore()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Attempts != 1 {
		t.Errorf("attempts = %d", done.Attempts)
	}
	if got := h.artifact(done); got != "node=mac text=finished offline" {
		t.Errorf("artifact = %q", got)
	}
}

func TestRetryableJobMovesToAnotherNodeAndFencesTheFirst(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	first := h.startWorker("first", p.url(), true)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("failover", 60)
	h.waitStatus(j.ID, "running")
	firstAttempt := h.getJob(j.ID).Events[0].Attempt
	firstPID := first.marker("pid")

	// With the first node offline, a second node picks up the job.
	p.cut()
	second := h.startWorker("second", h.url, true)
	eventually(t, 20*time.Second, "job to move to the second node", func() bool {
		current := h.getJob(j.ID)
		return current.Attempts == 2 && current.Status == "running"
	})

	// The first node returns while still running. It must be stopped and cleaned
	// up, and it must not be able to affect the job.
	p.restore()
	eventually(t, 10*time.Second, "fenced command to stop", func() bool { return !processAlive(firstPID) })
	eventually(t, 10*time.Second, "fenced attempt cleanup", func() bool { return first.heldAttempts() == 0 })

	// Even with valid node credentials, the old attempt can't upload a result.
	req, err := http.NewRequest("PUT", h.url+"/v1/worker/attempts/"+firstAttempt+"/artifacts/audio.wav", strings.NewReader("forged"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+first.token)
	req.Header.Set("X-Content-SHA256", strings.Repeat("0", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("stale upload status = %d, want 410", resp.StatusCode)
	}

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Node != "second" || done.Attempts != 2 {
		t.Errorf("node=%s attempts=%d", done.Node, done.Attempts)
	}
	if got := h.artifact(done); got != "node=second text=failover" {
		t.Errorf("artifact = %q", got)
	}
	if second.runs() != 1 {
		t.Errorf("second node runs = %d", second.runs())
	}
}

func TestRetryableJobKeepsRunningWorkIfWorkerReturnsFirst(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	w := h.startWorker("mac", p.url(), true)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("no wasted work", 32)
	h.waitStatus(j.ID, "running")
	p.cut()
	h.waitStatus(j.ID, "queued") // Lease expired and the job is eligible for a rerun.
	p.restore()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Attempts != 1 || w.runs() != 1 {
		t.Errorf("attempts=%d runs=%d: the surviving attempt should have been kept", done.Attempts, w.runs())
	}
}

func TestLostJobFailsWhenWorkerNeverReturns(t *testing.T) {
	h := newHarness(t, "--lost-grace", "1s")
	p := h.newProxy()
	h.startWorker("mac", p.url(), false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("abandoned", 200)
	h.waitStatus(j.ID, "running")
	p.cut()

	done := h.waitTerminal(j.ID)
	if done.Status != "failed" || done.Error == nil || done.Error.Code != "NODE_LOST" || !done.Error.Retryable {
		t.Fatalf("got %+v", done)
	}
}

func TestWorkerCrashStopsOrphanAndFailsNonRetryableJob(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("crash", 200)
	h.waitStatus(j.ID, "running")
	eventually(t, 5*time.Second, "pid marker", func() bool { return w.marker("pid") != 0 })
	pid := w.marker("pid")

	w.kill9()
	if !processAlive(pid) {
		t.Fatal("expected the command to outlive the crashed worker")
	}
	w.start()

	done := h.waitTerminal(j.ID)
	if done.Status != "failed" || done.Error == nil || done.Error.Code != "WORKER_RESTARTED" || !done.Error.Retryable {
		t.Fatalf("got %+v", done)
	}
	assertSingleResult(t, done)
	if processAlive(pid) {
		t.Error("restarted worker left the orphaned command running")
	}
	eventually(t, 5*time.Second, "worker cleanup", func() bool { return w.heldAttempts() == 0 })
}

func TestWorkerCrashRerunsRetryableJob(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, true)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("rerun", 16)
	h.waitStatus(j.ID, "running")
	w.kill9()
	w.start()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	if done.Attempts != 2 || w.runs() != 2 {
		t.Errorf("attempts=%d runs=%d, want 2 and 2", done.Attempts, w.runs())
	}
	if got := h.artifact(done); got != "node=mac text=rerun" {
		t.Errorf("artifact = %q", got)
	}
}

func TestGracefulWorkerRestartReportsInterruption(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("restart", 200)
	h.waitStatus(j.ID, "running")
	eventually(t, 5*time.Second, "pid marker", func() bool { return w.marker("pid") != 0 })
	pid := w.marker("pid")
	w.stop()
	if processAlive(pid) {
		t.Error("graceful stop left the command running")
	}
	w.start()

	done := h.waitTerminal(j.ID)
	if done.Error == nil || done.Error.Code != "WORKER_RESTARTED" {
		t.Fatalf("got %+v", done)
	}
}

func TestGatewayCrashDoesNotLoseJob(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("gateway crash", 32)
	h.waitStatus(j.ID, "running")
	h.killGateway()
	time.Sleep(2 * time.Second)
	h.startGateway()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Attempts != 1 || w.runs() != 1 {
		t.Errorf("attempts=%d runs=%d", done.Attempts, w.runs())
	}
	if got := h.artifact(done); got != "node=mac text=gateway crash" {
		t.Errorf("artifact = %q", got)
	}
}

// A gateway outage longer than the lease must not look like every node vanished.
// The proxy keeps the worker away after the restart, so only the extended lease
// can be keeping the job running.
func TestGatewayRestartGivesWorkersTimeToReconnect(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	h.startWorker("mac", p.url(), false)
	h.waitForCapability("tts.synthesize", 1)

	j := h.submitTTS("long gateway outage", 60)
	h.waitStatus(j.ID, "running")
	p.cut()
	h.killGateway()
	time.Sleep(4 * time.Second) // Longer than the 3s lease.
	h.startGateway()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if status := h.getJob(j.ID).Status; status != "running" {
			t.Fatalf("status right after restart = %s, want running", status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	p.restore()
	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	assertSingleResult(t, done)
	if done.Attempts != 1 {
		t.Errorf("attempts = %d", done.Attempts)
	}
}

func TestQueueTimeoutReportsNoNodeOnline(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	w.stop()
	eventually(t, 5*time.Second, "node to go offline", func() bool {
		_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
		return strings.Contains(string(raw), `"online_nodes":0`)
	})

	j := h.submit(map[string]any{
		"capability":    "tts.synthesize",
		"input":         map[string]any{"text": "x", "steps": 1},
		"queue_timeout": "500ms",
	})
	done := h.waitTerminal(j.ID)
	if done.Error == nil || done.Error.Code != "NO_NODE_ONLINE" || !done.Error.Retryable {
		t.Fatalf("got %+v", done)
	}
}

func TestJobWaitsForNodeThatComesBackOnline(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	w.stop()
	eventually(t, 5*time.Second, "node to go offline", func() bool {
		_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
		return strings.Contains(string(raw), `"online_nodes":0`)
	})
	j := h.submitTTS("patient", 1)
	time.Sleep(500 * time.Millisecond)
	if status := h.getJob(j.ID).Status; status != "queued" {
		t.Fatalf("status = %s", status)
	}
	w.start()
	requireSucceeded(t, h.waitTerminal(j.ID))
}

const heavyCapability = `
  - name: heavy.task
    adapter: cli
    command: [@TOOL@, normal, "{{workdir}}/out.bin", "{{input.steps}}"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object, required: [steps], properties: { steps: { type: integer } } }
    outputs: { result: out.bin }
    requires: { heavy: 1 }
`

func TestResourceLimitsSerializeHeavyJobs(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, heavyCapability)
	h.waitForCapability("heavy.task", 1)

	submit := func() job {
		return h.submit(map[string]any{"capability": "heavy.task", "input": map[string]any{"steps": 8}})
	}
	first, second := submit(), submit()
	h.waitStatus(first.ID, "running")
	if status := h.getJob(second.ID).Status; status != "queued" {
		t.Errorf("second job should wait for the resource, got %s", status)
	}
	requireSucceeded(t, h.waitTerminal(first.ID))
	requireSucceeded(t, h.waitTerminal(second.ID))
}

func TestSelectorRoutesToMatchingNode(t *testing.T) {
	h := newHarness(t)
	h.startWorker("one", h.url, false)
	h.startWorker("two", h.url, false)
	h.waitForCapability("tts.synthesize", 2)

	j := h.submit(map[string]any{
		"capability": "tts.synthesize",
		"input":      map[string]any{"text": "routed", "steps": 1},
		"selector":   map[string]string{"name": "two"},
	})
	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	if done.Node != "two" {
		t.Errorf("ran on %s", done.Node)
	}
	status, raw := h.request("POST", "/v1/jobs", h.apiKey, map[string]any{
		"capability": "tts.synthesize",
		"input":      map[string]any{"text": "x", "steps": 1},
		"selector":   map[string]string{"name": "nobody"},
	})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "NO_MATCHING_NODE") {
		t.Errorf("status %d: %s", status, raw)
	}
}

func TestIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)

	body := map[string]any{
		"capability":      "tts.synthesize",
		"input":           map[string]any{"text": "once", "steps": 1},
		"idempotency_key": "step-3",
	}
	first := h.submit(body)
	status, raw := h.request("POST", "/v1/jobs", h.apiKey, body)
	if status != http.StatusOK {
		t.Fatalf("replay status %d: %s", status, raw)
	}
	var replay job
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID {
		t.Errorf("replay created a new job: %s vs %s", replay.ID, first.ID)
	}
	body["input"] = map[string]any{"text": "different", "steps": 1}
	if status, _ := h.request("POST", "/v1/jobs", h.apiKey, body); status != http.StatusConflict {
		t.Errorf("reused key with new input: status %d", status)
	}
	requireSucceeded(t, h.waitTerminal(first.ID))
	if w.runs() != 1 {
		t.Errorf("runs = %d", w.runs())
	}
}

const scenarioCapabilities = `
  - name: test.fail
    adapter: cli
    command: [@TOOL@, fail, "{{workdir}}/out", "1"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    outputs: { result: out }
  - name: test.symlink
    adapter: cli
    command: [@TOOL@, symlink, "{{workdir}}/out", "1"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    outputs: { result: out }
  - name: test.env
    adapter: cli
    command: [@TOOL@, env, "{{workdir}}/out", "1"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    outputs: { result: out }
  - name: test.hang
    adapter: cli
    command: [@TOOL@, grandchild, "{{workdir}}/out", "1"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    outputs: { result: out }
    timeout: 1s
  - name: test.silent
    adapter: cli
    command: [@TOOL@, grandchild, "{{workdir}}/out", "1"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    outputs: { result: out }
    retry_if_lost: true
  - name: test.copy
    adapter: cli
    command: [@TOOL@, copy, "{{workdir}}/out.bin", "{{files.source}}"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    input_files: [source]
    outputs: { result: out.bin }
  - name: test.large
    adapter: cli
    command: [@TOOL@, large, "{{workdir}}/out.bin", "{{input.megabytes}}"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object, required: [megabytes], properties: { megabytes: { type: integer } } }
    outputs: { result: out.bin }
  - name: test.missing-output
    adapter: cli
    command: [@TOOL@, normal, "{{workdir}}/elsewhere", "1"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    outputs: { result: out }
  - name: test.arg
    adapter: cli
    command: [@TOOL@, normal, "{{input.path}}", "1"]
    env: { TOOL_NODE: @NODE@, MARKER_DIR: @MARKERS@ }
    input_schema: { type: object, required: [path], properties: { path: { type: string } } }
`

func runSimple(h *harness, capability string, input map[string]any) job {
	h.t.Helper()
	j := h.submit(map[string]any{"capability": capability, "input": input})
	return h.waitTerminal(j.ID)
}

func TestCommandFailureReportsCodeAndOutput(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.fail", 1)
	done := runSimple(h, "test.fail", map[string]any{})
	if done.Status != "failed" || done.Error.Code != "COMMAND_FAILED" || done.Error.Retryable {
		t.Fatalf("got %+v", done.Error)
	}
	if !strings.Contains(done.Error.Message, "the engine exploded") {
		t.Errorf("message = %q", done.Error.Message)
	}
}

func TestMissingOutputFailsJob(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.missing-output", 1)
	done := runSimple(h, "test.missing-output", map[string]any{})
	if done.Error == nil || done.Error.Code != "OUTPUT_MISSING" {
		t.Fatalf("got %+v", done)
	}
}

func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.hang", 1)

	done := runSimple(h, "test.hang", map[string]any{})
	if done.Status != "timed_out" || done.Error.Code != "TIMED_OUT" {
		t.Fatalf("got %+v", done)
	}
	grandchild := w.marker("grandchild")
	if grandchild == 0 {
		t.Fatal("grandchild never started")
	}
	eventually(t, 5*time.Second, "grandchild to die", func() bool { return !processAlive(grandchild) })
}

// --- Security ---

func TestAuthenticationAndOwnership(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	limited := h.credential("key", "add", "limited", "--capabilities", "video.*")
	other := h.credential("key", "add", "other", "--capabilities", "*")
	input := map[string]any{"capability": "tts.synthesize", "input": map[string]any{"text": "x", "steps": 1}}

	cases := []struct {
		name, method, path, token string
		body                      any
		want                      int
	}{
		{"no key", "GET", "/v1/capabilities", "", nil, 401},
		{"bad key", "GET", "/v1/capabilities", "fk_nope", nil, 401},
		{"node token on agent API", "POST", "/v1/jobs", w.token, input, 401},
		{"key outside its scope", "POST", "/v1/jobs", limited, input, 403},
	}
	for _, c := range cases {
		if status, raw := h.request(c.method, c.path, c.token, c.body); status != c.want {
			t.Errorf("%s: status %d, want %d: %s", c.name, status, c.want, raw)
		}
	}

	// A limited key doesn't even see capabilities outside its scope.
	if _, raw := h.request("GET", "/v1/capabilities", limited, nil); strings.Contains(string(raw), "tts.synthesize") {
		t.Errorf("limited key can see other capabilities: %s", raw)
	}
	// An agent key can't act as a worker.
	if status, _ := h.request("GET", "/v1/worker/connect", h.apiKey, nil); status != 401 {
		t.Errorf("agent key on worker endpoint: status %d", status)
	}

	done := runSimple(h, "tts.synthesize", map[string]any{"text": "private", "steps": 1})
	requireSucceeded(t, done)

	// Jobs and files are invisible to other keys, not just forbidden.
	if status, _ := h.request("GET", "/v1/jobs/"+done.ID, other, nil); status != 404 {
		t.Errorf("other key reading job: status %d", status)
	}
	if status, _ := h.request("GET", done.Artifacts[0].URL, other, nil); status != 404 {
		t.Errorf("other key reading file: status %d", status)
	}

	// Files must download as opaque attachments.
	req, _ := http.NewRequest("GET", h.url+done.Artifacts[0].URL, nil)
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("unsafe download headers: %v", resp.Header)
	}

	// Revoked keys stop working immediately.
	h.credential("key", "revoke", "other")
	if status, _ := h.request("GET", "/v1/capabilities", other, nil); status != 401 {
		t.Errorf("revoked key: status %d", status)
	}
}

func TestRevokedNodeIsDisconnected(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	h.credential("node", "revoke", w.name)
	eventually(t, 5*time.Second, "capability to disappear", func() bool {
		_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
		return !strings.Contains(string(raw), "tts.synthesize")
	})
	if status, _ := h.request("POST", "/v1/jobs", h.apiKey, map[string]any{
		"capability": "tts.synthesize", "input": map[string]any{"text": "x", "steps": 1},
	}); status != 404 {
		t.Errorf("job for a revoked node's capability: status %d", status)
	}
	// The open connection must actually close, which sends the worker back to the
	// gateway, where its token is now refused.
	eventually(t, 10*time.Second, "worker to be refused", func() bool {
		raw, err := os.ReadFile(h.logs.Name())
		return err == nil && strings.Contains(string(raw), "the gateway rejected this node's token")
	})
}

// A replaced attempt that prints nothing sends no events, so the reconnect itself
// has to stop it. Otherwise it would hold the machine until its command finished.
func TestReconnectStopsSilentReplacedAttempt(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	first := h.startWorker("first", p.url(), false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)

	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	eventually(t, 5*time.Second, "pid marker", func() bool { return first.marker("pid") != 0 })
	pid := first.marker("pid")

	p.cut()
	h.startWorker("second", h.url, false, scenarioCapabilities)
	eventually(t, 20*time.Second, "job to move to the second node", func() bool {
		current := h.getJob(j.ID)
		return current.Attempts == 2 && current.Status == "running"
	})
	if !processAlive(pid) {
		t.Fatal("the first command should still be running while its worker is offline")
	}

	p.restore()
	eventually(t, 10*time.Second, "replaced command to stop", func() bool { return !processAlive(pid) })
	eventually(t, 10*time.Second, "replaced attempt cleanup", func() bool { return first.heldAttempts() == 0 })
}

func TestWorkerRejectsTokenForAnotherNode(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	w.stop()
	other := h.credential("node", "add", "other-node")
	if err := os.WriteFile(filepath.Join(filepath.Dir(w.config), "token"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	w.start()
	time.Sleep(time.Second)
	_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
	if !strings.Contains(string(raw), `"online_nodes":0`) {
		t.Errorf("a token for another node was accepted: %s", raw)
	}
}

func TestInputIsValidatedBeforeQueueing(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	bad := []map[string]any{
		{"text": "missing steps"},
		{"text": "", "steps": 1},
		{"text": "x", "steps": "1; rm -rf /"},
		{"text": "x", "steps": 1, "extra": true},
	}
	for _, input := range bad {
		status, raw := h.request("POST", "/v1/jobs", h.apiKey, map[string]any{"capability": "tts.synthesize", "input": input})
		if status != http.StatusUnprocessableEntity {
			t.Errorf("input %v: status %d: %s", input, status, raw)
		}
	}
	if status, _ := h.request("POST", "/v1/jobs", h.apiKey, map[string]any{"capability": "does.not.exist"}); status != 404 {
		t.Errorf("unknown capability: status %d", status)
	}
}

func TestInputCannotInjectOptionsOrShellCommands(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.arg", 1)

	// A leading dash would be read as an option, so it never reaches the program.
	done := runSimple(h, "test.arg", map[string]any{"path": "--output=/etc/passwd"})
	if done.Error == nil || done.Error.Code != "INVALID_INPUT" {
		t.Fatalf("got %+v", done)
	}
	if w.runs() != 0 {
		t.Errorf("command ran %d times despite invalid input", w.runs())
	}

	// There is no shell, so metacharacters are inert: this is one literal argument.
	canary := filepath.Join(w.markerDir, "canary")
	attack := fmt.Sprintf("x; touch %s; $(touch %s)", canary, canary)
	runSimple(h, "test.arg", map[string]any{"path": filepath.Join(os.TempDir(), attack)})
	if _, err := os.Stat(canary); err == nil {
		t.Error("input reached a shell")
	}

	// Placeholder syntax in a value isn't expanded a second time.
	tts := runSimple(h, "tts.synthesize", map[string]any{"text": "{{workdir}}", "steps": 1})
	requireSucceeded(t, tts)
	if got := h.artifact(tts); got != "node=mac text={{workdir}}" {
		t.Errorf("artifact = %q", got)
	}
}

func TestUploadsAreCheckedAgainstOwnerAndChecksum(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	otherToken := h.credential("node", "add", "other")

	j := h.submitTTS("real result", 40)
	h.waitStatus(j.ID, "running")
	attempt := h.getJob(j.ID).Events[0].Attempt
	macToken := strings.TrimSpace(readFile(t, filepath.Join(h.dir, "worker-mac", "token")))

	put := func(token, sum string) int {
		req, err := http.NewRequest("PUT", h.url+"/v1/worker/attempts/"+attempt+"/artifacts/audio.wav", strings.NewReader("forged"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Content-SHA256", sum)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	wrongSum := strings.Repeat("0", 64)
	if status := put("", wrongSum); status != http.StatusUnauthorized {
		t.Errorf("no token: status %d", status)
	}
	if status := put(h.apiKey, wrongSum); status != http.StatusUnauthorized {
		t.Errorf("agent key: status %d", status)
	}
	if status := put(otherToken, wrongSum); status != http.StatusGone {
		t.Errorf("another node's token: status %d", status)
	}
	if status := put(macToken, wrongSum); status != http.StatusUnprocessableEntity {
		t.Errorf("wrong checksum: status %d", status)
	}

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	if got := h.artifact(done); got != "node=mac text=real result" {
		t.Errorf("artifact = %q", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSymlinkOutputCannotExfiltrateFiles(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.symlink", 1)
	if err := os.WriteFile(filepath.Join(w.markerDir, "secret"), []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := runSimple(h, "test.symlink", map[string]any{})
	if done.Status != "failed" || done.Error.Code != "OUTPUT_MISSING" || len(done.Artifacts) != 0 {
		t.Fatalf("got %+v", done)
	}
}

func TestCommandsDoNotInheritTheWorkerEnvironment(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.env", 1)
	w.stop()
	w.tokenInEnv = true
	w.start()
	h.waitForCapability("test.env", 1)

	done := runSimple(h, "test.env", map[string]any{})
	requireSucceeded(t, done)
	env := h.artifact(done)
	if strings.Contains(env, w.token) {
		t.Error("the node token leaked into the command's environment")
	}
	if strings.Contains(env, "FLEET_SENTINEL") {
		t.Error("the worker's environment was inherited")
	}
	if !strings.Contains(env, "TOOL_NODE=mac") {
		t.Errorf("manifest env missing: %s", env)
	}
}

func TestArtifactStorageLimit(t *testing.T) {
	h := newHarness(t, "--max-artifact-bytes", "5")
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	done := runSimple(h, "tts.synthesize", map[string]any{"text": "far too long for the limit", "steps": 1})
	if done.Status != "failed" || done.Error.Code != "UPLOAD_FAILED" {
		t.Fatalf("got %+v", done)
	}
}

func TestArtifactsExpire(t *testing.T) {
	h := newHarness(t, "--artifact-ttl", "1s")
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	done := runSimple(h, "tts.synthesize", map[string]any{"text": "short lived", "steps": 1})
	requireSucceeded(t, done)
	url := done.Artifacts[0].URL
	eventually(t, 10*time.Second, "artifact to expire", func() bool {
		status, _ := h.request("GET", url, h.apiKey, nil)
		return status == 404
	})
	entries, err := os.ReadDir(filepath.Join(h.dir, "gateway", "files"))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "file removal", func() bool {
		entries, err = os.ReadDir(filepath.Join(h.dir, "gateway", "files"))
		return err == nil && len(entries) == 0
	})
}
