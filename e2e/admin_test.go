package e2e

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

type adminKey struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Tools    []string `json:"tools"`
	Nodes    []string `json:"nodes"`
	Viewable bool     `json:"viewable"`
	Jobs     int      `json:"jobs"`
}

func (h *harness) adminKeys(admin string) map[string]adminKey {
	h.t.Helper()
	var body struct {
		Keys   []adminKey `json:"keys"`
		MCPURL string     `json:"mcp_url"`
	}
	h.dashboardGet(admin, "keys", &body)
	if !strings.HasSuffix(body.MCPURL, "/mcp") {
		h.t.Errorf("mcp_url = %q", body.MCPURL)
	}
	out := map[string]adminKey{}
	for _, k := range body.Keys {
		out[k.Name] = k
	}
	return out
}

func (h *harness) adminCall(admin, method, path string, body any, want int) []byte {
	h.t.Helper()
	status, raw := h.request(method, "/v1/dashboard/"+path, admin, body)
	if status != want {
		h.t.Fatalf("%s %s: %d, want %d: %s", method, path, status, want, raw)
	}
	return raw
}

func (h *harness) keySecret(admin, id string) string {
	h.t.Helper()
	var body struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(h.adminCall(admin, "GET", "keys/"+id+"/secret", nil, 200), &body); err != nil {
		h.t.Fatal(err)
	}
	return body.Secret
}

