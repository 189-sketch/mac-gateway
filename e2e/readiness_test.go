package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type capabilityStatus struct {
	Name        string `json:"name"`
	OnlineNodes int    `json:"online_nodes"`
	ReadyNodes  int    `json:"ready_nodes"`
	Nodes       []struct {
		Name     string `json:"name"`
		Online   bool   `json:"online"`
		Ready    bool   `json:"ready"`
		NotReady string `json:"not_ready"`
	} `json:"nodes"`
}

func (h *harness) capabilityStatus(name string) capabilityStatus {
	h.t.Helper()
	status, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
	if status != 200 {
		h.t.Fatalf("capabilities: %d %s", status, raw)
	}
	var body struct {
		Capabilities []capabilityStatus `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		h.t.Fatal(err)
	}
	for _, c := range body.Capabilities {
		if c.Name == name {
			return c
		}
	}
	return capabilityStatus{}
}

func (h *harness) waitReady(name string, nodes int) {
	h.t.Helper()
	eventually(h.t, 15*time.Second, fmt.Sprintf("%s to have %d ready nodes", name, nodes), func() bool {
		return h.capabilityStatus(name).ReadyNodes == nodes
	})
}

// A service that's down when the worker starts is discovered as not ready. Work
// waits for it, and reaches it only once the worker has seen it working.
func TestJobsWaitForAServiceToBecomeReady(t *testing.T) {
	var healthy atomic.Bool
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path == "/health" && !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("llm.gated", "http", fmt.Sprintf(`    request: { url: "%[1]s/run" }
    response: { result: json }
    check: { url: "%[1]s/health", every: 100ms }
`, service.URL)))
	h.waitForCapability("llm.gated", 1)

	status := h.capabilityStatus("llm.gated")
	if status.OnlineNodes != 1 || status.ReadyNodes != 0 || status.Nodes[0].Ready || status.Nodes[0].NotReady == "" {
		t.Fatalf("status = %+v", status)
	}
	if strings.Contains(status.Nodes[0].NotReady, "127.0.0.1") {
		t.Errorf("the reason exposes this machine's address: %q", status.Nodes[0].NotReady)
	}

	j := h.runCapability("llm.gated", map[string]any{})
	time.Sleep(time.Second)
	if got := h.getJob(j.ID); got.Status != "queued" {
		t.Fatalf("job should wait for the service, but is %s: %+v", got.Status, got)
	}
	if n := service.count("/run"); n != 0 {
		t.Fatalf("a service that isn't ready received %d requests", n)
	}

	healthy.Store(true)
	h.waitReady("llm.gated", 1)
	final := h.waitTerminal(j.ID)
	if final.Status != "succeeded" || string(final.Result) != `{"ok":true}` {
		t.Fatalf("job = %+v, result %s", final, final.Result)
	}

	// Going down again is discovered too, with no reconnect involved.
	healthy.Store(false)
	h.waitReady("llm.gated", 0)
	if status := h.capabilityStatus("llm.gated"); status.OnlineNodes != 1 {
		t.Errorf("the machine itself should still be online: %+v", status)
	}
}

func TestDefaultCheckConnectsToTheService(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()

	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("svc.plain", "http", fmt.Sprintf(`    request: { url: "http://%s/{{input.path}}" }
    check: { every: 100ms }
`, address)))
	h.waitForCapability("svc.plain", 1)
	if status := h.capabilityStatus("svc.plain"); status.ReadyNodes != 0 {
		t.Fatalf("nothing is listening, but status = %+v", status)
	}

	up, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("couldn't get the same port back: %v", err)
	}
	h.waitReady("svc.plain", 1)
	up.Close()
	h.waitReady("svc.plain", 0)
}

func TestDefaultCheckNoticesAToolBeingRemoved(t *testing.T) {
	h := newHarness(t)
	tool := filepath.Join(h.dir, "my-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.startWorker("mac", h.url, false, anyObject("tool.local", "cli", fmt.Sprintf("    command: [%s]\n    check: { every: 100ms }\n", tool)))
	h.waitReady("tool.local", 1)
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	h.waitReady("tool.local", 0)
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.waitReady("tool.local", 1)
	if j := h.waitTerminal(h.runCapability("tool.local", map[string]any{}).ID); j.Status != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
}

func TestCheckCommandDecidesReadiness(t *testing.T) {
	h := newHarness(t)
	flag := filepath.Join(h.dir, "healthy")
	h.startWorker("mac", h.url, false, anyObject("tool.checked", "cli", fmt.Sprintf(`    command: [/bin/sh, -c, "exit 0"]
    check: { command: [/bin/sh, -c, "test -f %s"], every: 100ms }
`, flag)))
	h.waitForCapability("tool.checked", 1)
	h.waitReady("tool.checked", 0)
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.waitReady("tool.checked", 1)
}

func TestStuckCheckCountsAsFailedAndIsKilled(t *testing.T) {
	h := newHarness(t)
	pidFile := filepath.Join(h.dir, "check.pid")
	h.startWorker("mac", h.url, false, anyObject("tool.stuck", "cli", fmt.Sprintf(`    command: [/bin/sh, -c, "exit 0"]
    check: { command: [/bin/sh, -c, "echo $$ > %s; exec sleep 300"], every: 1h, timeout: 300ms }
`, pidFile)))
	h.waitForCapability("tool.stuck", 1)
	if status := h.capabilityStatus("tool.stuck"); status.ReadyNodes != 0 {
		t.Fatalf("status = %+v", status)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	fmt.Sscanf(string(raw), "%d", &pid)
	eventually(t, 5*time.Second, "the stuck check to be killed", func() bool { return !processAlive(pid) })
}

func TestJobsOnlyGoToMachinesWhereTheCapabilityIsReady(t *testing.T) {
	h := newHarness(t)
	capability := func(check string) string {
		return anyObject("tool.shared", "cli", fmt.Sprintf("    command: [/bin/sh, -c, \"echo run >> @MARKERS@/ran\"]\n    check: { command: [/bin/sh, -c, %q], every: 100ms }\n", check))
	}
	broken := h.startWorker("broken", h.url, false, capability("exit 1"))
	working := h.startWorker("working", h.url, false, capability("exit 0"))
	h.waitForCapability("tool.shared", 2)
	h.waitReady("tool.shared", 1)

	for i := 0; i < 6; i++ {
		j := h.waitTerminal(h.runCapability("tool.shared", map[string]any{}).ID)
		if j.Status != "succeeded" || j.Node != "working" {
			t.Fatalf("job = %+v", j)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(broken.markerDir, "ran")); err == nil {
		t.Errorf("the machine that isn't ready ran jobs: %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(working.markerDir, "ran")); strings.Count(string(raw), "run") != 6 {
		t.Errorf("working ran: %q", raw)
	}

	// With a selector that only fits the broken machine, the job can't run. That's
	// reported as nothing being available, not as machines being busy.
	status, raw := h.request("POST", "/v1/jobs", h.apiKey, map[string]any{
		"capability": "tool.shared", "input": map[string]any{}, "selector": map[string]string{"name": "broken"}, "queue_timeout": "1s",
	})
	if status != http.StatusAccepted {
		t.Fatalf("%d %s", status, raw)
	}
	var waiting job
	json.Unmarshal(raw, &waiting)
	final := h.waitTerminal(waiting.ID)
	wantError(t, final, "failed", "NO_NODE_ONLINE", true)
}

func TestQueueTimeoutReportsNothingReady(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("tool.down", "cli", "    command: [/bin/sh, -c, \"exit 0\"]\n    check: { command: [/bin/sh, -c, \"exit 1\"] }\n"))
	h.waitForCapability("tool.down", 1)
	j := h.submit(map[string]any{"capability": "tool.down", "input": map[string]any{}, "queue_timeout": "500ms"})
	final := h.waitTerminal(j.ID)
	wantError(t, final, "failed", "NO_NODE_ONLINE", true)
	if len(final.Events) != 0 {
		t.Errorf("a machine that isn't ready was sent the job: %+v", final.Events)
	}
}

// A change that happens during an outage, when no message could be sent, must be
// known as soon as the worker is back.
func TestReadinessIsReportedAgainAfterReconnecting(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	h := newHarness(t)
	p := h.newProxy()
	h.startWorker("mac", p.url(), false, anyObject("svc.flaky", "http", fmt.Sprintf("    request: { url: \"%[1]s/run\" }\n    check: { url: \"%[1]s/health\", every: 100ms }\n", service.URL)))
	h.waitReady("svc.flaky", 1)

	p.cut()
	eventually(t, 15*time.Second, "the gateway to notice the cut", func() bool { return h.capabilityStatus("svc.flaky").OnlineNodes == 0 })
	// The machine last reported this as working, but it can't take jobs while it's gone.
	if status := h.capabilityStatus("svc.flaky"); status.ReadyNodes != 0 || status.Nodes[0].Ready {
		t.Errorf("an offline machine counts as ready: %+v", status)
	}
	healthy.Store(false)
	time.Sleep(500 * time.Millisecond) // Long enough for the worker to see it.
	p.restore()
	eventually(t, 45*time.Second, "the worker to reconnect", func() bool { return h.capabilityStatus("svc.flaky").OnlineNodes == 1 })
	if status := h.capabilityStatus("svc.flaky"); status.ReadyNodes != 0 {
		t.Fatalf("the change during the outage was lost: %+v", status)
	}

	// A gateway restart loses no state either, as the worker reports it again.
	h.stopGateway()
	h.startGateway()
	eventually(t, 45*time.Second, "the worker to reconnect", func() bool { return h.capabilityStatus("svc.flaky").OnlineNodes == 1 })
	if status := h.capabilityStatus("svc.flaky"); status.ReadyNodes != 0 {
		t.Fatalf("after a gateway restart: %+v", status)
	}
	healthy.Store(true)
	h.waitReady("svc.flaky", 1)
}

func TestDashboardShowsWhatIsntReady(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("tool.down", "cli", "    command: [/bin/sh, -c, \"exit 0\"]\n    check: { command: [/bin/sh, -c, \"exit 1\"] }\n"))
	h.waitForCapability("tool.down", 1)
	dashboard := h.dashboardKey()
	var view struct {
		Nodes []struct {
			Capabilities []string          `json:"capabilities"`
			NotReady     map[string]string `json:"not_ready"`
		} `json:"nodes"`
	}
	h.dashboardGet(dashboard, "overview", &view)
	if len(view.Nodes) != 1 || view.Nodes[0].NotReady["tool.down"] == "" || view.Nodes[0].NotReady["tts.synthesize"] != "" {
		t.Fatalf("view = %+v", view)
	}
	if !contains(view.Nodes[0].Capabilities, "tool.down") {
		t.Errorf("a capability that isn't ready should still be listed: %+v", view.Nodes[0].Capabilities)
	}
}

func TestCheckFlagReportsEachCapability(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, anyObject("tool.down", "cli", "    command: [/bin/sh, -c, \"exit 0\"]\n    check: { command: [/bin/sh, -c, \"exit 7\"] }\n"))
	w.stop()
	out, err := exec.Command(workerBin, "--config", w.config, "--check").CombinedOutput()
	// An installer runs this to validate a config, and a service that's merely down
	// right now mustn't stop an install.
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "ready: tts.synthesize") {
		t.Errorf("output: %s", text)
	}
	if !strings.Contains(text, "not ready: tool.down") || !strings.Contains(text, "exit status 7") {
		t.Errorf("output: %s", text)
	}
}
