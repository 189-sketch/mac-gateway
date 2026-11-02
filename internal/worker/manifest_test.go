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
		{"unsupported adapter", manifestWith("https://x", strings.Replace(validCapability, "adapter: cli", "adapter: http", 1)), "not supported"},
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