// capabilityNodes lists, per capability, the machines a key is shown.
func (h *harness) capabilityNodes(key string) map[string][]string {
	h.t.Helper()
	status, raw := h.request("GET", "/v1/capabilities", key, nil)
	if status != 200 {
		h.t.Fatalf("capabilities: %d %s", status, raw)
	}
	var body struct {
		Capabilities []struct {
			Name  string `json:"name"`
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		h.t.Fatal(err)
	}
	out := map[string][]string{}
	for _, c := range body.Capabilities {
		for _, n := range c.Nodes {
			out[c.Name] = append(out[c.Name], n.Name)
		}
		sort.Strings(out[c.Name])
	}
	return out
}

func TestOnlyAdminKeysCanManage(t *testing.T) {
	h := newHarness(t)
	viewer := h.dashboardKey()
	admin := h.credential("key", "add", "boss", "--admin")
	if !strings.HasPrefix(admin, "fd_") {
		t.Errorf("admin key = %q", admin[:3])
	}

	var overview struct {
		Viewer struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"viewer"`
	}
	h.dashboardGet(viewer, "overview", &overview)
	if overview.Viewer.Name != "ops" || overview.Viewer.Admin {
		t.Errorf("viewer = %+v", overview.Viewer)
	}
	h.dashboardGet(admin, "overview", &overview)
	if overview.Viewer.Name != "boss" || !overview.Viewer.Admin {
		t.Errorf("admin = %+v", overview.Viewer)
	}

	agentID := h.adminKeys(admin)["agent"].ID
	for _, attempt := range []struct{ method, path string }{
		{"GET", "keys"},
		{"POST", "keys"},
		{"GET", "keys/" + agentID + "/secret"},
		{"PUT", "keys/" + agentID},
		{"DELETE", "keys/" + agentID},
		{"DELETE", "nodes/mac"},
		{"POST", "nodes/mac/test"},
	} {
		for name, key := range map[string]string{"view-only": viewer, "agent": h.apiKey} {
			if status, raw := h.request(attempt.method, "/v1/dashboard/"+attempt.path, key, map[string]any{}); status != http.StatusForbidden {
				t.Errorf("%s key on %s %s: %d %s", name, attempt.method, attempt.path, status, raw)
			}
		}
	}

	// The key the harness made on the command line can be shown, and it's the same key.
	keys := h.adminKeys(admin)
	if k := keys["agent"]; k.Kind != "agent" || !k.Viewable || k.Tools[0] != "*" || k.Nodes[0] != "*" {
		t.Errorf("agent = %+v", k)
	}
	if got := h.keySecret(admin, agentID); got != h.apiKey {
		t.Errorf("shown key differs from the one made")
	}
	if keys["ops"].Kind != "dashboard" || keys["boss"].Kind != "admin" {
		t.Errorf("dashboard keys = %+v, %+v", keys["ops"], keys["boss"])
	}
	// Dashboard keys aren't shown again, and an admin can't lock itself out.
	h.adminCall(admin, "GET", "keys/"+keys["ops"].ID+"/secret", nil, http.StatusNotFound)
	h.adminCall(admin, "DELETE", "keys/"+keys["boss"].ID, nil, http.StatusConflict)
	h.adminCall(admin, "DELETE", "keys/"+keys["ops"].ID, nil, http.StatusNoContent)
	if status, _ := h.request("GET", "/v1/dashboard/overview", viewer, nil); status != http.StatusUnauthorized {
		t.Errorf("a revoked dashboard key still works: %d", status)
	}
}

func TestKeysAreLimitedToToolsAndMachines(t *testing.T) {
	h := newHarness(t)
	mac := h.startWorker("mac", h.url, false, testedCapability)
	h.startWorker("pc", h.url, false, testedCapability)
	h.waitForCapability("voice.tested", 2)
	admin := h.credential("key", "add", "boss", "--admin")

	raw := h.adminCall(admin, "POST", "keys", map[string]any{"name": "alice", "tools": []string{"tts.*"}, "nodes": []string{"mac"}}, http.StatusCreated)
	var created struct {
		ID, Name, Secret string
		MCPURL           string `json:"mcp_url"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "fk_") || created.Name != "alice" || created.MCPURL == "" {
		t.Fatalf("created = %s", raw)
	}
	if got := h.keySecret(admin, created.ID); got != created.Secret {
		t.Error("the key shown later differs from the one made")
	}
	alice := created.Secret

	// Names are checked, and can't be taken twice.
	h.adminCall(admin, "POST", "keys", map[string]any{"name": "alice"}, http.StatusConflict)
	h.adminCall(admin, "POST", "keys", map[string]any{"name": " "}, http.StatusBadRequest)
	h.adminCall(admin, "POST", "keys", map[string]any{"name": "fleet:mine"}, http.StatusBadRequest)
	h.adminCall(admin, "POST", "keys", map[string]any{"name": "bob", "tools": []string{"[bad"}}, http.StatusBadRequest)
	// Leaving the lists out means everything.
	h.adminCall(admin, "POST", "keys", map[string]any{"name": "carol"}, http.StatusCreated)
	if k := h.adminKeys(admin)["carol"]; len(k.Tools) != 1 || k.Tools[0] != "*" || len(k.Nodes) != 1 || k.Nodes[0] != "*" {
		t.Errorf("carol = %+v", k)
	}

	// Alice sees only her tools, and only on her machine.
	nodes := h.capabilityNodes(alice)
	if len(nodes) != 1 || strings.Join(nodes["tts.synthesize"], ",") != "mac" {
		t.Errorf("alice sees %v", nodes)
	}
	c := &mcpClient{t: t, url: h.url + "/mcp", key: alice}
	reply := c.call("tools/list", nil)
	var names []string
	for _, tool := range reply.Result["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Contains(strings.Join(names, ","), "voice") || !strings.Contains(strings.Join(names, ","), "tts_synthesize") {
		t.Errorf("alice's MCP tools = %v", names)
	}

	submit := func(key string, body map[string]any) (int, job) {
		t.Helper()
		status, raw := h.request("POST", "/v1/jobs", key, body)
		var j job
		_ = json.Unmarshal(raw, &j)
		return status, j
	}
	tts := map[string]any{"capability": "tts.synthesize", "input": map[string]any{"text": "hi", "steps": 1}}
	if status, _ := submit(alice, map[string]any{"capability": "voice.tested", "input": map[string]any{"text": "hi", "steps": 1}}); status != http.StatusForbidden {
		t.Errorf("a tool alice may not use: %d", status)
	}
	for i := 0; i < 3; i++ {
		status, j := submit(alice, tts)
		if status != http.StatusAccepted {
			t.Fatalf("submit: %d", status)
		}
		var done job
		eventually(t, 30*time.Second, "alice's job", func() bool {
			_, raw := h.request("GET", "/v1/jobs/"+j.ID, alice, nil)
			_ = json.Unmarshal(raw, &done)
			return done.Terminal
		})
		if done.Status != "succeeded" || done.Node != "mac" {
			t.Fatalf("alice's job ran on %q: %s", done.Node, done.Status)
		}
	}

	// With her machine gone, her job waits rather than going to another.
	mac.stop()
	eventually(t, 10*time.Second, "mac to go offline", func() bool {
		var view dashboardOverview
		h.dashboardGet(admin, "overview", &view)
		for _, n := range view.Nodes {
			if n.Name == "mac" {
				return !n.Online
			}
		}
		return false
	})
	_, waiting := submit(alice, map[string]any{"capability": "tts.synthesize", "input": map[string]any{"text": "hi", "steps": 1}, "queue_timeout": "30s"})
	time.Sleep(time.Second)
	_, raw = h.request("GET", "/v1/jobs/"+waiting.ID, alice, nil)
	var still job
	_ = json.Unmarshal(raw, &still)
	if still.Status != "queued" {
		t.Fatalf("alice's job went to %q: %s", still.Node, still.Status)
	}

	// Allowing every machine applies to her queued job only from then on, so it
	// keeps waiting; a new one runs on pc.
	h.adminCall(admin, "PUT", "keys/"+created.ID, map[string]any{"tools": []string{"tts.*"}, "nodes": []string{"*"}}, http.StatusNoContent)
	if nodes := h.capabilityNodes(alice); strings.Join(nodes["tts.synthesize"], ",") != "mac,pc" {
		t.Errorf("after the change alice sees %v", nodes)
	}
	_, j := submit(alice, tts)
	var done job
	eventually(t, 30*time.Second, "alice's job on pc", func() bool {
		_, raw := h.request("GET", "/v1/jobs/"+j.ID, alice, nil)
		_ = json.Unmarshal(raw, &done)
		return done.Terminal
	})
	if done.Status != "succeeded" || done.Node != "pc" {
		t.Errorf("job = %s on %q", done.Status, done.Node)
	}
	if k := h.adminKeys(admin)["alice"]; k.Jobs != 5 {
		t.Errorf("alice's jobs = %d", k.Jobs)
	}

	// Revoking takes effect at once, and the key can't be shown any more.
	h.adminCall(admin, "DELETE", "keys/"+created.ID, nil, http.StatusNoContent)
	if status, _ := h.request("GET", "/v1/capabilities", alice, nil); status != http.StatusUnauthorized {
		t.Errorf("revoked key: %d", status)
	}
	h.adminCall(admin, "GET", "keys/"+created.ID+"/secret", nil, http.StatusNotFound)
	if _, listed := h.adminKeys(admin)["alice"]; listed {
		t.Error("a revoked key is still listed")
	}
}

func TestDashboardRemovesOfflineMachinesAndRunsTests(t *testing.T) {
	h := newHarness(t)
	mac := h.startWorker("mac", h.url, false, testedCapability)
	h.waitReady("voice.tested", 1)
	admin := h.credential("key", "add", "boss", "--admin")

	var overview struct {
		Nodes []struct {
			Name     string   `json:"name"`
			Online   bool     `json:"online"`
			Testable []string `json:"testable"`
		} `json:"nodes"`
	}
	h.dashboardGet(admin, "overview", &overview)
	if len(overview.Nodes) != 1 || strings.Join(overview.Nodes[0].Testable, ",") != "voice.tested" {
		t.Errorf("overview = %+v", overview.Nodes)
	}

	// Testing everything runs what declares a test and says why the rest didn't run.
	var started struct {
		Tests []struct {
			Capability string `json:"capability"`
			Job        struct {
				ID string `json:"id"`
			} `json:"job"`
			Already bool `json:"already"`
		} `json:"tests"`
		Skipped []struct {
			Capability string `json:"capability"`
			Reason     string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(h.adminCall(admin, "POST", "nodes/mac/test", nil, http.StatusAccepted), &started); err != nil {
		t.Fatal(err)
	}
	if len(started.Tests) != 1 || started.Tests[0].Capability != "voice.tested" ||
		len(started.Skipped) != 1 || started.Skipped[0].Capability != "tts.synthesize" || !strings.Contains(started.Skipped[0].Reason, "doesn't declare a test") {
		t.Fatalf("started = %+v", started)
	}
	jobID := started.Tests[0].Job.ID
	// A second click while it runs follows the same test.
	var again struct {
		Tests []struct {
			Job struct {
				ID string `json:"id"`
			} `json:"job"`
			Already bool `json:"already"`
		} `json:"tests"`
	}
	status, raw := h.request("POST", "/v1/dashboard/nodes/mac/test", admin, map[string]any{"capability": "voice.tested"})
	if err := json.Unmarshal(raw, &again); err != nil || status != http.StatusAccepted || len(again.Tests) != 1 {
		t.Fatalf("again: %d %s", status, raw)
	}
	if again.Tests[0].Already && again.Tests[0].Job.ID != jobID {
		t.Errorf("following a running test gave another job")
	}
	h.adminCall(admin, "POST", "nodes/mac/test", map[string]any{"capability": "tts.synthesize"}, http.StatusUnprocessableEntity)
	h.adminCall(admin, "POST", "nodes/mac/test", map[string]any{"capability": "nope"}, http.StatusNotFound)

	var detail struct {
		Status       string `json:"status"`
		Test         bool   `json:"test"`
		SubmittedBy  string `json:"submitted_by"`
		TestExitCode *int   `json:"test_exit_code"`
	}
	eventually(t, 30*time.Second, "the test to finish", func() bool {
		h.dashboardGet(admin, "jobs/"+jobID, &detail)
		return detail.Status == "succeeded"
	})
	if !detail.Test || detail.SubmittedBy != "boss" || detail.TestExitCode == nil || *detail.TestExitCode != 0 {
		t.Errorf("detail = %+v", detail)
	}
	if last := h.offerTest("voice.tested", "mac").LastTest; last == nil || last.Status != "succeeded" {
		t.Errorf("last test = %+v", last)
	}

	// A connected machine can't be removed.
	h.adminCall(admin, "DELETE", "nodes/mac", nil, http.StatusConflict)
	h.adminCall(admin, "DELETE", "nodes/nowhere", nil, http.StatusNotFound)

	mac.stop()
	eventually(t, 10*time.Second, "mac to go offline", func() bool {
		h.dashboardGet(admin, "overview", &overview)
		return len(overview.Nodes) == 1 && !overview.Nodes[0].Online
	})
	h.adminCall(admin, "POST", "nodes/mac/test", nil, http.StatusConflict)
	h.adminCall(admin, "DELETE", "nodes/mac", nil, http.StatusNoContent)
	h.dashboardGet(admin, "overview", &overview)
	if len(overview.Nodes) != 0 {
		t.Errorf("removed machine is still listed: %+v", overview.Nodes)
	}
	if nodes := h.capabilityNodes(h.apiKey); len(nodes) != 0 {
		t.Errorf("its tools are still offered: %v", nodes)
	}

	// Its token no longer works.
	mac.start()
	time.Sleep(2 * time.Second)
	h.dashboardGet(admin, "overview", &overview)
	if len(overview.Nodes) != 0 {
		t.Errorf("a removed machine came back: %+v", overview.Nodes)
	}
}
