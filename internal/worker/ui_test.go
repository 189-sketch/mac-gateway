package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const uiHost = "127.0.0.1:7878"

type testUI struct {
	t       *testing.T
	s       *uiServer
	w       *Worker
	h       http.Handler
	dir     string
	toolDir string
}

// newTestUI serves the page for a worker with one tool in worker.yaml and these
// tool files.
func newTestUI(t *testing.T, files map[string]string) *testUI {
	t.Helper()
	path := writeWorker(t, "tools:\n"+echoTool("builtin.echo"), files)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(m, "node-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	logs := NewLogRing(10)
	s, err := newUIServer(w, UIOptions{ConfigPath: path, Logs: logs, Version: "test"}, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7878})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.tries.stopAll)
	return &testUI{t: t, s: s, w: w, h: s.handler(), dir: filepath.Dir(path), toolDir: filepath.Join(filepath.Dir(path), "tools")}
}

func echoTool(name string) string {
	return "  - name: " + name + "\n    adapter: cli\n    command: [" + echoPath() + ", hello]\n    input_schema: { type: object }\n"
}

func echoPath() string {
	if path, err := lookPathForTest("echo"); err == nil {
		return path
	}
	return "/bin/echo"
}

func lookPathForTest(name string) (string, error) {
	for _, dir := range []string{"/bin", "/usr/bin"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return filepath.Join(dir, name), nil
		}
	}
	return "", os.ErrNotExist
}

// do sends a request as the page would, signed in.
func (u *testUI) do(method, path string, body any) *httptest.ResponseRecorder {
	u.t.Helper()
	return u.raw(method, path, body, map[string]string{"Authorization": "Bearer " + u.s.token})
}

