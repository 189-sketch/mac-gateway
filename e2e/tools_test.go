package e2e

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tools here are small sh scripts written into the manifest. The script is fixed
// text, and job input reaches it only as separate arguments.
func declaredTools(cwd string) string {
	return fmt.Sprintf(`
  - name: sh.args
    description: Prints the arguments it was given, one per line.
    adapter: cli
    command: [/bin/sh, -c, 'printf "%%s\n" "$@"; echo to-stderr >&2', sh]
    args:
      - "{{input.target}}"
      - { flag: --model, value: "{{input.model}}" }
      - { flag: --fast, when: input.fast }
      - { flag: --file, each: input.files }
    input_schema:
      type: object
      required: [target]
      properties:
        target: { type: string }
        model: { type: string }
        fast: { type: boolean }
        files: { type: array, items: { type: string } }
    result: { from: stdout, format: text }
    timeout: 1m
  - name: sh.json
    adapter: cli
    command: [/bin/sh, -c, 'printf "{\"answer\": {\"n\": %%s}}\n" "$1"', sh, "{{input.n}}"]
    input_schema:
      type: object
      properties: { n: { type: integer, default: 7 } }
    result: { format: json, field: answer.n }
    timeout: 1m
  - name: sh.exit
    adapter: cli
    command: [/bin/sh, -c, 'echo report line; echo problem >&2; exit "$1"', sh, "{{input.code}}"]
    input_schema:
      type: object
      required: [code]
      properties: { code: { type: integer } }
    success_exit_codes: [0, 3]
    result: { from: stdout }
    timeout: 1m
  - name: sh.place
    adapter: cli
    command: [/bin/sh, -c, 'pwd; echo "$GREETING"; echo "$FROM_WORKER"']
    cwd: %s
    env:
      GREETING: "hi {{input.name}}"
      FROM_WORKER: "{{env.FLEET_SENTINEL}}"
    input_schema:
      type: object
      required: [name]
      properties: { name: { type: string } }
    result: { from: stdout }
    timeout: 1m
  - name: sh.files
    adapter: cli
    command: [/bin/sh, -c, 'mkdir -p report/sub && echo ok > report/summary.txt && echo d > report/sub/data.txt && echo made']
    input_schema: { type: object }
    outputs:
      report: { path: report, archive: zip }
      extra: { path: missing.txt, optional: true }
    timeout: 1m
`, cwd)
}

func (h *harness) runAndWait(name string, input map[string]any) job {
	h.t.Helper()
	return h.waitTerminal(h.runCapability(name, input).ID)
}

