package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderCommand(t *testing.T) {
	input := map[string]any{"text": "hello world", "count": json.Number("3"), "on": true, "flag": "-rf", "nested": map[string]any{}, "tpl": "{{workdir}}"}
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantErr string
	}{
		{name: "whole value stays one argument", args: []string{"{{input.text}}"}, want: []string{"hello world"}},
		{name: "workdir", args: []string{"{{workdir}}/out"}, want: []string{"/work/out"}},
		{name: "number and boolean", args: []string{"{{input.count}}", "{{input.on}}"}, want: []string{"3", "true"}},
		{name: "spaces inside braces", args: []string{"{{ input.text }}"}, want: []string{"hello world"}},
		{name: "embedded after a prefix may contain a dash", args: []string{"--name={{input.flag}}"}, want: []string{"--name=-rf"}},
		{name: "values aren't expanded twice", args: []string{"x{{input.tpl}}"}, want: []string{"x{{workdir}}"}},
		{name: "leading dash value is refused", args: []string{"{{input.flag}}"}, wantErr: "must not start with"},
		{name: "literal option is fine", args: []string{"-o", "{{workdir}}/out"}, want: []string{"-o", "/work/out"}},
		{name: "missing field", args: []string{"{{input.absent}}"}, wantErr: "is missing"},
		{name: "input file path", args: []string{"-i", "{{files.source}}"}, want: []string{"-i", "/work/inputs/source.mp4"}},
		{name: "missing input file", args: []string{"{{files.other}}"}, wantErr: `input file "other" is missing`},
		{name: "object field", args: []string{"{{input.nested}}"}, wantErr: "must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderCommand(tt.args, templateVars{workdir: "/work", input: input, files: map[string]string{"source": "/work/inputs/source.mp4"}})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func manifestWith(gateway, capability string) string {
	return "gateway_url: " + gateway + `
state_dir: state
node: { name: mac, resources: { gpu: 1 } }
capabilities:
` + capability
}

const validCapability = `  - name: tts.say
    adapter: cli
    command: [/bin/echo, "{{input.text}}"]
    input_schema: { type: object }
    outputs: { audio: out.wav }
`

func TestLoadManifestRejectsMistakes(t *testing.T) {
	tests := []struct {
		name, manifest, wantErr string
	}{
		{"plain http to a remote host", manifestWith("http://example.com", validCapability), "must use https"},
		{"unknown scheme", manifestWith("ftp://example.com", validCapability), "must start with https"},
		{"unknown field", manifestWith("https://x", validCapability+"    shell: true\n"), "shell"},
		{"unsupported adapter", manifestWith("https://x", strings.Replace(validCapability, "adapter: cli", "adapter: ftp", 1)), "not supported"},
		{"placeholder as the program", manifestWith("https://x", strings.Replace(validCapability, "/bin/echo", "'{{input.text}}'", 1)), "cannot contain placeholders"},
		{"missing program", manifestWith("https://x", strings.Replace(validCapability, "/bin/echo", "/no/such/tool", 1)), "cannot find program"},
		{"unknown placeholder", manifestWith("https://x", strings.Replace(validCapability, "{{input.text}}", "{{secrets.key}}", 1)), "unknown placeholder"},
		{"output escaping the workdir", manifestWith("https://x", strings.Replace(validCapability, "out.wav", "../../etc/passwd", 1)), "inside the job's working directory"},
		{"absolute output", manifestWith("https://x", strings.Replace(validCapability, "out.wav", "/etc/passwd", 1)), "inside the job's working directory"},
		{"input file placeholder that isn't declared", manifestWith("https://x", strings.Replace(validCapability, "{{input.text}}", "{{files.source}}", 1)), "listed in input_files"},
		{"invalid input file name", manifestWith("https://x", validCapability+"    input_files: [\"../evil\"]\n"), "is invalid"},
		{"no input schema", manifestWith("https://x", strings.Replace(validCapability, "    input_schema: { type: object }\n", "", 1)), "input_schema is required"},
		{"external schema reference", manifestWith("https://x", strings.Replace(validCapability, "{ type: object }", `{ $ref: "file:///etc/passwd" }`, 1)), "input_schema"},
		{"more resources than the node has", manifestWith("https://x", validCapability+"    requires: { gpu: 2 }\n"), "node.resources provides 1"},
		{"bad capability name", manifestWith("https://x", strings.Replace(validCapability, "tts.say", "../evil", 1)), "name must be"},
		{"duplicate capability", manifestWith("https://x", validCapability+validCapability), "declared twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "worker.yaml")
			if err := os.WriteFile(path, []byte(tt.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadManifest(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadManifestAcceptsValidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, []byte(manifestWith("https://fleet.example.com/", validCapability)), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.GatewayURL != "https://fleet.example.com" {
		t.Errorf("trailing slash kept: %s", m.GatewayURL)
	}
	if m.Node.MaxJobs != 1 {
		t.Errorf("max_jobs default = %d", m.Node.MaxJobs)
	}
	if !filepath.IsAbs(m.StateDir) {
		t.Errorf("state_dir not resolved: %s", m.StateDir)
	}
}

func TestReadTokenRefusesSharedFile(t *testing.T) {
	t.Setenv("FLEET_NODE_TOKEN", "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{TokenFile: path}
	if _, err := m.ReadToken(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := m.ReadToken()
	if err != nil || token != "secret" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestSplitOutputLinesNeverStalls(t *testing.T) {
	long := strings.Repeat("x", maxLineBytes)
	advance, token, err := splitOutputLines([]byte(long), false)
	if err != nil || advance != len(long) || len(token) != len(long) {
		t.Errorf("over-long line: advance=%d token=%d err=%v", advance, len(token), err)
	}
	advance, token, _ = splitOutputLines([]byte("frame=1\rframe=2\r"), false)
	if advance != 8 || string(token) != "frame=1" {
		t.Errorf("carriage return: advance=%d token=%q", advance, token)
	}
}

func TestLoadEventsDropsTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), eventsFile)
	content := `{"attempt_id":"a","seq":1,"type":"started"}` + "\n" + `{"attempt_id":"a","seq":2,"ty`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	events, file, err := loadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if _, err := file.WriteString("next\n"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\"started\"}\nnext\n") {
		t.Errorf("torn line was not removed: %q", raw)
	}
}

const validService = `  - name: llm.generate
    adapter: http
    request:
      url: http://127.0.0.1:11434/api/generate
      json: { prompt: "{{input.prompt}}" }
    response: { result: json }
    input_schema: { type: object }
`

const validAsync = `  - name: image.generate
    adapter: http_async
    request:
      url: http://127.0.0.1:8188/prompt
      json: "{{input}}"
    async:
      job_id_field: id
      poll:
        url: http://127.0.0.1:8188/jobs/{{remote.id}}
        status_field: state
        succeeded: [done]
        failed: [error]
    input_schema: { type: object }
`

func TestLoadManifestRejectsServiceMistakes(t *testing.T) {
	sub := func(base, old, replacement string) string {
		if !strings.Contains(base, old) {
			t.Fatalf("test setup: %q isn't in the template", old)
		}
		return strings.Replace(base, old, replacement, 1)
	}
	const url = "http://127.0.0.1:11434/api/generate"
	tests := []struct {
		name, capability, wantErr string
	}{
		{"no request", sub(validService, "    request:\n      url: "+url+"\n      json: { prompt: \"{{input.prompt}}\" }\n", ""), "request is required"},
		{"no url", sub(validService, "url: "+url+"\n", "method: POST\n"), "url: is required"},
		{"another machine", sub(validService, url, "http://192.168.1.20:11434/x"), "isn't on this machine"},
		{"public host", sub(validService, url, "https://example.com/x"), "isn't on this machine"},
		{"other scheme", sub(validService, url, "file:///etc/passwd"), "must start with http"},
		{"placeholder in host", sub(validService, url, "http://{{input.host}}/x"), "scheme or host"},
		{"placeholder in port", sub(validService, url, "http://127.0.0.1:{{input.port}}/x"), "scheme or host"},
		{"credentials in url", sub(validService, url, "http://me:pw@127.0.0.1/x"), "username or password"},
		{"fragment", sub(validService, url, "http://127.0.0.1/x#y"), "fragment"},
		{"unknown method", sub(validService, "url: "+url, "method: TRACE\n      url: "+url), "not supported"},
		{"two bodies", sub(validService, "json: {", "body: raw\n      json: {"), "only one of"},
		{"secret in json placeholder", sub(validService, "{{input.prompt}}", "{{env.HOME}}"), "isn't allowed here"},
		{"workdir in url", sub(validService, url, "http://127.0.0.1/{{workdir}}"), "isn't allowed here"},
		{"whole input inside a string", sub(validService, "{{input.prompt}}", "x{{input}}"), "whole value"},
		{"placeholder in a json key", sub(validService, "prompt:", "\"{{input.k}}\":"), "can't contain a placeholder"},
		{"undeclared input file", sub(validService, "{{input.prompt}}", "{{files.source}}"), "isn't allowed here"},
		{"body_file that isn't a file", sub(validService, "json: { prompt: \"{{input.prompt}}\" }", "body_file: \"{{input.path}}\""), "body_file"},
		{"input in a header", sub(validService, "url: "+url, "url: "+url+"\n      headers: { X-A: \"{{input.prompt}}\" }"), "only {{env.NAME}}"},
		{"unset env in a header", sub(validService, "url: "+url, "url: "+url+"\n      headers: { X-A: \"{{env.FLEET_TEST_UNSET}}\" }"), "isn't set"},
		{"node token in a header", sub(validService, "url: "+url, "url: "+url+"\n      headers: { X-A: \"{{env.FLEET_NODE_TOKEN}}\" }"), "node token"},
		{"host header", sub(validService, "url: "+url, "url: "+url+"\n      headers: { Host: x }"), "can't be set"},
		{"bad header name", sub(validService, "url: "+url, "url: "+url+"\n      headers: { \"a b\": x }"), "valid header name"},
		{"command on http", validService + "    command: [/bin/echo]\n", "command doesn't apply"},
		{"progress on http", validService + "    progress: { regex: x }\n", "progress doesn't apply"},
		{"async on http", validService + "    async: { job_id_field: id }\n", "only to the http_async"},
		{"result and file", sub(validService, "{ result: json }", "{ result: json, file: out.txt }"), "not both"},
		{"saved file not delivered", sub(validService, "{ result: json }", "{ file: out.txt }"), "must also be listed in outputs"},
		{"unknown result format", sub(validService, "result: json", "result: xml"), "must be json or text"},
		{"field without json", sub(validService, "{ result: json }", "{ result: text, result_field: a }"), "needs result: json"},
		{"bad timeout", validService + "    timeout: soon\n", "not a positive duration"},
		{"async without async block", sub(validAsync, "    async:\n      job_id_field: id\n      poll:\n        url: http://127.0.0.1:8188/jobs/{{remote.id}}\n        status_field: state\n        succeeded: [done]\n        failed: [error]\n", ""), "async is required"},
		{"no job id field", sub(validAsync, "job_id_field: id", "job_id_field: \"\""), "job_id_field"},
		{"input in a poll url", sub(validAsync, "{{remote.id}}", "{{input.id}}"), "isn't allowed here"},
		{"remote id in the first request", sub(validAsync, "http://127.0.0.1:8188/prompt", "http://127.0.0.1:8188/{{remote.id}}"), "isn't allowed here"},
		{"poll to another machine", sub(validAsync, "http://127.0.0.1:8188/jobs", "http://10.0.0.5/jobs"), "isn't on this machine"},
		{"no status field", sub(validAsync, "status_field: state", "status_field: \"\""), "status_field"},
		{"no success status", sub(validAsync, "succeeded: [done]", "succeeded: []"), "at least one status"},
		{"status listed twice", sub(validAsync, "failed: [error]", "failed: [done]"), "both succeeded and failed"},
		{"bad poll interval", sub(validAsync, "status_field: state", "every: 0s\n        status_field: state"), "async.poll.every"},
		{"cli field on a command", validCapability + "    request: { url: \"http://127.0.0.1/x\" }\n", "only to the http"},
		{"allow_remote on a command", validCapability + "    allow_remote: true\n", "allow_remote applies"},
		{"check with command and url", validService + "    check: { command: [/bin/true], url: \"http://127.0.0.1/h\" }\n", "command or url, not both"},
		{"check with a missing program", validService + "    check: { command: [/no/such/tool] }\n", "cannot find program"},
		{"check with a placeholder in its command", validService + "    check: { command: [/bin/echo, \"{{input.x}}\"] }\n", "can't use placeholders"},
		{"check with a placeholder in its url", validService + "    check: { url: \"http://127.0.0.1/{{input.x}}\" }\n", "isn't allowed here"},
		{"check on another machine", validService + "    check: { url: \"http://10.0.0.5/h\" }\n", "isn't on this machine"},
		{"check with a body", validService + "    check: { url: \"http://127.0.0.1/h\", body: x }\n", "can't send a body"},
		{"check with headers but no url", validService + "    check: { headers: { X-A: b } }\n", "need check.url"},
		{"bad check interval", validService + "    check: { every: soon }\n", "check.every"},
		{"bad check timeout", validService + "    check: { timeout: 0s }\n", "check.timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "worker.yaml")
			if err := os.WriteFile(path, []byte(manifestWith("https://x", tt.capability)), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadManifest(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadManifestAcceptsServices(t *testing.T) {
	t.Setenv("FLEET_TEST_SECRET", "s3cret")
	remote := strings.Replace(validService, "http://127.0.0.1:11434/api/generate", "https://gpu.lan/api", 1) + "    allow_remote: true\n"
	withHeader := strings.Replace(validService, "      json:", "      headers: { Authorization: \"Bearer {{env.FLEET_TEST_SECRET}}\" }\n      json:", 1)
	for name, capability := range map[string]string{"local": validService, "async": validAsync, "remote when allowed": remote} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "worker.yaml")
			if err := os.WriteFile(path, []byte(manifestWith("https://x", capability)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(path); err != nil {
				t.Fatal(err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, []byte(manifestWith("https://x", withHeader)), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Capabilities[0].Request.Headers["Authorization"]; got != "Bearer s3cret" {
		t.Errorf("header = %q", got)
	}
	if got := m.Capabilities[0].Request.Method; got != "POST" {
		t.Errorf("a request with a body should default to POST, got %q", got)
	}
}

func TestRenderURLKeepsValuesInTheirPlace(t *testing.T) {
	const tpl = "http://127.0.0.1/models/{{input.m}}/run?tag={{input.q}}"
	tests := []struct {
		name, model, query, want, wantErr string
	}{
		{name: "plain", model: "llama3", query: "a", want: "http://127.0.0.1/models/llama3/run?tag=a"},
		{name: "query stays one value", model: "m", query: "a&admin=1#x", want: "http://127.0.0.1/models/m/run?tag=a%26admin%3D1%23x"},
		{name: "path stays escaped", model: "a b?c#d", query: "q", want: "http://127.0.0.1/models/a%20b%3Fc%23d/run?tag=q"},
		{name: "value that looks like a placeholder", model: "{{workdir}}", query: "q", want: "http://127.0.0.1/models/%7B%7Bworkdir%7D%7D/run?tag=q"},
		{name: "slash", model: "../admin", query: "q", wantErr: "URL path"},
		{name: "backslash", model: `..\admin`, query: "q", wantErr: "URL path"},
		{name: "dot dot", model: "..", query: "q", wantErr: "URL path"},
		{name: "dot", model: ".", query: "q", wantErr: "URL path"},
		{name: "empty", model: "", query: "q", wantErr: "URL path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderURL(tpl, templateVars{input: map[string]any{"m": tt.model, "q": tt.query}})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got %q, err = %v", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, err = %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestRenderJSONKeepsTypesAndStructure(t *testing.T) {
	input := map[string]any{
		"n": json.Number("0.70"), "s": `he said "hi"}, "admin": true, {"`, "o": map[string]any{"a": json.Number("1")},
	}
	template := map[string]any{
		"number": "{{input.n}}", "text": "{{input.s}}", "object": "{{input.o}}", "embedded": "n={{input.n}}",
		"optional": "{{input.absent}}", "literal": 5, "list": []any{"{{input.n}}", "x"},
		"nested": map[string]any{"optional": "{{input.absent}}", "whole": "{{input}}"},
	}
	rendered, omit, err := renderJSON(template, templateVars{input: input})
	if err != nil || omit {
		t.Fatalf("omit=%v err=%v", omit, err)
	}
	encoded, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["number"] != json.Number("0.70") {
		t.Errorf("number = %#v", got["number"])
	}
	if got["text"] != input["s"] {
		t.Errorf("a string with JSON syntax changed: %#v", got["text"])
	}
	if _, injected := got["admin"]; injected {
		t.Error("input added a key to the document")
	}
	if got["embedded"] != "n=0.70" {
		t.Errorf("embedded = %#v", got["embedded"])
	}
	if _, present := got["optional"]; present {
		t.Error("a missing optional field wasn't left out")
	}
	nested := got["nested"].(map[string]any)
	if _, present := nested["optional"]; present {
		t.Error("a nested missing optional field wasn't left out")
	}
	if nested["whole"].(map[string]any)["s"] != input["s"] {
		t.Errorf("whole input = %#v", nested["whole"])
	}
	if _, _, err := renderJSON([]any{"{{input.absent}}"}, templateVars{input: input}); err == nil {
		t.Error("a missing field in a list should be an error, because dropping it would shift the list")
	}
	if _, _, err := renderJSON("x{{input.absent}}", templateVars{input: input}); err == nil {
		t.Error("a missing field inside a longer string should be an error")
	}
}

func TestFieldAt(t *testing.T) {
	doc, err := decodeJSON([]byte(`{"data":[{"id":7,"ok":true}],"s":"x","nothing":null}`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"data.0.id": "7", "data.0.ok": "true", "s": "x"} {
		if got, ok := fieldText(doc, path); !ok || got != want {
			t.Errorf("%s = %q, %v", path, got, ok)
		}
	}
	for _, path := range []string{"missing", "data.1.id", "data.x", "s.y", "data", "nothing", "data.-1"} {
		if got, ok := fieldText(doc, path); ok {
			t.Errorf("%s = %q, want nothing", path, got)
		}
	}
	if _, err := decodeJSON([]byte(`{} {}`)); err == nil {
		t.Error("extra data after the value should be refused")
	}
}

func TestStatusFailureCode(t *testing.T) {
	for status, want := range map[int]string{
		400: "SERVICE_ERROR", 404: "SERVICE_ERROR", 500: "SERVICE_ERROR",
		408: "SERVICE_UNAVAILABLE", 429: "SERVICE_UNAVAILABLE", 502: "SERVICE_UNAVAILABLE", 503: "SERVICE_UNAVAILABLE", 504: "SERVICE_UNAVAILABLE",
	} {
		if got := statusFailureCode(status); got != want {
			t.Errorf("%d = %s, want %s", status, got, want)
		}
	}
}

func TestCheckExecutableRefusesBatchFilesOnWindows(t *testing.T) {
	for _, path := range []string{`C:\tools\run.bat`, `C:\tools\RUN.CMD`} {
		if err := checkExecutable(path, "windows"); err == nil {
			t.Errorf("%s was accepted", path)
		}
	}
	if err := checkExecutable(`C:\tools\ffmpeg.exe`, "windows"); err != nil {
		t.Error(err)
	}
	if err := checkExecutable("/usr/bin/run.cmd", "darwin"); err != nil {
		t.Error(err)
	}
}

func TestBuildChildEnv(t *testing.T) {
	source := map[string]string{"PATH": "p", "HOME": "h", "SystemRoot": `C:\Windows`, "FLEET_NODE_TOKEN": "secret", "OTHER": "x"}
	lookup := func(key string) (string, bool) { v, ok := source[key]; return v, ok }
	has := func(env []string, want string) bool {
		for _, entry := range env {
			if entry == want {
				return true
			}
		}
		return false
	}
	unix := buildChildEnv("darwin", lookup, map[string]string{"TOOL": "1"}, "/work")
	for _, want := range []string{"PATH=p", "HOME=h", "TMPDIR=/work", "TOOL=1"} {
		if !has(unix, want) {
			t.Errorf("unix env lacks %s: %v", want, unix)
		}
	}
	windows := buildChildEnv("windows", lookup, nil, `C:\work`)
	for _, want := range []string{"PATH=p", `SystemRoot=C:\Windows`, `TEMP=C:\work`, `TMP=C:\work`} {
		if !has(windows, want) {
			t.Errorf("windows env lacks %s: %v", want, windows)
		}
	}
	for _, env := range [][]string{unix, windows} {
		for _, entry := range env {
			if strings.HasPrefix(entry, "FLEET_NODE_TOKEN") || strings.HasPrefix(entry, "OTHER") {
				t.Errorf("leaked %s", entry)
			}
		}
	}
}

func TestDialAddress(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1:11434/api/{{input.m}}?x={{input.y}}": "127.0.0.1:11434",
		"http://localhost/x":  "localhost:80",
		"https://gpu.lan/x":   "gpu.lan:443",
		"http://[::1]:8000/x": "[::1]:8000",
	} {
		got, err := dialAddress(raw)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v, want %q", raw, got, err, want)
		}
	}
}

func TestProgramRunnable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool")
	if err := programRunnable(path, "linux"); err == nil {
		t.Error("a missing file counted as runnable")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := programRunnable(path, "linux"); err == nil {
		t.Error("a file with no execute bit counted as runnable")
	}
	if err := programRunnable(path, "windows"); err != nil {
		t.Errorf("Windows has no execute bit to require: %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := programRunnable(path, "linux"); err != nil {
		t.Error(err)
	}
	if err := programRunnable(dir, "linux"); err == nil {
		t.Error("a directory counted as runnable")
	}
}