func (u *testUI) raw(method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	u.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			u.t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req := httptest.NewRequest(method, "http://"+uiHost+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	u.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
}

func TestUIRefusesRequestsItShouldnt(t *testing.T) {
	u := newTestUI(t, nil)
	signedIn := map[string]string{"Authorization": "Bearer " + u.s.token}

	expectStatus(t, u.raw("GET", "/api/status", nil, nil), http.StatusUnauthorized)
	expectStatus(t, u.raw("GET", "/api/status", nil, map[string]string{"Authorization": "Bearer wrong"}), http.StatusUnauthorized)
	expectStatus(t, u.raw("GET", "/api/status", nil, map[string]string{"Authorization": u.s.token}), http.StatusUnauthorized)
	expectStatus(t, u.raw("GET", "/api/status", nil, signedIn), http.StatusOK)

	// A page from elsewhere can make its own name point at this machine.
	for _, host := range []string{"evil.example:7878", "127.0.0.1:7879", "127.0.0.1", "0.0.0.0:7878"} {
		req := httptest.NewRequest("GET", "http://"+uiHost+"/api/status", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+u.s.token)
		rec := httptest.NewRecorder()
		u.h.ServeHTTP(rec, req)
		expectStatus(t, rec, http.StatusMisdirectedRequest)
	}
	for _, host := range []string{"localhost:7878", "[::1]:7878"} {
		req := httptest.NewRequest("GET", "http://"+uiHost+"/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		u.h.ServeHTTP(rec, req)
		expectStatus(t, rec, http.StatusOK)
	}

	withOrigin := map[string]string{"Authorization": "Bearer " + u.s.token, "Origin": "http://evil.example"}
	expectStatus(t, u.raw("POST", "/api/files", map[string]string{"template": "cli", "name": "x"}, withOrigin), http.StatusForbidden)
	withOrigin["Origin"] = "http://" + uiHost
	expectStatus(t, u.raw("GET", "/api/status", nil, withOrigin), http.StatusOK)

	// A form from another page can only send a few content types, none of them JSON.
	req := httptest.NewRequest("POST", "http://"+uiHost+"/api/files", strings.NewReader(`{"template":"cli","name":"x"}`))
	req.Header.Set("Authorization", "Bearer "+u.s.token)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	u.h.ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusUnsupportedMediaType)

	// The page itself needs no secret, and says what it may load.
	page := u.raw("GET", "/", nil, nil)
	expectStatus(t, page, http.StatusOK)
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
	if !strings.Contains(page.Body.String(), "app.js") {
		t.Fatal("the page wasn't served")
	}
}

func TestUISignInCodeWorksOnceAndBriefly(t *testing.T) {
	u := newTestUI(t, nil)
	expectStatus(t, u.raw("POST", "/api/sign-in-code", map[string]string{}, nil), http.StatusUnauthorized)

	rec := u.do("POST", "/api/sign-in-code", map[string]string{})
	expectStatus(t, rec, http.StatusOK)
	code := decode[map[string]string](t, rec)["code"]

	signIn := func(code string) *httptest.ResponseRecorder {
		return u.raw("POST", "/api/sign-in", map[string]string{"code": code}, nil)
	}
	expectStatus(t, signIn("not-a-code"), http.StatusUnauthorized)
	first := signIn(code)
	expectStatus(t, first, http.StatusOK)
	if decode[map[string]string](t, first)["token"] != u.s.token {
		t.Fatal("signing in didn't give the secret")
	}
	expectStatus(t, signIn(code), http.StatusUnauthorized)

	rec = u.do("POST", "/api/sign-in-code", map[string]string{})
	late := decode[map[string]string](t, rec)["code"]
	u.s.mu.Lock()
	u.s.codes[late] = time.Now().Add(-time.Second)
	u.s.mu.Unlock()
	expectStatus(t, signIn(late), http.StatusUnauthorized)

	for range maxSignInCodes {
		u.do("POST", "/api/sign-in-code", map[string]string{})
	}
	expectStatus(t, u.do("POST", "/api/sign-in-code", map[string]string{}), http.StatusTooManyRequests)
}

const disabledTool = `name: later
enabled: false # Not ready yet.
adapter: cli
command: [TODO/path]
input_schema: { type: object }
`

func TestUIStatusShowsEveryTool(t *testing.T) {
	u := newTestUI(t, map[string]string{
		"on.yaml":     "name: on.tool\ndescription: Says hello.\nadapter: cli\ncommand: [" + echoPath() + "]\ninput_schema: { type: object }\n",
		"later.yaml":  disabledTool,
		"broken.yaml": "name: broken\nadapter: cli\ncommand: [/no/such/program]\ninput_schema: { type: object }\n",
	})
	u.w.mu.Lock()
	u.w.notReady["on.tool"] = "check_failed"
	u.w.notReadyDetail["on.tool"] = "exit status 3"
	u.w.mu.Unlock()

	st := decode[uiStatus](t, u.do("GET", "/api/status", nil))
	if st.Node != "mac" || st.Connected || st.Version != "test" {
		t.Fatalf("status = %+v", st)
	}
	got := map[string]uiTool{}
	for _, tool := range st.Tools {
		got[tool.Name] = tool
	}
	want := map[string]uiTool{
		"builtin.echo": {Name: "builtin.echo", Adapter: "cli", State: "ready"},
		"on.tool":      {Name: "on.tool", Description: "Says hello.", Adapter: "cli", State: "not_ready", Reason: "check_failed", Detail: "exit status 3", File: "on.yaml"},
		"later":        {Name: "later", Adapter: "cli", State: "disabled", File: "later.yaml"},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %+v, want %+v", name, got[name], w)
		}
	}
	if b := got["broken"]; b.State != "invalid" || b.File != "broken.yaml" || !strings.Contains(b.Error, "/no/such/program") {
		t.Errorf("broken = %+v", b)
	}

	u.w.connectedAt.Store(time.Now().UnixMilli())
	if st := decode[uiStatus](t, u.do("GET", "/api/status", nil)); !st.Connected || st.ConnectedSince == "" {
		t.Fatalf("connection not shown: %+v", st)
	}
}