func TestToolResultsAndOutputReachTheCaller(t *testing.T) {
	h := newHarness(t)
	cwd := filepath.Join(h.dir, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	h.startWorker("mac", h.url, false, declaredTools(cwd))
	h.waitReady("sh.files", 1)

	// Arguments: only what the input gives, and what the command prints comes back.
	j := h.runAndWait("sh.args", map[string]any{"target": "a b", "model": "m", "fast": true, "files": []string{"x", "y"}})
	var text string
	if j.Status != "succeeded" || json.Unmarshal(j.Result, &text) != nil {
		t.Fatalf("sh.args: %s %s %+v", j.Status, j.Result, j.Error)
	}
	if want := "a b\n--model\nm\n--fast\n--file\nx\n--file\ny"; text != want {
		t.Errorf("result = %q, want %q", text, want)
	}
	if j.ExitCode == nil || *j.ExitCode != 0 || !strings.Contains(j.Output, "to-stderr") || !strings.Contains(j.Output, "--model") {
		t.Errorf("exit code %v, output %q", j.ExitCode, j.Output)
	}
	j = h.runAndWait("sh.args", map[string]any{"target": "only", "fast": false})
	if json.Unmarshal(j.Result, &text) != nil || text != "only" {
		t.Errorf("optional arguments were passed: %q", text)
	}

	// JSON, one field of it, and a schema default for a field the caller left out.
	if j := h.runAndWait("sh.json", map[string]any{"n": 42}); string(j.Result) != "42" {
		t.Errorf("json field = %s %+v", j.Result, j.Error)
	}
	if j := h.runAndWait("sh.json", map[string]any{}); string(j.Result) != "7" {
		t.Errorf("default = %s %+v", j.Result, j.Error)
	}

	// An exit code listed as success is a result. Any other is a failure that still
	// says what the command printed, and how it exited.
	j = h.runAndWait("sh.exit", map[string]any{"code": 3})
	if j.Status != "succeeded" || j.ExitCode == nil || *j.ExitCode != 3 || !strings.Contains(string(j.Result), "report line") {
		t.Errorf("exit 3: %s %v %s %+v", j.Status, j.ExitCode, j.Result, j.Error)
	}
	j = h.runAndWait("sh.exit", map[string]any{"code": 4})
	wantError(t, j, "failed", "COMMAND_FAILED", false)
	if j.ExitCode == nil || *j.ExitCode != 4 || !strings.Contains(j.Output, "report line") || !strings.Contains(j.Output, "problem") {
		t.Errorf("exit 4: exit code %v, output %q", j.ExitCode, j.Output)
	}

	// The folder it runs in, and env values from the input and from the worker's
	// own environment. Text in the input is never read as a placeholder.
	j = h.runAndWait("sh.place", map[string]any{"name": "{{env.FLEET_SENTINEL}}"})
	if json.Unmarshal(j.Result, &text) != nil || text != cwd+"\nhi {{env.FLEET_SENTINEL}}\npresent" {
		t.Errorf("sh.place = %q %+v", text, j.Error)
	}

	// A folder comes back as a zip, and an optional file that wasn't made is skipped.
	j = h.runAndWait("sh.files", map[string]any{})
	if j.Status != "succeeded" || len(j.Artifacts) != 1 || j.Artifacts[0].Name != "report.zip" || !strings.Contains(j.Output, "made") {
		t.Fatalf("sh.files: %s %+v %+v", j.Status, j.Artifacts, j.Error)
	}
	zipped := h.artifact(j)
	reader, err := zip.NewReader(bytes.NewReader([]byte(zipped)), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range reader.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	if want := []string{"sub/", "sub/data.txt", "summary.txt"}; !slices.Equal(names, want) {
		t.Errorf("zip has %q, want %q", names, want)
	}

	// Agents see each tool's description.
	_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
	if !strings.Contains(string(raw), `"description":"Prints the arguments it was given, one per line."`) {
		t.Errorf("no description in %s", raw)
	}

	// The dashboard can't see what a job printed, as it can't see its result.
	status, raw := h.request("GET", "/v1/dashboard/jobs/"+j.ID, h.dashboardKey(), nil)
	if status != http.StatusOK || strings.Contains(string(raw), "made") {
		t.Errorf("dashboard: %d %s", status, raw)
	}
}

const liveTool = `name: live.echo
description: %s
adapter: cli
command: [/bin/echo, live]
input_schema: { type: object }
result: { from: stdout }
`

func (h *harness) capabilityNames() []string {
	h.t.Helper()
	_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
	var body struct {
		Capabilities []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			OnlineNodes int    `json:"online_nodes"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, c := range body.Capabilities {
		if c.OnlineNodes > 0 {
			names = append(names, c.Name+":"+c.Description)
		}
	}
	return names
}

func (h *harness) offered(name string) bool {
	for _, entry := range h.capabilityNames() {
		if strings.HasPrefix(entry, name+":") {
			return true
		}
	}
	return false
}

// Tools added, changed or removed while the worker runs reach the gateway within
// seconds, without a restart, and without disturbing a job already running.
func TestToolsFolderChangesApplyWhileRunning(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitReady("tts.synthesize", 1)
	tools := filepath.Join(filepath.Dir(w.config), "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tools, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	long := h.submitTTS("still running", 20)
	h.waitStatus(long.ID, "running")
	write("live.yaml", fmt.Sprintf(liveTool, "First version."))
	eventually(t, 15*time.Second, "the new tool to be offered", func() bool { return h.offered("live.echo") })
	if j := h.runAndWait("live.echo", map[string]any{}); string(j.Result) != `"live"` {
		t.Errorf("new tool: %s %+v", j.Result, j.Error)
	}
	if done := h.waitTerminal(long.ID); done.Status != "succeeded" || done.Attempts != 1 {
		t.Errorf("the running job was disturbed: %s after %d attempts", done.Status, done.Attempts)
	}

	// A broken file is skipped, and everything else carries on.
	write("broken.yaml", "name: live.broken\nadapter: cli\ncommand: [/no/such/program]\ninput_schema: { type: object }\n")
	write("off.yaml", "name: live.off\nenabled: false\nadapter: cli\ncommand: [/bin/echo]\ninput_schema: { type: object }\n")
	write("live.yaml", fmt.Sprintf(liveTool, "Second version."))
	eventually(t, 15*time.Second, "the new description", func() bool { return slices.Contains(h.capabilityNames(), "live.echo:Second version.") })
	if h.offered("live.broken") || h.offered("live.off") {
		t.Errorf("offered a broken or disabled tool: %q", h.capabilityNames())
	}
	eventually(t, 5*time.Second, "the skipped file to be logged", func() bool {
		raw, _ := os.ReadFile(h.logs.Name())
		return strings.Contains(string(raw), "skipped a tool file that isn't valid") && strings.Contains(string(raw), "broken.yaml")
	})
	out, err := exec.Command(workerBin, "--config", w.config, "--check").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "invalid: ") || !strings.Contains(string(out), "disabled: live.off") {
		t.Errorf("--check with a broken tool file: %v\n%s", err, out)
	}

	// A worker.yaml that stops being valid leaves the worker as it was.
	original, err := os.ReadFile(w.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.config, append(original, []byte("\nnot_a_setting: true\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the bad manifest to be logged", func() bool {
		raw, _ := os.ReadFile(h.logs.Name())
		return strings.Contains(string(raw), "the changed configuration isn't valid")
	})
	if !h.offered("live.echo") || !h.offered("tts.synthesize") {
		t.Errorf("a bad manifest took tools away: %q", h.capabilityNames())
	}
	if err := os.WriteFile(w.config, original, 0o600); err != nil {
		t.Fatal(err)
	}

	// Removing a file takes its tool away.
	if err := os.Remove(filepath.Join(tools, "live.yaml")); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "the removed tool to go", func() bool { return !h.offered("live.echo") })
	if !h.offered("tts.synthesize") {
		t.Error("removing one tool took another")
	}
}

// A template, filled in, becomes a tool agents can call and test.
func TestTemplateBecomesAWorkingTool(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitReady("tts.synthesize", 1)

	out, err := exec.Command(workerBin, "tools", "new", "made.shout", "--template", "shell", "--config", w.config).CombinedOutput()
	if err != nil {
		t.Fatalf("tools new: %v\n%s", err, out)
	}
	path := filepath.Join(filepath.Dir(w.config), "tools", "made.shout.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(h.dir, "shout.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho \"${1^^}!\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Do what a person would: fill in the TODOs and enable it.
	filled := strings.NewReplacer(
		"enabled: false", "enabled: true",
		"TODO/path/to/script.sh", script,
		"TODO/path/to/folder", h.dir,
		"description: TODO Say what this does and when to use it. Agents read this to pick a tool.", "description: Shouts the text back.",
		"target: TODO a value that is safe to run again and again", "target: hello",
	).Replace(string(raw))
	for _, line := range strings.Split(filled, "\n") {
		if !strings.HasPrefix(line, "#") && strings.Contains(line, "TODO") {
			t.Fatalf("the test missed a TODO: %s", line)
		}
	}
	if err := os.WriteFile(path, []byte(filled), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "the template's tool to be offered", func() bool { return h.offered("made.shout") })
	h.waitReady("made.shout", 1)

	if j := h.runAndWait("made.shout", map[string]any{"target": "quiet"}); string(j.Result) != `"QUIET!"` {
		t.Errorf("result = %s %+v", j.Result, j.Error)
	}
	status, started := h.testCapability("made.shout", nil)
	if status != http.StatusAccepted || len(started.Tests) != 1 {
		t.Fatalf("test: %d %+v", status, started)
	}
	if done := h.waitTerminal(started.Tests[0].Job.ID); done.Status != "succeeded" || string(done.Result) != `"HELLO!"` {
		t.Errorf("the template's test: %s %s", done.Status, done.Result)
	}
}
