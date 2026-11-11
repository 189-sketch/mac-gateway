package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// uiClient talks to a worker's configuration page as the page does.
type uiClient struct {
	t     *testing.T
	base  string
	token string
}

func (c *uiClient) call(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const pageTool = `name: page.echo
description: Made on the configuration page.
adapter: cli
command: [/bin/sh, -c, 'printf "page %s" "$1"', sh, "{{input.text}}"]
input_schema: { type: object, required: [text], properties: { text: { type: string } } }
result: { from: stdout }
`

func TestConfigurationPageChangesWhatTheGatewayCanRun(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	// The harness turns the page off, so workers can run side by side.
	w.stop()
	config, err := os.ReadFile(w.config)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.Replace(config, []byte("ui_listen: off"), []byte("ui_listen: 127.0.0.1:0"), 1)
	if err := os.WriteFile(w.config, config, 0o600); err != nil {
		t.Fatal(err)
	}
	w.start()
	eventually(t, 10*time.Second, "the page's sign-in file", func() bool {
		_, err := os.Stat(filepath.Join(w.stateDir, "ui.json"))
		return err == nil
	})

	// `fleet-worker ui` gives an address that signs in once.
	out, err := exec.Command(workerBin, "ui", "--config", w.config, "--no-open").Output()
	if err != nil {
		t.Fatalf("fleet-worker ui: %v", err)
	}
	link, err := url.Parse(strings.TrimSpace(string(out)))
	if err != nil || !strings.HasPrefix(link.Fragment, "code=") || link.Hostname() != "127.0.0.1" {
		t.Fatalf("link = %q", out)
	}
	base := link.Scheme + "://" + link.Host + "/"
	signIn := func() (int, string) {
		body, _ := json.Marshal(map[string]string{"code": strings.TrimPrefix(link.Fragment, "code=")})
		resp, err := http.Post(base+"api/sign-in", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got struct{ Token string }
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return resp.StatusCode, got.Token
	}
	status, token := signIn()
	if status != http.StatusOK || token == "" {
		t.Fatalf("sign in: %d", status)
	}
	if again, _ := signIn(); again != http.StatusUnauthorized {
		t.Fatalf("the link worked twice: %d", again)
	}
	page := &uiClient{t: t, base: base, token: token}
	eventually(t, 10*time.Second, "the page to show the gateway connection", func() bool {
		_, st := page.call("GET", "api/status", nil)
		return st["connected"] == true
	})

	// A tool saved on the page can be tried there, then run by the gateway.
	if code, body := page.call("PUT", "api/files/page.yaml", map[string]any{"yaml": pageTool, "version": ""}); code != http.StatusOK {
		t.Fatalf("save: %d %v", code, body)
	}
	_, started := page.call("POST", "api/try", map[string]any{"yaml": pageTool, "file": "page.yaml", "input": map[string]string{"text": "tried"}})
	var tried map[string]any
	eventually(t, 10*time.Second, "the try to finish", func() bool {
		_, tried = page.call("GET", "api/try/"+started["id"].(string), nil)
		return tried["state"] != "running"
	})
	if tried["state"] != "succeeded" || tried["result"] != "page tried" {
		t.Fatalf("try = %v", tried)
	}
	eventually(t, 15*time.Second, "the saved tool to be offered", func() bool { return h.offered("page.echo") })
	if j := h.runAndWait("page.echo", map[string]any{"text": "from an agent"}); string(j.Result) != `"page from an agent"` {
		t.Fatalf("job: %s %+v", j.Result, j.Error)
	}

	// Turned off on the page, the gateway stops offering it.
	if code, body := page.call("POST", "api/files/page.yaml/enabled", map[string]bool{"enabled": false}); code != http.StatusOK {
		t.Fatalf("turn off: %d %v", code, body)
	}
	eventually(t, 15*time.Second, "the tool to be withdrawn", func() bool { return !h.offered("page.echo") })

	// When the worker stops, so does the page, and its sign-in file goes.
	w.stop()
	if _, err := os.Stat(filepath.Join(w.stateDir, "ui.json")); !os.IsNotExist(err) {
		t.Fatalf("the sign-in file is still there: %v", err)
	}
	if out, err := exec.Command(workerBin, "ui", "--config", w.config, "--no-open").CombinedOutput(); err == nil || !strings.Contains(string(out), "isn't running") {
		t.Fatalf("ui with no worker: %v %s", err, out)
	}
}