func TestUISavesOnlyWhatWouldLoad(t *testing.T) {
	u := newTestUI(t, map[string]string{"later.yaml": disabledTool})
	file := decode[uiFile](t, u.do("GET", "/api/files/later.yaml", nil))
	if file.YAML != disabledTool || file.Tool["name"] != "later" || file.Problem == "" {
		t.Fatalf("read = %+v", file)
	}

	save := func(yaml, version string) *httptest.ResponseRecorder {
		return u.do("PUT", "/api/files/later.yaml", map[string]any{"yaml": yaml, "version": version})
	}
	read := func() string {
		data, err := os.ReadFile(filepath.Join(u.toolDir, "later.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	// Turned on, it must work.
	enabled := strings.Replace(disabledTool, "enabled: false", "enabled: true", 1)
	expectStatus(t, save(enabled, file.Version), http.StatusUnprocessableEntity)
	// Off, it can be saved unfinished.
	unfinished := disabledTool + "description: Still working on it.\n"
	rec := save(unfinished, file.Version)
	expectStatus(t, rec, http.StatusOK)
	saved := decode[uiFile](t, rec)
	if read() != unfinished || saved.Problem == "" {
		t.Fatalf("saved %q, problem %q", read(), saved.Problem)
	}
	// Something else changed it since the page read it.
	expectStatus(t, save(disabledTool, file.Version), http.StatusConflict)
	if read() != unfinished {
		t.Fatal("a stale save replaced the file")
	}
	// Two tools can't share a name.
	clash := strings.Replace(unfinished, "name: later", "name: builtin.echo", 1)
	expectStatus(t, save(clash, saved.Version), http.StatusUnprocessableEntity)
	// Unknown fields are refused, as the worker would.
	expectStatus(t, save(unfinished+"colour: blue\n", saved.Version), http.StatusUnprocessableEntity)

	// A working tool, saved from the form.
	tool := map[string]any{
		"name": "later", "enabled": true, "adapter": "cli",
		"command":      []any{echoPath(), "{{input.text}}"},
		"input_schema": map[string]any{"type": "object", "required": []any{"text"}, "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		"result":       map[string]any{"from": "stdout"},
		"outputs":      map[string]any{"report": map[string]any{"path": "report", "archive": "zip"}, "log": "run.log"},
	}
	rec = u.do("PUT", "/api/files/later.yaml", map[string]any{"tool": tool, "version": saved.Version})
	expectStatus(t, rec, http.StatusOK)
	body := read()
	for _, want := range []string{"name: later\n", "enabled: true\n", "command: [" + echoPath() + ", '{{input.text}}']\n",
		"input_schema:\n  type: object\n  required: [text]\n  properties:\n    text: {type: string}\n",
		"result: {from: stdout}\n", "  log: run.log\n", "  report: {path: report, archive: zip}\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("form save is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "optional") || strings.Contains(body, "format") {
		t.Errorf("form save wrote empty settings:\n%s", body)
	}
	if _, err := LoadManifest(filepath.Join(u.dir, "worker.yaml")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(u.toolDir, "later.yaml"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(u.toolDir); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
}

func TestUIFileNamesStayInTheToolsFolder(t *testing.T) {
	u := newTestUI(t, map[string]string{"later.yaml": disabledTool})
	for _, path := range []string{"/api/files/..%2Fworker.yaml", "/api/files/worker.txt", "/api/files/.hidden.yaml", "/api/files/a%5Cb.yaml"} {
		if rec := u.do("GET", path, nil); rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	// A link can't lead out of the folder either.
	if err := os.Symlink(filepath.Join(u.dir, "worker.yaml"), filepath.Join(u.toolDir, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	rec := u.do("GET", "/api/files/link.yaml", nil)
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "gateway_url") {
		t.Fatalf("read through a link: %d %s", rec.Code, rec.Body.String())
	}
	before, _ := os.ReadFile(filepath.Join(u.dir, "worker.yaml"))
	u.do("PUT", "/api/files/link.yaml", map[string]any{"yaml": disabledTool, "version": fileVersion(before)})
	after, _ := os.ReadFile(filepath.Join(u.dir, "worker.yaml"))
	if string(before) != string(after) {
		t.Fatal("a save through a link changed worker.yaml")
	}
}

func TestUINewTurnOnAndDelete(t *testing.T) {
	u := newTestUI(t, nil)
	rec := u.do("POST", "/api/files", map[string]string{"template": "cli", "name": "my.tool"})
	expectStatus(t, rec, http.StatusOK)
	created := decode[uiFile](t, rec)
	if created.File != "my.tool.yaml" || !strings.Contains(created.YAML, "enabled: false") {
		t.Fatalf("created = %+v", created)
	}
	expectStatus(t, u.do("POST", "/api/files", map[string]string{"template": "cli", "name": "my.tool"}), http.StatusUnprocessableEntity)
	expectStatus(t, u.do("POST", "/api/files", map[string]string{"template": "cli", "name": "builtin.echo"}), http.StatusUnprocessableEntity)
	expectStatus(t, u.do("POST", "/api/files", map[string]string{"template": "nope", "name": "other"}), http.StatusUnprocessableEntity)

	// Its TODOs aren't filled in, so it can't be turned on.
	expectStatus(t, u.do("POST", "/api/files/my.tool.yaml/enabled", map[string]bool{"enabled": true}), http.StatusUnprocessableEntity)
	data, _ := os.ReadFile(filepath.Join(u.toolDir, "my.tool.yaml"))
	if !strings.Contains(string(data), "enabled: false") {
		t.Fatal("a tool that doesn't work was turned on")
	}

	expectStatus(t, u.do("DELETE", "/api/files/my.tool.yaml", nil), http.StatusOK)
	if _, err := os.Stat(filepath.Join(u.toolDir, "my.tool.yaml")); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
	expectStatus(t, u.do("DELETE", "/api/files/my.tool.yaml", nil), http.StatusNotFound)
}

func TestTurningAToolOnKeepsTheRestOfTheFile(t *testing.T) {
	working := "# My tool.\nname: ok # Its name.\nenabled: false   # Off for now.\nadapter: cli\ncommand: [" + echoPath() + "]\ninput_schema: { type: object }\n"
	u := newTestUI(t, map[string]string{"ok.yaml": working})
	expectStatus(t, u.do("POST", "/api/files/ok.yaml/enabled", map[string]bool{"enabled": true}), http.StatusOK)
	data, _ := os.ReadFile(filepath.Join(u.toolDir, "ok.yaml"))
	if want := strings.Replace(working, "enabled: false", "enabled: true", 1); string(data) != want {
		t.Fatalf("got\n%s\nwant\n%s", data, want)
	}
	expectStatus(t, u.do("POST", "/api/files/ok.yaml/enabled", map[string]bool{"enabled": false}), http.StatusOK)
	data, _ = os.ReadFile(filepath.Join(u.toolDir, "ok.yaml"))
	if string(data) != working {
		t.Fatalf("got\n%s", data)
	}
}

func TestSetEnabledText(t *testing.T) {
	cases := []struct {
		name, in, want string
		enabled        bool
	}{
		{"adds the line", "# Note.\nname: a\nadapter: cli\n", "# Note.\nenabled: false\nname: a\nadapter: cli\n", false},
		{"changes it", "name: a\nenabled: true # Keep this.\n", "name: a\nenabled: false # Keep this.\n", false},
		{"after a document marker", "---\nname: a\n", "---\nenabled: true\nname: a\n", true},
	}
	for _, c := range cases {
		got, err := setEnabledText([]byte(c.in), c.enabled)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	for _, in := range []string{"{name: a, enabled: true}\n", "- a\n", "", "name: a\nenabled: [x]\n"} {
		if got, err := setEnabledText([]byte(in), false); err == nil {
			t.Errorf("%q: changed to %q instead of refusing", in, got)
		}
	}
}

func TestUIConvertAndCheck(t *testing.T) {
	u := newTestUI(t, nil)
	yaml := "name: x\nadapter: cli\ncommand: [" + echoPath() + ", '{{input.a}}']\ninput_schema:\n  type: object\n  properties: { a: { type: string } }\n"
	tool := decode[map[string]map[string]any](t, u.do("POST", "/api/convert", map[string]string{"yaml": yaml}))["tool"]
	if tool["name"] != "x" {
		t.Fatalf("tool = %v", tool)
	}
	back := decode[map[string]string](t, u.do("POST", "/api/convert", map[string]any{"tool": tool}))["yaml"]
	if !strings.Contains(back, "name: x\nadapter: cli\n") {
		t.Fatalf("yaml = %q", back)
	}
	// a is optional, so it can't be a plain argument.
	if problem := decode[map[string]string](t, u.do("POST", "/api/check", map[string]string{"yaml": yaml}))["problem"]; !strings.Contains(problem, "a") {
		t.Fatalf("problem = %q", problem)
	}
	fixed := strings.Replace(yaml, "type: object\n", "type: object\n  required: [a]\n", 1)
	if problem := decode[map[string]string](t, u.do("POST", "/api/check", map[string]string{"yaml": fixed}))["problem"]; problem != "" {
		t.Fatalf("problem = %q", problem)
	}
	expectStatus(t, u.do("POST", "/api/convert", map[string]any{"tool": map[string]any{"name": "x", "colour": "blue"}}), http.StatusUnprocessableEntity)
}

func TestUIReadsToolsFromWorkerYAML(t *testing.T) {
	u := newTestUI(t, nil)
	file := decode[uiFile](t, u.do("GET", "/api/builtin/builtin.echo", nil))
	if !strings.Contains(file.YAML, "name: builtin.echo") || file.Tool["adapter"] != "cli" {
		t.Fatalf("builtin = %+v", file)
	}
	expectStatus(t, u.do("GET", "/api/builtin/nope", nil), http.StatusNotFound)
}

func shTool(script string) string {
	return "name: sh.try\nadapter: cli\ncommand: [/bin/sh, -c, '" + script + "', sh, '{{input.who}}']\n" +
		"input_schema: { type: object, required: [who], properties: { who: { type: string } } }\n" +
		"result: { from: stdout }\noutputs: { made: out.txt, maybe: { path: nope.txt, optional: true } }\n"
}

func TestUIPreviewShowsTheCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	u := newTestUI(t, nil)
	rec := u.do("POST", "/api/preview", map[string]any{"yaml": shTool("echo $1"), "input": map[string]string{"who": "bob"}})
	call := decode[map[string]callView](t, rec)["call"]
	if strings.Join(call.Argv, " ") != "/bin/sh -c echo $1 sh bob" || call.Cwd != jobFolderLabel {
		t.Fatalf("call = %+v (%s)", call, rec.Body.String())
	}
	problem := decode[map[string]string](t, u.do("POST", "/api/preview", map[string]any{"yaml": shTool("echo $1"), "input": map[string]string{"who": "-rf"}}))["problem"]
	if !strings.Contains(problem, "-") {
		t.Fatalf("an option-like value wasn't refused: %q", problem)
	}
	problem = decode[map[string]string](t, u.do("POST", "/api/preview", map[string]any{"yaml": shTool("echo $1"), "input": map[string]any{}}))["problem"]
	if !strings.Contains(problem, "who") {
		t.Fatalf("missing input wasn't refused: %q", problem)
	}
}

func waitTry(t *testing.T, u *testUI, id string) tryView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		view := decode[tryView](t, u.do("GET", "/api/try/"+id, nil))
		if view.State != "running" {
			return view
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the try didn't finish")
	return tryView{}
}

func TestUITryRunsTheToolHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	u := newTestUI(t, nil)
	rec := u.do("POST", "/api/try", map[string]any{"yaml": shTool(`printf "hi %s" "$1"; echo made > out.txt; echo noise >&2`), "input": map[string]string{"who": "bob"}})
	expectStatus(t, rec, http.StatusOK)
	id := decode[map[string]string](t, rec)["id"]
	view := waitTry(t, u, id)
	if view.State != "succeeded" || string(view.Result) != `"hi bob"` || view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("try = %+v", view)
	}
	if !strings.Contains(view.Output, "noise") || !strings.Contains(view.Output, "hi bob") {
		t.Fatalf("output = %q", view.Output)
	}
	if len(view.Files) != 1 || view.Files[0].Name != "made.txt" {
		t.Fatalf("files = %+v", view.Files)
	}
	file := u.do("GET", "/api/try/"+id+"/files/made.txt", nil)
	expectStatus(t, file, http.StatusOK)
	if file.Body.String() != "made\n" || file.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(file.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %q %v", file.Body.String(), file.Header())
	}
	expectStatus(t, u.do("GET", "/api/try/"+id+"/files/nope.txt", nil), http.StatusNotFound)

	// A failure still shows what it printed and its exit code.
	rec = u.do("POST", "/api/try", map[string]any{"yaml": shTool(`echo oops; exit 3`), "input": map[string]string{"who": "x"}})
	view = waitTry(t, u, decode[map[string]string](t, rec)["id"])
	if view.State != "failed" || view.ExitCode == nil || *view.ExitCode != 3 || !strings.Contains(view.Output, "oops") || view.Failure == nil {
		t.Fatalf("failed try = %+v", view)
	}
	// It doesn't reach the gateway.
	u.w.mu.Lock()
	n := len(u.w.attempts)
	u.w.mu.Unlock()
	if n != 0 {
		t.Fatalf("a try became a job: %d attempts", n)
	}
}

func TestUITriesAreLimitedAndCanBeStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	u := newTestUI(t, nil)
	start := func() *httptest.ResponseRecorder {
		return u.do("POST", "/api/try", map[string]any{"yaml": shTool(`echo started; sleep 30`), "input": map[string]string{"who": "x"}})
	}
	var ids []string
	for range maxRunningTries {
		rec := start()
		expectStatus(t, rec, http.StatusOK)
		ids = append(ids, decode[map[string]string](t, rec)["id"])
	}
	expectStatus(t, start(), http.StatusTooManyRequests)
	began := time.Now()
	for _, id := range ids {
		expectStatus(t, u.do("DELETE", "/api/try/"+id, nil), http.StatusOK)
	}
	for _, id := range ids {
		if view := waitTry(t, u, id); view.State != "failed" {
			t.Fatalf("stopped try = %+v", view)
		}
	}
	if time.Since(began) > 15*time.Second {
		t.Fatal("stopping took too long")
	}
	expectStatus(t, start(), http.StatusOK)
	expectStatus(t, u.do("GET", "/api/try/nope", nil), http.StatusNotFound)

	// A tool that takes files can't be tried here yet.
	withFiles := "name: f\nadapter: cli\ncommand: [/bin/cat, '{{files.doc}}']\ninput_files: [doc]\ninput_schema: { type: object }\n"
	expectStatus(t, u.do("POST", "/api/try", map[string]any{"yaml": withFiles}), http.StatusUnprocessableEntity)
}

func TestUIShowsTheLog(t *testing.T) {
	u := newTestUI(t, nil)
	io.WriteString(u.s.opts.Logs, "one\ntwo\nthr")
	lines := decode[map[string][]LogLine](t, u.do("GET", "/api/logs?after=0", nil))["lines"]
	if len(lines) != 2 || lines[1].Text != "two" {
		t.Fatalf("lines = %+v", lines)
	}
	io.WriteString(u.s.opts.Logs, "ee\n")
	lines = decode[map[string][]LogLine](t, u.do("GET", "/api/logs?after="+jsonInt(lines[1].Seq), nil))["lines"]
	if len(lines) != 1 || lines[0].Text != "three" {
		t.Fatalf("lines = %+v", lines)
	}
}

func jsonInt(n int64) string {
	data, _ := json.Marshal(n)
	return string(data)
}

func TestLogRingKeepsTheNewestLines(t *testing.T) {
	r := NewLogRing(3)
	for i := range 5 {
		io.WriteString(r, "line "+jsonInt(int64(i))+"\n")
	}
	got := r.After(0)
	if len(got) != 3 || got[0].Text != "line 2" || got[2].Seq != 5 {
		t.Fatalf("got %+v", got)
	}
	if after := r.After(4); len(after) != 1 || after[0].Text != "line 4" {
		t.Fatalf("after 4 = %+v", after)
	}
	// A line that never ends doesn't grow without limit.
	io.WriteString(r, strings.Repeat("x", maxLineBytes+1))
	if len(r.partial) != 0 {
		t.Fatalf("kept %d bytes of an endless line", len(r.partial))
	}
}

func TestUIListenMustBeOnThisMachine(t *testing.T) {
	cases := map[string]string{
		"":               DefaultUIListen,
		"off":            "",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"[::1]:9000":     "[::1]:9000",
	}
	for in, want := range cases {
		m := Manifest{UIListen: in}
		if err := m.validateUI(); err != nil || m.UIAddress() != want {
			t.Errorf("%q: %q, %v", in, m.UIAddress(), err)
		}
	}
	for _, in := range []string{"0.0.0.0:7878", ":7878", "192.168.1.5:7878", "localhost:7878", "127.0.0.1", "127.0.0.1:99999"} {
		m := Manifest{UIListen: in}
		if err := m.validateUI(); err == nil {
			t.Errorf("%q was allowed", in)
		}
	}
}

func TestServeUIWritesAndRemovesItsSignInFile(t *testing.T) {
	path := writeWorker(t, "ui_listen: 127.0.0.1:0\n", nil)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(m, "node-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.ServeUI(ctx, UIOptions{ConfigPath: path})
		close(done)
	}()
	statePath := filepath.Join(m.StateDir, UIStateFile)
	var state UIState
	deadline := time.Now().Add(5 * time.Second)
	for {
		if state, err = ReadUIState(m.StateDir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no sign-in file")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info, err := os.Stat(statePath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("sign-in file mode %v, %v", info.Mode(), err)
	}
	req, _ := http.NewRequest("GET", state.URL+"api/status", nil)
	req.Header.Set("Authorization", "Bearer "+state.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the page didn't stop")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("the sign-in file was left behind")
	}
}
