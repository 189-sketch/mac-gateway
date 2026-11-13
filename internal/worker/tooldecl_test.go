package worker

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// writeWorker writes a minimal worker.yaml with these tools, and any tool files.
func writeWorker(t *testing.T, tools string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	body := "gateway_url: https://fleet.example.com\ntoken_file: token\nstate_dir: state\nnode: { name: mac, resources: { lock: 1 } }\n" + tools
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(files) > 0 {
		if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, "tools", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func loadTool(t *testing.T, tool string) (*Capability, error) {
	t.Helper()
	m, err := LoadManifest(writeWorker(t, "tools:\n"+tool, nil))
	if err != nil {
		return nil, err
	}
	return &m.Capabilities[0], nil
}

const echoArgsTool = `  - name: echo.args
    adapter: cli
    command: [/bin/echo, start]
    args:
      - "{{input.target}}"
      - { flag: --model, value: "{{input.model}}" }
      - { flag: --fast, when: input.fast }
      - { flag: --file, each: input.files }
      - { each: input.tags, value: "tag={{item}}" }
      - { value: "--size={{input.size}}" }
    input_schema:
      type: object
      required: [target]
      properties:
        target: { type: string }
        model: { type: string }
        fast: { type: boolean }
        files: { type: array, items: { type: string } }
        tags: { type: array }
        size: { type: integer, default: 3 }
`

func TestArgsAddOnlyWhatTheInputGives(t *testing.T) {
	c, err := loadTool(t, echoArgsTool)
	if err != nil {
		t.Fatal(err)
	}
	render := func(input map[string]any) []string {
		t.Helper()
		raw, _ := json.Marshal(input)
		var decoded map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		c.applyDefaults(decoded)
		args, err := renderArgs(c.Args, templateVars{input: decoded})
		if err != nil {
			t.Fatal(err)
		}
		return args
	}

	if got := render(map[string]any{"target": "a"}); !slices.Equal(got, []string{"a", "--size=3"}) {
		t.Errorf("only required = %q", got)
	}
	got := render(map[string]any{"target": "a", "model": "m", "fast": true, "files": []string{"x", "y"}, "tags": []any{"t", 2, true}, "size": 9})
	want := []string{"a", "--model", "m", "--fast", "--file", "x", "--file", "y", "tag=t", "tag=2", "tag=true", "--size=9"}
	if !slices.Equal(got, want) {
		t.Errorf("everything = %q, want %q", got, want)
	}
	if got := render(map[string]any{"target": "a", "fast": false, "model": nil, "files": []string{}}); !slices.Equal(got, []string{"a", "--size=3"}) {
		t.Errorf("false, null and empty = %q", got)
	}
	// A value that would be read as an option is refused, in a list too.
	for _, input := range []map[string]any{{"target": "-rf"}, {"target": "a", "files": []any{"--delete"}}} {
		if _, err := renderArgs(c.Args, templateVars{input: input}); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
			t.Errorf("%v: error = %v", input, err)
		}
	}
	if _, err := renderArgs(c.Args, templateVars{input: map[string]any{"target": "a", "files": "x"}}); err == nil {
		t.Error("each accepted a value that isn't a list")
	}
}

func TestDeclarationsAreCheckedAtStartup(t *testing.T) {
	base := func(extra string) string {
		return `  - name: t
    adapter: cli
    command: [/bin/echo]
    input_schema:
      type: object
      required: [a]
      properties: { a: { type: string }, b: { type: string }, c: { type: string, default: x } }
` + extra
	}
	for name, tc := range map[string]struct{ tool, want string }{
		"optional field always used": {base("    args: [\"{{input.b}}\"]\n"), "must be required or have a default"},
		"optional field in stdin":    {base("    stdin: \"{{input.b}}\"\n"), "stdin always uses"},
		"optional field in env":      {base("    env: { X: \"{{input.b}}\" }\n"), "env.X always uses"},
		"unknown field":              {base("    args: [{ flag: --z, value: \"{{input.z}}\" }]\n"), "isn't in input_schema.properties"},
		"unknown field always used":  {base("    args: [\"{{input.z}}\"]\n"), "{{input.z}}, which isn't in input_schema.properties"},
		"unknown when":               {base("    args: [{ flag: --z, when: input.z }]\n"), "isn't in input_schema.properties"},
		"placeholder in flag":        {base("    args: [{ flag: \"--{{input.a}}\" }]\n"), "can't contain placeholders"},
		"item outside each":          {base("    args: [\"{{item}}\"]\n"), "{{item}}"},
		"when and each":              {base("    args: [{ flag: --z, when: input.a, each: input.a }]\n"), "when or each"},
		"empty item":                 {base("    args: [{}]\n"), "flag, value or both"},
		"unknown key in item":        {base("    args: [{ flag: --z, values: x }]\n"), "values not found"},
		"bad env name":               {base("    env: { \"1X\": y }\n"), "valid variable name"},
		"node token in env":          {base("    env: { X: \"{{env.FLEET_NODE_TOKEN}}\" }\n"), "node token"},
		"placeholder in cwd":         {base("    cwd: \"{{input.a}}\"\n"), "cwd can't"},
		"result from nowhere":        {base("    result: { from: stderr }\n"), "must be stdout or file"},
		"result file without path":   {base("    result: { from: file }\n"), "result.path"},
		"result path outside":        {base("    result: { from: file, path: ../x }\n"), "inside the job's"},
		"field on text":              {base("    result: { field: a }\n"), "result.field"},
		"tail lines on json":         {base("    result: { format: json, tail_lines: 5 }\n"), "tail_lines"},
		"negative exit code":         {base("    success_exit_codes: [-1]\n"), "isn't an exit code"},
		"archive that isn't zip":     {base("    outputs: { r: { path: out, archive: tar } }\n"), "must be zip"},
		"unknown key in output":      {base("    outputs: { r: { path: out, zip: true } }\n"), "zip not found"},
		"description too long":       {base("    description: " + strings.Repeat("x", 2001) + "\n"), "description is longer"},
		"args on a service": {`  - name: t
    adapter: http
    request: { url: "http://127.0.0.1:9/x" }
    args: [a]
    input_schema: { type: object }
`, "args doesn't apply"},
	} {
		if _, err := loadTool(t, tc.tool); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
	}
	// What the rules allow.
	for name, tool := range map[string]string{
		"required field":        base("    args: [\"{{input.a}}\"]\n"),
		"field with a default":  base("    args: [\"{{input.c}}\"]\n    env: { C: \"{{input.c}}\" }\n"),
		"optional field in arg": base("    args: [{ flag: --b, value: \"{{input.b}}\" }]\n"),
		"no properties to check": `  - name: t
    adapter: cli
    command: [/bin/echo, "{{input.anything}}"]
    input_schema: { type: object }
`,
	} {
		if _, err := loadTool(t, tool); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Text from a job's input must never be read as a placeholder for the worker's
// own environment, which could hold secrets.
func TestEnvValuesAreFilledInOnePass(t *testing.T) {
	t.Setenv("FLEET_TEST_SECRET", "s3cret")
	env, err := renderEnv(map[string]string{"KEY": "{{env.FLEET_TEST_SECRET}}", "TEXT": "{{input.text}}"},
		templateVars{input: map[string]any{"text": "{{env.FLEET_TEST_SECRET}}"}})
	if err != nil {
		t.Fatal(err)
	}
	if env["KEY"] != "s3cret" || env["TEXT"] != "{{env.FLEET_TEST_SECRET}}" {
		t.Errorf("env = %v", env)
	}
	if _, err := renderEnv(map[string]string{"KEY": "{{env.FLEET_TEST_UNSET}}"}, templateVars{}); err == nil {
		t.Error("a missing variable was accepted")
	}
	// Outside env, {{env.NAME}} is never filled in, even with the same template.
	if _, err := render("{{env.FLEET_TEST_SECRET}}", templateVars{input: map[string]any{}}); err == nil {
		t.Error("render read the environment outside env")
	}
}

func TestCommandResults(t *testing.T) {
	text := &ResultConfig{Format: formatText}
	if got, _ := commandResult(text, []byte("hello\n\n"), false); string(got) != `"hello"` {
		t.Errorf("text = %s", got)
	}
	got, _ := commandResult(&ResultConfig{Format: formatText, TailLines: 2}, []byte("a\nb\nc\nd\n"), false)
	if string(got) != `"[earlier output was cut]\nc\nd"` {
		t.Errorf("tail lines = %s", got)
	}
	jsonField := &ResultConfig{Format: formatJSON, Field: "data.answer"}
	if got, failure := commandResult(jsonField, []byte(`{"data":{"answer":42}}`+"\n"), false); failure != nil || string(got) != "42" {
		t.Errorf("json field = %s %v", got, failure)
	}
	if _, failure := commandResult(jsonField, []byte(`{"data":{}}`), false); failure == nil || !strings.Contains(failure.Message, `"data.answer"`) {
		t.Errorf("missing field = %v", failure)
	}
	if _, failure := commandResult(&ResultConfig{Format: formatJSON}, []byte("not json"), false); failure == nil {
		t.Error("accepted output that isn't JSON")
	}

	// Long text keeps its end, starting at a whole line. Long JSON fails.
	capture := &outputCapture{limit: 20, keepEnd: true}
	capture.Write([]byte("first line is long\nsecond\nthird line\nend\n"))
	got, _ = commandResult(text, capture.data, capture.cut())
	if string(got) != `"[earlier output was cut]\nthird line\nend"` {
		t.Errorf("cut text = %s", got)
	}
	// The part kept is valid JSON by itself, which is what makes cutting dangerous.
	head := &outputCapture{limit: 8}
	head.Write([]byte(`{"a":1}    {"b":2}`))
	if _, failure := commandResult(&ResultConfig{Format: formatJSON}, head.data, head.cut()); failure == nil {
		t.Error("accepted cut-off JSON")
	}
}

func TestOutputKeepsTheEnd(t *testing.T) {
	tail := &logTail{}
	for i := range 10000 {
		tail.add(strings.Repeat("x", 20) + string(rune('a'+i%26)))
	}
	tail.add("the last line")
	if tail.keptBytes > maxOutputBytes || len(tail.kept) > maxOutputBytes/20 {
		t.Errorf("kept %d bytes in %d lines, so memory isn't bounded", tail.keptBytes, len(tail.kept))
	}
	out := tail.output()
	if len(out) > maxOutputBytes+len(cutNote) || !strings.HasPrefix(out, cutNote) || !strings.HasSuffix(out, "\nthe last line") {
		t.Errorf("output is %d bytes: %q...%q", len(out), out[:40], out[len(out)-20:])
	}
	if lines := strings.Split(tail.String(), "\n"); len(lines) != logTailLines {
		t.Errorf("the error message keeps %d lines", len(lines))
	}
}

func TestFolderOutputsAreZippedWithoutFollowingLinks(t *testing.T) {
	work := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(t.TempDir(), "secret")
	must(os.WriteFile(secret, []byte("private"), 0o600))
	must(os.MkdirAll(filepath.Join(work, "report", "nested", "empty"), 0o700))
	must(os.WriteFile(filepath.Join(work, "report", "summary.txt"), []byte("ok"), 0o600))
	must(os.WriteFile(filepath.Join(work, "report", "nested", "data.json"), []byte("{}"), 0o600))
	must(os.Symlink(secret, filepath.Join(work, "report", "link")))

	root, err := os.OpenRoot(work)
	must(err)
	defer root.Close()
	out, name, err := archiveOutput(root, "report", filepath.Join(t.TempDir(), archivesDir), "report")
	must(err)
	defer out.Close()
	reader, err := zip.OpenReader(filepath.Join(out.Name(), name))
	must(err)
	defer reader.Close()
	var names []string
	for _, f := range reader.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	want := []string{"nested/", "nested/data.json", "nested/empty/", "summary.txt"}
	if !slices.Equal(names, want) {
		t.Errorf("zip has %q, want %q", names, want)
	}

	// A single file can be zipped too, and a link can't be.
	_, name, err = archiveOutput(root, "report/summary.txt", filepath.Join(t.TempDir(), archivesDir), "one")
	if err != nil || name != "one.zip" {
		t.Errorf("file: %s %v", name, err)
	}
	if _, _, err := archiveOutput(root, "report/link", filepath.Join(t.TempDir(), archivesDir), "link"); err == nil {
		t.Error("zipped a link")
	}
}

func TestToolsFolder(t *testing.T) {
	tool := func(name, extra string) string {
		return "name: " + name + "\nadapter: cli\ncommand: [/bin/echo]\ninput_schema: { type: object }\n" + extra
	}
	path := writeWorker(t, "tools:\n  - name: main.one\n    adapter: cli\n    command: [/bin/echo]\n    input_schema: { type: object }\ncapabilities:\n  - name: main.two\n    adapter: cli\n    command: [/bin/echo]\n    input_schema: { type: object }\n", map[string]string{
		"good.yaml":    tool("file.good", "description: From a file.\n"),
		"also.yml":     tool("file.also", ""),
		"off.yaml":     tool("file.off", "enabled: false\ncwd: \"{{input.not_checked_because_disabled}}\"\n"),
		"broken.yaml":  tool("file.broken", "command: [/no/such/program]\n"),
		"unknown.yaml": tool("file.unknown", "colour: blue\n"),
		"dup.yaml":     tool("main.one", ""),
		"notes.txt":    "ignored",
		".hidden.yaml": "ignored: true",
	})
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(m)
	slices.Sort(names)
	if want := []string{"file.also", "file.good", "main.one", "main.two"}; !slices.Equal(names, want) {
		t.Errorf("tools = %q, want %q", names, want)
	}
	if m.byName["file.good"].Description != "From a file." || m.byName["file.good"] != &m.Capabilities[slices.IndexFunc(m.Capabilities, func(c Capability) bool { return c.Name == "file.good" })] {
		t.Error("byName doesn't point at the tool")
	}
	// enabled: false skips a tool before its settings are checked, so a template can
	// sit half-made. Its fields must still be ones a tool can have.
	if !slices.Equal(m.Disabled, []string{"file.off"}) {
		t.Errorf("disabled = %q", m.Disabled)
	}
	var bad []string
	for _, e := range m.ToolErrors {
		bad = append(bad, filepath.Base(e.Path))
	}
	slices.Sort(bad)
	if want := []string{"broken.yaml", "dup.yaml", "unknown.yaml"}; !slices.Equal(bad, want) {
		t.Errorf("skipped = %q, want %q", bad, want)
	}

	// A worker with no tools yet is valid, so tools can be added while it runs.
	if m, err := LoadManifest(writeWorker(t, "", nil)); err != nil || len(m.Capabilities) != 0 {
		t.Errorf("no tools: %v", err)
	}
	// An invalid tool in worker.yaml itself still stops the worker.
	if _, err := LoadManifest(writeWorker(t, "tools:\n  - name: x\n    adapter: cli\n    command: [/no/such]\n    input_schema: { type: object }\n", nil)); err == nil {
		t.Error("accepted an invalid tool in worker.yaml")
	}
}

func TestReloadOnlyReconnectsForChangesTheGatewaySees(t *testing.T) {
	load := func(description, command string) *Manifest {
		t.Helper()
		m, err := LoadManifest(writeWorker(t, "tools:\n  - name: a\n    description: "+description+"\n    adapter: cli\n    command: ["+command+"]\n    input_schema: { type: object }\n", nil))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if !sameHello(load("x", "/bin/echo"), load("x", "/bin/cat")) {
		t.Error("a new command needs no reconnect")
	}
	if sameHello(load("x", "/bin/echo"), load("y", "/bin/echo")) {
		t.Error("a new description must reach the gateway")
	}
	relabelled := load("x", "/bin/echo")
	relabelled.Node.Labels = map[string]string{"gpu": "yes"}
	if sameHello(load("x", "/bin/echo"), relabelled) {
		t.Error("new labels must reach the gateway")
	}
	path := writeWorker(t, "", nil)
	a, _ := LoadManifest(path)
	b, _ := LoadManifest(path)
	// The gateway can change while running, which is how pairing takes effect.
	b.GatewayURL, b.LogLevel = "https://other.example.com", "debug"
	if got := fixedChanges(a, b); !slices.Equal(got, []string{"log_level"}) {
		t.Errorf("fixed changes = %q", got)
	}
}

// Each template, filled in, must be a valid tool. Values a person would choose
// are swapped for ones that exist on this machine.
func TestTemplatesAreValidOnceFilledIn(t *testing.T) {
	all := Templates("")
	if len(all) < 8 {
		t.Fatalf("only %d templates", len(all))
	}
	todo := regexp.MustCompile(`TODO[A-Za-z0-9_./\\-]*`)
	for _, tpl := range all {
		if tpl.Title == "" || tpl.Description == "" || !strings.Contains(tpl.Body, "enabled: false") || strings.Contains(tpl.Body, "# template:") {
			t.Errorf("%s: header or body is wrong", tpl.Name)
		}
		body := strings.ReplaceAll(tpl.Body, templateNameToken, "made."+strings.ReplaceAll(tpl.Name, "-", "."))
		body = strings.Replace(body, "enabled: false", "enabled: true", 1)
		// Programs become ones this machine has, and folders an existing one.
		body = regexp.MustCompile(`command: \[[^,\]]+`).ReplaceAllString(body, "command: [/bin/echo")
		body = strings.ReplaceAll(body, "cwd: TODO/path/to/project", "cwd: /tmp")
		body = strings.ReplaceAll(body, "cwd: TODO/path/to/folder", "cwd: /tmp")
		body = strings.ReplaceAll(body, ":TODO_PORT", ":8080")
		body = todo.ReplaceAllString(body, "filled")
		path := writeWorker(t, "", map[string]string{"t.yaml": body})
		m, err := LoadManifest(path)
		if err != nil {
			t.Errorf("%s: %v", tpl.Name, err)
			continue
		}
		if len(m.ToolErrors) > 0 || len(m.Capabilities) != 1 {
			t.Errorf("%s: %v\n%s", tpl.Name, m.ToolErrors, body)
			continue
		}
		if c := m.Capabilities[0]; c.Description == "" || c.Test == nil && tpl.Name != "http-async" {
			t.Errorf("%s: needs a description and a test", tpl.Name)
		}
	}

	names := func(goos string) []string {
		var out []string
		for _, tpl := range Templates(goos) {
			out = append(out, tpl.Name)
		}
		return out
	}
	if got := names("linux"); slices.Contains(got, "ios-test") || slices.Contains(got, "powershell") || !slices.Contains(got, "shell") {
		t.Errorf("linux templates = %q", got)
	}
	if got := names("windows"); !slices.Contains(got, "powershell") || slices.Contains(got, "shell") {
		t.Errorf("windows templates = %q", got)
	}
	if got := names("darwin"); !slices.Contains(got, "ios-test") || !slices.Contains(got, "claude-code") {
		t.Errorf("mac templates = %q", got)
	}
}

func TestNewToolFile(t *testing.T) {
	path := writeWorker(t, "tools:\n  - name: taken\n    adapter: cli\n    command: [/bin/echo]\n    input_schema: { type: object }\n", nil)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	made, err := NewToolFile(m, "cli", "my.tool", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(made); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file: %v %v", info, err)
	}
	// It starts disabled, so the worker loads it without complaint.
	if m, err = LoadManifest(path); err != nil || !slices.Equal(m.Disabled, []string{"my.tool"}) || len(m.ToolErrors) > 0 {
		t.Errorf("after creating: %v %v %v", err, m.Disabled, m.ToolErrors)
	}
	for name, tc := range map[string]struct{ template, tool, goos, want string }{
		"existing file":    {"cli", "my.tool", "linux", "already exists"},
		"existing tool":    {"cli", "taken", "linux", "already exists"},
		"bad name":         {"cli", "My Tool", "linux", "lowercase"},
		"unknown template": {"nope", "x", "linux", "no template"},
		"other platform's": {"ios-test", "x", "linux", "no template"},
	} {
		if _, err := NewToolFile(m, tc.template, tc.tool, tc.goos); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestResultFromAFile(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "answer.json"), []byte(`{"text":"hi"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, failure := fileResult(&ResultConfig{From: resultFromFile, Path: "answer.json", Format: formatJSON, Field: "text"}, work)
	if failure != nil || string(got) != `"hi"` {
		t.Errorf("result = %s %v", got, failure)
	}
	if _, failure := fileResult(&ResultConfig{From: resultFromFile, Path: "missing.json", Format: formatJSON}, work); failure == nil || failure.Code != "OUTPUT_MISSING" {
		t.Errorf("missing file = %v", failure)
	}
}

// A tool whose folder has gone stops being offered until it's back.
func TestMissingWorkingFolderMeansNotReady(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := loadTool(t, "  - name: t\n    adapter: cli\n    command: [/bin/echo]\n    cwd: "+dir+"\n    input_schema: { type: object }\n")
	if err != nil {
		t.Fatal(err)
	}
	if reason, _ := checkCapability(t.Context(), nil, c); reason != "" {
		t.Errorf("ready folder: %q", reason)
	}
	os.Remove(dir)
	if reason, _ := checkCapability(t.Context(), nil, c); reason != reasonFolderMissing {
		t.Errorf("missing folder: %q", reason)
	}
}

// A file that's there but couldn't be loaded, such as a broken one, is never
// replaced by a new tool.
func TestNewToolFileNeverReplacesAFile(t *testing.T) {
	path := writeWorker(t, "", map[string]string{"broken.yaml": "not: [valid"})
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewToolFile(m, "cli", "broken", "linux"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "tools", "broken.yaml")); string(raw) != "not: [valid" {
		t.Errorf("the file was replaced: %q", raw)
	}
}
