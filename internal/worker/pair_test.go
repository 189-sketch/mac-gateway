package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeGatewayURL(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.10":                  "https://203.0.113.10",
		" fleet.example.com ":           "https://fleet.example.com",
		"https://fleet.example.com/":    "https://fleet.example.com",
		"https://fleet.example.com/mcp": "https://fleet.example.com",
		"fleet.example.com:8443":        "https://fleet.example.com:8443",
		"http://127.0.0.1:8080":         "http://127.0.0.1:8080",
	} {
		got, err := NormalizeGatewayURL(in, false)
		if err != nil || got != want {
			t.Errorf("NormalizeGatewayURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "http://fleet.example.com", "ftp://fleet.example.com", "https://user:pw@fleet.example.com", "https://"} {
		if got, err := NormalizeGatewayURL(bad, false); err == nil {
			t.Errorf("NormalizeGatewayURL(%q) = %q, want an error", bad, got)
		}
	}
	if _, err := NormalizeGatewayURL("http://fleet.example.com", true); err != nil {
		t.Errorf("plain http with allow_insecure_http: %v", err)
	}
}

const installedManifest = `# This file alone decides what this machine will run.
#
# Pairing this machine fills in gateway_url and node.name.
gateway_url: ""   # set by pairing
token_file: token
state_dir: state

node:
  name: my-host   # what the gateway calls it
  max_jobs: 2
  labels:
    os: linux

capabilities:
  - name: system.hostname
    adapter: cli
    command: [/bin/uname, -n]
    input_schema:
      type: object
`

func TestSavePairingChangesOnlyItsLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(path, []byte(installedManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SavePairing(path, "https://203.0.113.10", "mac-mini-2", "fn_secret"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		`gateway_url: ""   # set by pairing`, `gateway_url: https://203.0.113.10 # set by pairing`,
		`name: my-host   # what the gateway calls it`, `name: mac-mini-2 # what the gateway calls it`,
	).Replace(installedManifest)
	if string(got) != want {
		t.Errorf("worker.yaml is now:\n%s\nwant:\n%s", got, want)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.GatewayURL != "https://203.0.113.10" || m.Node.Name != "mac-mini-2" {
		t.Errorf("loaded gateway %q, name %q", m.GatewayURL, m.Node.Name)
	}
	token, err := m.ReadToken()
	if err != nil || token != "fn_secret" {
		t.Errorf("token = %q, %v", token, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "token")); info.Mode().Perm() != 0o600 {
		t.Errorf("the token file's mode is %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("worker.yaml's mode changed to %v", info.Mode().Perm())
	}

	// Pairing again, with another gateway, replaces what the first one wrote.
	if err := SavePairing(path, "https://fleet.example.com", "mac", "fn_other"); err != nil {
		t.Fatal(err)
	}
	m, err = LoadManifest(path)
	if err != nil || m.GatewayURL != "https://fleet.example.com" || m.Node.Name != "mac" {
		t.Fatalf("after pairing again: %+v %v", m, err)
	}
	if token, _ := m.ReadToken(); token != "fn_other" {
		t.Errorf("token = %q", token)
	}
}

func TestSavePairingAddsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	// No gateway_url, no token_file, and a token kept somewhere of its own.
	body := "# mine\nstate_dir: state\nnode:\n    max_jobs: 3\n    name: old\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SavePairing(path, "https://fleet.example.com", "new", "fn_secret"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	for _, line := range []string{"# mine", "gateway_url: https://fleet.example.com", "token_file: token", "    name: new", "    max_jobs: 3"} {
		if !strings.Contains(string(got), line+"\n") {
			t.Errorf("worker.yaml lacks %q:\n%s", line, got)
		}
	}
	if m, err := LoadManifest(path); err != nil || m.GatewayURL != "https://fleet.example.com" || m.Node.Name != "new" {
		t.Fatalf("loaded %+v, %v", m, err)
	}

	custom := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(custom, []byte("gateway_url: https://old.example.com\ntoken_file: secrets/node\nstate_dir: state\nnode:\n  name: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SavePairing(custom, "https://fleet.example.com", "new", "fn_secret"); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "secrets", "node")); err != nil || string(raw) != "fn_secret\n" {
		t.Errorf("the token didn't go where token_file says: %q %v", raw, err)
	}
}

func TestSavePairingLeavesAFileItCantChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	body := "gateway_url: https://old.example.com\ntoken_file: token\nstate_dir: state\nnode: { name: old }\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SavePairing(path, "https://fleet.example.com", "new", "fn_secret")
	if err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != body {
		t.Errorf("worker.yaml changed:\n%s", got)
	}
}

// A machine with no gateway and no token is a valid one that isn't paired yet.
func TestAnUnpairedManifestLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(path, []byte(installedManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil || m.GatewayURL != "" {
		t.Fatalf("gateway %q, %v", m.GatewayURL, err)
	}
	if _, err := m.ReadToken(); err == nil || !strings.Contains(err.Error(), ErrNoToken.Error()) {
		t.Fatalf("ReadToken with no token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadToken(); err == nil || !strings.Contains(err.Error(), ErrNoToken.Error()) {
		t.Fatalf("ReadToken with an empty token file: %v", err)
	}
}
