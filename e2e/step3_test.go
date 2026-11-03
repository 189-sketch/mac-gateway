package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeService stands in for a local service such as a model server. It records
// each request, and each test decides how it answers.
type fakeService struct {
	*httptest.Server
	mu       sync.Mutex
	requests []seenRequest
	handler  func(w http.ResponseWriter, r *http.Request, body []byte)
}

type seenRequest struct {
	Method, Path, RawURL, Query, Body string
	Header                            http.Header
}

func newFakeService(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) *fakeService {
	t.Helper()
	s := &fakeService{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, seenRequest{r.Method, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Clone()})
		current := s.handler
		s.mu.Unlock()
		current(w, r, body)
	}))
	t.Cleanup(func() {
		s.CloseClientConnections()
		s.Close()
	})
	return s
}

func (s *fakeService) seen() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.requests...)
}

func (s *fakeService) count(pathPrefix string) int {
	n := 0
	for _, r := range s.seen() {
		if strings.HasPrefix(r.Path, pathPrefix) {
			n++
		}
	}
	return n
}

func anyObject(name, adapter, body string) string {
	return fmt.Sprintf("\n  - name: %s\n    adapter: %s\n    input_schema: { type: object }\n%s", name, adapter, body)
}

func (h *harness) runCapability(name string, input map[string]any) job {
	h.t.Helper()
	return h.submit(map[string]any{"capability": name, "input": input})
}

func wantError(t *testing.T, j job, status, code string, retryable bool) {
	t.Helper()
	if j.Status != status || j.Error == nil || j.Error.Code != code || j.Error.Retryable != retryable {
		t.Fatalf("job = %s %+v, want %s %s retryable=%v", j.Status, j.Error, status, code, retryable)
	}
}

func TestHTTPResultReturnedInline(t *testing.T) {
	t.Setenv("SERVICE_KEY", "hunter2")
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"response":"hello from the model","usage":{"tokens":12}}`)
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("llm.generate", "http", fmt.Sprintf(`    request:
      url: %s/api/{{input.model}}/generate?tag={{input.tag}}
      headers: { Authorization: "Bearer {{env.SERVICE_KEY}}" }
      json:
        prompt: "{{input.prompt}}"
        temperature: "{{input.temperature}}"
        top_k: "{{input.top_k}}"
        stream: false
    response: { result: json, result_field: response }
`, service.URL)))
	h.waitForCapability("llm.generate", 1)

	j := h.runCapability("llm.generate", map[string]any{
		"model": "llama 3", "tag": "a&b=c", "prompt": `say "hi"}, "stream": true, {"`, "temperature": 0.25,
	})
	j = h.waitTerminal(j.ID)
	if j.Status != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	var result string
	if err := json.Unmarshal(j.Result, &result); err != nil || result != "hello from the model" {
		t.Fatalf("result = %s, %v", j.Result, err)
	}
	if len(j.Artifacts) != 0 {
		t.Errorf("artifacts = %+v", j.Artifacts)
	}
	assertSingleResult(t, j)

	requests := service.seen()
	if len(requests) != 1 {
		t.Fatalf("requests = %+v", requests)
	}
	got := requests[0]
	if got.Method != "POST" || got.Path != "/api/llama 3/generate" || got.Query != "tag=a%26b%3Dc" {
		t.Errorf("request went to %s %s?%s", got.Method, got.Path, got.Query)
	}
	if got.Header.Get("Authorization") != "Bearer hunter2" {
		t.Errorf("auth header = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", got.Header.Get("Content-Type"))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("body %q: %v", got.Body, err)
	}
	if body["prompt"] != `say "hi"}, "stream": true, {"` || body["temperature"] != 0.25 || body["stream"] != false {
		t.Errorf("body = %+v", body)
	}
	if _, present := body["top_k"]; present {
		t.Errorf("an optional field nobody sent was included: %+v", body)
	}
}

func TestHTTPResponseSavedAsArtifact(t *testing.T) {
	audio := strings.Repeat("RIFF-audio-", 20000)
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { io.WriteString(w, audio) })
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("tts.remote", "http", fmt.Sprintf(`    request:
      method: POST
      url: %s/speak
      body: "{{input.text}}"
      headers: { Content-Type: text/plain }
    response: { file: audio/out.wav }
    outputs: { audio: audio/out.wav }
`, service.URL)))
	h.waitForCapability("tts.remote", 1)
	j := h.waitTerminal(h.runCapability("tts.remote", map[string]any{"text": "hello"}).ID)
	if j.Status != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	if got := h.artifact(j); got != audio {
		t.Errorf("artifact is %d bytes, want %d", len(got), len(audio))
	}
	if j.Artifacts[0].Name != "audio.wav" {
		t.Errorf("artifact name = %s", j.Artifacts[0].Name)
	}
	got := service.seen()[0]
	if got.Body != "hello" || got.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("request = %+v", got)
	}
}

func TestHTTPSendsInputFileAsBody(t *testing.T) {
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		fmt.Fprintf(w, `{"bytes":%d}`, len(body))
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("audio.transcribe", "http", fmt.Sprintf(`    input_files: [source]
    request:
      url: %s/transcribe
      body_file: "{{files.source}}"
    response: { result: json }
`, service.URL)))
	h.waitForCapability("audio.transcribe", 1)
	content := strings.Repeat("sound", 50000)
	fileID := h.uploadInputID("clip.wav", []byte(content))
	j := h.waitTerminal(h.submit(map[string]any{"capability": "audio.transcribe", "input": map[string]any{}, "files": map[string]string{"source": fileID}}).ID)
	if j.Status != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	if string(j.Result) != fmt.Sprintf(`{"bytes":%d}`, len(content)) {
		t.Errorf("result = %s", j.Result)
	}
	got := service.seen()[0]
	if got.Body != content || got.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("sent %d bytes as %q", len(got.Body), got.Header.Get("Content-Type"))
	}
}

func TestHTTPFailuresAreClassified(t *testing.T) {
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch r.URL.Path {
		case "/refuse":
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "no such model")
		case "/busy":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/not-json":
			io.WriteString(w, "<html>oops</html>")
		case "/no-field":
			io.WriteString(w, `{"other":1}`)
		case "/huge":
			io.WriteString(w, `"`+strings.Repeat("x", 2<<20)+`"`)
		case "/escapes":
			io.WriteString(w, strings.Repeat(`<`, 1<<20-1))
		default:
			io.WriteString(w, `{"ok":true}`)
		}
	})
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()

	capability := func(name, url, response string) string {
		return anyObject(name, "http", fmt.Sprintf("    request: { url: \"%s\" }\n    response: %s\n", url, response))
	}
	h := newHarness(t)
	h.startWorker("mac", h.url, false,
		capability("svc.refuse", service.URL+"/refuse", "{ result: json }"),
		capability("svc.busy", service.URL+"/busy", "{ result: json }"),
		capability("svc.notjson", service.URL+"/not-json", "{ result: json }"),
		capability("svc.nofield", service.URL+"/no-field", "{ result: json, result_field: answer }"),
		capability("svc.huge", service.URL+"/huge", "{ result: json }"),
		capability("svc.escapes", service.URL+"/escapes", "{ result: text }"),
		// Its check passes regardless, as if the service died just after being checked.
		capability("svc.down", goneURL+"/x", "{ result: json }")+"    check: { command: [/bin/sh, -c, \"exit 0\"] }\n",
		capability("svc.fine", service.URL+"/fine", "{ result: json }"),
		anyObject("svc.path", "http", fmt.Sprintf("    request: { url: \"%s/items/{{input.id}}\" }\n", service.URL)),
	)
	h.waitForCapability("svc.fine", 1)

	tests := []struct {
		capability, code, inMessage string
		retryable                   bool
		input                       map[string]any
	}{
		{capability: "svc.refuse", code: "SERVICE_ERROR", inMessage: "no such model"},
		{capability: "svc.busy", code: "SERVICE_UNAVAILABLE", retryable: true},
		{capability: "svc.notjson", code: "BAD_RESPONSE"},
		{capability: "svc.nofield", code: "BAD_RESPONSE", inMessage: "answer"},
		{capability: "svc.huge", code: "BAD_RESPONSE", inMessage: "larger than the 1048576 bytes"},
		{capability: "svc.down", code: "SERVICE_UNAVAILABLE", retryable: true},
		{capability: "svc.path", code: "INVALID_INPUT", inMessage: "URL path", input: map[string]any{"id": "../admin"}},
	}
	for _, tt := range tests {
		t.Run(tt.capability, func(t *testing.T) {
			input := tt.input
			if input == nil {
				input = map[string]any{}
			}
			j := h.waitTerminal(h.runCapability(tt.capability, input).ID)
			wantError(t, j, "failed", tt.code, tt.retryable)
			if !strings.Contains(j.Error.Message, tt.inMessage) {
				t.Errorf("message = %q, want %q", j.Error.Message, tt.inMessage)
			}
			if strings.Contains(j.Error.Message, "127.0.0.1") {
				t.Errorf("the service's address leaked into the error: %q", j.Error.Message)
			}
		})
	}
	if n := service.count("/items"); n != 0 {
		t.Errorf("a refused path still reached the service %d times", n)
	}

	// A reply within the size limit that grows when escaped must still give the
	// job an answer. An event too big for the gateway would be resent forever.
	j := h.waitTerminal(h.runCapability("svc.escapes", map[string]any{}).ID)
	wantError(t, j, "failed", "BAD_RESPONSE", false)

	// None of that may have disturbed the worker.
	j = h.waitTerminal(h.runCapability("svc.fine", map[string]any{}).ID)
	if j.Status != "succeeded" || string(j.Result) != `{"ok":true}` {
		t.Fatalf("job = %+v, result %s", j, j.Result)
	}
}

func TestHTTPRedirects(t *testing.T) {
	other := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { io.WriteString(w, `{"from":"other"}`) })
	var service *fakeService
	service = newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, service.URL+"/target", http.StatusFound)
		case "/away":
			http.Redirect(w, r, other.URL+"/secret", http.StatusFound)
		default:
			io.WriteString(w, `{"from":"same"}`)
		}
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false,
		anyObject("redirect.same", "http", fmt.Sprintf("    request: { url: \"%s/same\" }\n    response: { result: json }\n", service.URL)),
		anyObject("redirect.away", "http", fmt.Sprintf("    request: { url: \"%s/away\" }\n    response: { result: json }\n", service.URL)),
	)
	h.waitForCapability("redirect.away", 1)

	j := h.waitTerminal(h.runCapability("redirect.same", map[string]any{}).ID)
	if j.Status != "succeeded" || string(j.Result) != `{"from":"same"}` {
		t.Fatalf("same host: %+v, %s", j, j.Result)
	}
	j = h.waitTerminal(h.runCapability("redirect.away", map[string]any{}).ID)
	wantError(t, j, "failed", "SERVICE_ERROR", false)
	if n := len(other.seen()); n != 0 {
		t.Errorf("followed a redirect to a host the manifest doesn't name, %d requests", n)
	}
}

func TestHTTPTimeoutAndCancelStopTheRequest(t *testing.T) {
	disconnected := make(chan string, 4)
	release := make(chan struct{})
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-r.Context().Done():
			disconnected <- r.URL.Path
		case <-release:
		}
	})
	defer close(release)
	h := newHarness(t)
	h.startWorker("mac", h.url, false,
		anyObject("slow.timeout", "http", fmt.Sprintf("    request: { url: \"%s/timeout\" }\n    timeout: 500ms\n", service.URL)),
		anyObject("slow.cancel", "http", fmt.Sprintf("    request: { url: \"%s/cancel\" }\n    timeout: 1m\n", service.URL)),
	)
	h.waitForCapability("slow.cancel", 1)

	j := h.waitTerminal(h.runCapability("slow.timeout", map[string]any{}).ID)
	wantError(t, j, "timed_out", "TIMED_OUT", false)
	waitDisconnect(t, disconnected, "/timeout")

	running := h.runCapability("slow.cancel", map[string]any{})
	h.waitStatus(running.ID, "running")
	eventually(t, 5*time.Second, "the request to arrive", func() bool { return service.count("/cancel") == 1 })
	if status, raw := h.request("POST", "/v1/jobs/"+running.ID+"/cancel", h.apiKey, nil); status != 200 {
		t.Fatalf("cancel: %d %s", status, raw)
	}
	waitDisconnect(t, disconnected, "/cancel")
}

func waitDisconnect(t *testing.T, disconnected <-chan string, want string) {
	t.Helper()
	select {
	case got := <-disconnected:
		if got != want {
			t.Fatalf("disconnected %s, want %s", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the request to %s was never dropped", want)
	}
}

// asyncService answers like a service with its own job queue: POST /jobs returns
// an ID, GET /jobs/ID reports on it, and DELETE /jobs/ID cancels it.
type asyncService struct {
	*fakeService
	mu      sync.Mutex
	replies map[string]func(w http.ResponseWriter)
	polls   int
	script  func(poll int, w http.ResponseWriter)
}

func newAsyncService(t *testing.T, script func(poll int, w http.ResponseWriter)) *asyncService {
	t.Helper()
	a := &asyncService{script: script}
	a.fakeService = newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/jobs":
			io.WriteString(w, `{"data":{"job":"remote 1?x"}}`)
		case r.Method == "POST" && r.URL.Path == "/numeric":
			io.WriteString(w, `{"data":{"job":42}}`)
		case r.Method == "POST" && r.URL.Path == "/slash":
			io.WriteString(w, `{"data":{"job":"../admin"}}`)
		case r.Method == "POST":
			io.WriteString(w, `{"nothing":true}`)
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/result"):
			io.WriteString(w, "the final image")
		default:
			a.mu.Lock()
			a.polls++
			poll := a.polls
			a.mu.Unlock()
			a.script(poll, w)
		}
	})
	return a
}

func asyncCapability(name, base, submitPath, extra string) string {
	return anyObject(name, "http_async", fmt.Sprintf(`    request:
      url: %[1]s%[2]s
      json: "{{input}}"
    async:
      job_id_field: data.job
      poll:
        url: %[1]s/jobs/{{remote.id}}
        every: 50ms
        status_field: state
        succeeded: [done]
        failed: [error, dead]
        percent_field: fraction
        percent_scale: 100
        message_field: stage
        error_field: detail
        give_up_after: 2s
      cancel:
        method: DELETE
        url: %[1]s/jobs/{{remote.id}}
%[3]s`, base, submitPath, extra))
}

// The remote ID "remote 1?x" must arrive as one escaped path segment.
const escapedRemotePath = "/jobs/remote%201%3Fx"

func deletes(s *asyncService) []seenRequest {
	var out []seenRequest
	for _, r := range s.seen() {
		if r.Method == "DELETE" {
			out = append(out, r)
		}
	}
	return out
}

func TestHTTPAsyncFollowsJobAndFetchesResult(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) {
		switch {
		case poll == 1:
			io.WriteString(w, `{}`) // Some services report nothing until they start.
		case poll < 30:
			fmt.Fprintf(w, `{"state":"working","fraction":%g,"stage":"step %d"}`, float64(poll)/100, poll)
		default:
			io.WriteString(w, `{"state":"done"}`)
		}
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("image.generate", service.URL, "/jobs", fmt.Sprintf(`      fetch: { url: "%s/jobs/{{remote.id}}/result" }
    response: { file: out.png }
    outputs: { image: out.png }
`, service.URL)))
	h.waitForCapability("image.generate", 1)

	j := h.waitTerminal(h.runCapability("image.generate", map[string]any{"prompt": "a cat"}).ID)
	if j.Status != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	if got := h.artifact(j); got != "the final image" {
		t.Errorf("artifact = %q", got)
	}
	assertSingleResult(t, j)

	sawProgress := false
	for _, ev := range j.Events {
		if ev.Type != "progress" {
			continue
		}
		var p struct {
			Percent *float64 `json:"percent"`
			Message string   `json:"message"`
		}
		json.Unmarshal(ev.Payload, &p)
		if p.Percent != nil && *p.Percent > 0 && strings.HasPrefix(p.Message, "step ") {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Errorf("no progress events: %+v", j.Events)
	}

	requests := service.seen()
	if requests[0].Body != `{"prompt":"a cat"}` {
		t.Errorf("submitted %q", requests[0].Body)
	}
	if requests[1].RawURL != escapedRemotePath {
		t.Errorf("polled %q, want %q", requests[1].RawURL, escapedRemotePath)
	}
	if n := len(deletes(service)); n != 0 {
		t.Errorf("cancelled a job that finished, %d times", n)
	}
}

func TestHTTPAsyncResultFromFinalPoll(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) {
		io.WriteString(w, `{"state":"done","answer":{"text":"hi"}}`)
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("llm.queue", service.URL, "/numeric", "    response: { result: json, result_field: answer }\n"))
	h.waitForCapability("llm.queue", 1)
	j := h.waitTerminal(h.runCapability("llm.queue", map[string]any{}).ID)
	if j.Status != "succeeded" || string(j.Result) != `{"text":"hi"}` {
		t.Fatalf("job = %+v, result %s", j, j.Result)
	}
	if got := service.count("/jobs/42"); got != 1 {
		t.Errorf("a numeric job ID wasn't used: %+v", service.seen())
	}
}

func TestHTTPAsyncReportedFailureIsNotCancelled(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) {
		io.WriteString(w, `{"state":"dead","detail":"out of VRAM"}`)
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("image.fail", service.URL, "/jobs", ""))
	h.waitForCapability("image.fail", 1)
	j := h.waitTerminal(h.runCapability("image.fail", map[string]any{}).ID)
	wantError(t, j, "failed", "SERVICE_ERROR", false)
	if !strings.Contains(j.Error.Message, "out of VRAM") {
		t.Errorf("message = %q", j.Error.Message)
	}
	if n := len(deletes(service)); n != 0 {
		t.Errorf("cancelled a job the service had already failed, %d times", n)
	}
}

func TestHTTPAsyncBadSubmitReplies(t *testing.T) {
	service := newAsyncService(t, func(int, http.ResponseWriter) {})
	h := newHarness(t)
	h.startWorker("mac", h.url, false,
		asyncCapability("async.noid", service.URL, "/noid", ""),
		asyncCapability("async.slash", service.URL, "/slash", ""),
	)
	h.waitForCapability("async.slash", 1)
	for _, name := range []string{"async.noid", "async.slash"} {
		j := h.waitTerminal(h.runCapability(name, map[string]any{}).ID)
		wantError(t, j, "failed", "BAD_RESPONSE", false)
	}
	if n := service.count("/admin"); n != 0 {
		t.Errorf("a job ID from the service reached another endpoint, %d times", n)
	}
}

func TestHTTPAsyncCancelStopsTheServiceJob(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) { io.WriteString(w, `{"state":"working"}`) })
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("image.long", service.URL, "/jobs", "    timeout: 1m\n"))
	h.waitForCapability("image.long", 1)
	j := h.runCapability("image.long", map[string]any{})
	h.waitStatus(j.ID, "running")
	eventually(t, 10*time.Second, "polling to start", func() bool { return service.polledAtLeast(1) })
	if status, raw := h.request("POST", "/v1/jobs/"+j.ID+"/cancel", h.apiKey, nil); status != 200 {
		t.Fatalf("cancel: %d %s", status, raw)
	}
	eventually(t, 10*time.Second, "the service job to be cancelled", func() bool { return len(deletes(service)) == 1 })
	if got := deletes(service)[0].RawURL; got != escapedRemotePath {
		t.Errorf("cancelled %q", got)
	}
}

func (a *asyncService) polledAtLeast(n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.polls >= n
}

func TestHTTPAsyncTimeoutStopsTheServiceJob(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) { io.WriteString(w, `{"state":"working"}`) })
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("image.slow", service.URL, "/jobs", "    timeout: 500ms\n"))
	h.waitForCapability("image.slow", 1)
	j := h.waitTerminal(h.runCapability("image.slow", map[string]any{}).ID)
	wantError(t, j, "timed_out", "TIMED_OUT", false)
	eventually(t, 10*time.Second, "the service job to be cancelled", func() bool { return len(deletes(service)) == 1 })
}

// Two outages that are each within give_up_after, but together exceed it, must
// not add up: a successful check in between resets the clock.
func TestHTTPAsyncRidesOutBriefOutage(t *testing.T) {
	const outage = 1200 * time.Millisecond
	var started, recovered time.Time
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) {
		if started.IsZero() {
			started = time.Now()
		}
		switch {
		case time.Since(started) < outage:
			w.WriteHeader(http.StatusServiceUnavailable)
		case recovered.IsZero():
			recovered = time.Now()
			io.WriteString(w, `{"state":"working"}`)
		case time.Since(recovered) < outage:
			w.WriteHeader(http.StatusBadGateway)
		default:
			io.WriteString(w, `{"state":"done"}`)
		}
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("image.flaky", service.URL, "/jobs", ""))
	h.waitForCapability("image.flaky", 1)
	if j := h.waitTerminal(h.runCapability("image.flaky", map[string]any{}).ID); j.Status != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
}

func TestHTTPAsyncGivesUpOnLongOutage(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("image.gone", service.URL, "/jobs", ""))
	h.waitForCapability("image.gone", 1)
	j := h.waitTerminal(h.runCapability("image.gone", map[string]any{}).ID)
	wantError(t, j, "failed", "SERVICE_UNAVAILABLE", true)
	if len(deletes(service)) != 1 {
		t.Errorf("should try to cancel a job it walked away from: %+v", deletes(service))
	}
}

func TestHTTPAsyncForgottenJobFailsImmediately(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) })
	h := newHarness(t)
	h.startWorker("mac", h.url, false, asyncCapability("image.forgot", service.URL, "/jobs", "    timeout: 1m\n"))
	h.waitForCapability("image.forgot", 1)
	started := time.Now()
	j := h.waitTerminal(h.runCapability("image.forgot", map[string]any{}).ID)
	wantError(t, j, "failed", "SERVICE_ERROR", false)
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Errorf("took %s to notice", elapsed)
	}
}

func TestHTTPAsyncWorkerCrashStopsTheServiceJob(t *testing.T) {
	service := newAsyncService(t, func(poll int, w http.ResponseWriter) { io.WriteString(w, `{"state":"working"}`) })
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, asyncCapability("image.crash", service.URL, "/jobs", "    timeout: 1m\n"))
	h.waitForCapability("image.crash", 1)
	j := h.runCapability("image.crash", map[string]any{})
	h.waitStatus(j.ID, "running")
	eventually(t, 10*time.Second, "polling to start", func() bool { return service.polledAtLeast(1) })

	w.kill9()
	if n := len(deletes(service)); n != 0 {
		t.Fatalf("nothing should cancel the job yet, got %d", n)
	}
	w.start()
	eventually(t, 15*time.Second, "the restarted worker to cancel the service job", func() bool { return len(deletes(service)) == 1 })
	if got := deletes(service)[0].RawURL; got != escapedRemotePath {
		t.Errorf("cancelled %q", got)
	}
	final := h.waitTerminal(j.ID)
	wantError(t, final, "failed", "WORKER_RESTARTED", true)
}

func TestHTTPRetryAfterCrashOnlyWhenAllowed(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		io.WriteString(w, `{"attempt":2}`)
	})
	defer close(release)
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, anyObject("retry.sync", "http", fmt.Sprintf("    request: { url: \"%s/run\" }\n    response: { result: json }\n    retry_if_lost: true\n", service.URL)))
	h.waitForCapability("retry.sync", 1)
	j := h.runCapability("retry.sync", map[string]any{})
	h.waitStatus(j.ID, "running")
	eventually(t, 5*time.Second, "the first request", func() bool { return service.count("/run") == 1 })
	w.kill9()
	w.start()
	final := h.waitTerminal(j.ID)
	if final.Status != "succeeded" || string(final.Result) != `{"attempt":2}` || final.Attempts != 2 {
		t.Fatalf("job = %+v, result %s", final, final.Result)
	}
}

func runWorkerCheck(config string) (string, error) {
	out, err := exec.Command(workerBin, "--config", config, "--check").CombinedOutput()
	return string(out), err
}

func TestWorkerRefusesServiceCapabilityOnAnotherMachine(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	w.stop()
	raw, err := os.ReadFile(w.config)
	if err != nil {
		t.Fatal(err)
	}
	bad := string(raw) + anyObject("sneaky", "http", "    request: { url: \"http://169.254.169.254/latest/meta-data\" }\n")
	if err := os.WriteFile(w.config, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkerCheck(w.config)
	if err == nil || !strings.Contains(out, "isn't on this machine") {
		t.Fatalf("err = %v, output: %s", err, out)
	}
}

// --- Dashboard ---

type dashboardNode struct {
	Name         string                               `json:"name"`
	Online       bool                                 `json:"online"`
	OS           string                               `json:"os"`
	Jobs         struct{ Used, Total int }            `json:"jobs"`
	Resources    map[string]struct{ Used, Total int } `json:"resources"`
	Capabilities []string                             `json:"capabilities"`
	LastSeenAt   string                               `json:"last_seen_at"`
}

type dashboardOverview struct {
	Nodes        []dashboardNode             `json:"nodes"`
	Active       map[string]int              `json:"active"`
	Recent       map[string]int              `json:"recent"`
	Capabilities []string                    `json:"capabilities"`
	Storage      struct{ Used, Total int64 } `json:"storage"`
}

type dashboardJobRow struct {
	ID          string `json:"id"`
	Capability  string `json:"capability"`
	Status      string `json:"status"`
	Node        string `json:"node"`
	SubmittedBy string `json:"submitted_by"`
}

type dashboardJobList struct {
	Jobs []dashboardJobRow `json:"jobs"`
	Next string            `json:"next"`
}

func (h *harness) dashboardKey() string {
	h.t.Helper()
	return h.credential("key", "add", "ops", "--dashboard")
}

func (h *harness) dashboardGet(key, path string, into any) {
	h.t.Helper()
	status, raw := h.request("GET", "/v1/dashboard/"+path, key, nil)
	if status != 200 {
		h.t.Fatalf("GET %s: %d %s", path, status, raw)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		h.t.Fatalf("GET %s: %v in %s", path, err, raw)
	}
}

func TestDashboardKeysAreSeparateFromAgentKeys(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	dashboard := h.dashboardKey()
	if !strings.HasPrefix(dashboard, "fd_") {
		t.Errorf("dashboard key = %q", dashboard[:3])
	}

	// An agent key can't see other people's jobs through the dashboard.
	for _, path := range []string{"overview", "jobs", "jobs/job_x"} {
		if status, _ := h.request("GET", "/v1/dashboard/"+path, h.apiKey, nil); status != http.StatusForbidden {
			t.Errorf("agent key on %s: %d", path, status)
		}
		if status, _ := h.request("GET", "/v1/dashboard/"+path, "", nil); status != http.StatusUnauthorized {
			t.Errorf("no key on %s: %d", path, status)
		}
		if status, _ := h.request("GET", "/v1/dashboard/"+path, "fd_wrong", nil); status != http.StatusUnauthorized {
			t.Errorf("wrong key on %s: %d", path, status)
		}
	}

	j := h.waitTerminal(h.submitTTS("private words", 1).ID)
	if j.Status != "succeeded" {
		t.Fatalf("job = %+v", j)
	}

	// A dashboard key can't use anything in the agent API.
	for _, attempt := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/capabilities", nil},
		{"POST", "/v1/jobs", map[string]any{"capability": "tts.synthesize", "input": map[string]any{"text": "x", "steps": 1}}},
		{"GET", "/v1/jobs/" + j.ID, nil},
		{"GET", "/v1/jobs/" + j.ID + "/events", nil},
		{"POST", "/v1/jobs/" + j.ID + "/cancel", nil},
		{"POST", "/v1/files?name=x", map[string]any{}},
		{"GET", j.Artifacts[0].URL, nil},
	} {
		if status, raw := h.request(attempt.method, attempt.path, dashboard, attempt.body); status != http.StatusForbidden {
			t.Errorf("%s %s with a dashboard key: %d %s", attempt.method, attempt.path, status, raw)
		}
	}

	// Worker endpoints don't accept it either.
	if status, _ := h.request("GET", "/v1/worker/connect", dashboard, nil); status != http.StatusUnauthorized {
		t.Errorf("worker endpoint: %d", status)
	}

	h.credential("key", "revoke", "ops")
	if status, _ := h.request("GET", "/v1/dashboard/overview", dashboard, nil); status != http.StatusUnauthorized {
		t.Errorf("a revoked key still works: %d", status)
	}
}

func TestDashboardKeyOptionsAreChecked(t *testing.T) {
	h := newHarness(t)
	out, err := exec.Command(gatewayBin, "key", "add", "both", "--dashboard", "--capabilities", "*", "--data", h.dir+"/gateway").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "don't combine") {
		t.Errorf("err = %v, output: %s", err, out)
	}
	out, err = exec.Command(gatewayBin, "key", "add", "neither", "--data", h.dir+"/gateway").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--capabilities is required") {
		t.Errorf("err = %v, output: %s", err, out)
	}
}

func TestDashboardNeverShowsInputOrResults(t *testing.T) {
	const secretInput, secretResult = "SECRET-INPUT-7f3a", "SECRET-RESULT-9c1d"
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		fmt.Fprintf(w, `{"echo":%q}`, secretResult)
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("llm.private", "http", fmt.Sprintf("    request:\n      url: %s/run\n      json: \"{{input}}\"\n    response: { result: json }\n", service.URL)))
	h.waitForCapability("llm.private", 1)
	dashboard := h.dashboardKey()

	j := h.waitTerminal(h.runCapability("llm.private", map[string]any{"prompt": secretInput}).ID)
	if j.Status != "succeeded" || !strings.Contains(string(j.Result), secretResult) {
		t.Fatalf("the agent should get its result: %+v %s", j, j.Result)
	}

	for _, path := range []string{"overview", "jobs", "jobs/" + j.ID} {
		status, raw := h.request("GET", "/v1/dashboard/"+path, dashboard, nil)
		if status != 200 {
			t.Fatalf("%s: %d %s", path, status, raw)
		}
		for _, secret := range []string{secretInput, secretResult} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s shows %s: %s", path, secret, raw)
			}
		}
	}
	var detail struct {
		Events []jobEvent `json:"events"`
	}
	h.dashboardGet(dashboard, "jobs/"+j.ID, &detail)
	sawCompleted := false
	for _, ev := range detail.Events {
		sawCompleted = sawCompleted || ev.Type == "completed"
	}
	if !sawCompleted {
		t.Errorf("the history should still say the job completed: %+v", detail.Events)
	}
}

func TestDashboardOverview(t *testing.T) {
	h := newHarness(t)
	// One machine that's connected and busy, one that has gone away, one that has
	// never connected, and one that's been revoked.
	h.startWorker("busy", h.url, false, `
  - name: heavy.job
    adapter: cli
    command: [@TOOL@, sleep-in-group, "{{workdir}}/out", "20"]
    env: { MARKER_DIR: @MARKERS@ }
    input_schema: { type: object }
    requires: { heavy: 1 }
    timeout: 1m
`)
	away := h.startWorker("away", h.url, false)
	h.credential("node", "add", "never")
	h.credential("node", "add", "gone")
	h.credential("node", "revoke", "gone")
	h.waitForCapability("tts.synthesize", 2)
	dashboard := h.dashboardKey()
	away.stop()

	j := h.submit(map[string]any{"capability": "heavy.job", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	done := h.waitTerminal(h.submit(map[string]any{"capability": "tts.synthesize", "input": map[string]any{"text": "hello", "steps": 1}, "selector": map[string]string{"name": "busy"}}).ID)
	if done.Status != "succeeded" {
		t.Fatalf("job = %+v", done)
	}

	var view dashboardOverview
	eventually(t, 10*time.Second, "the overview to settle", func() bool {
		h.dashboardGet(dashboard, "overview", &view)
		return len(view.Nodes) == 3 && !view.Nodes[0].Online && view.Nodes[1].Online && view.Nodes[1].Jobs.Used == 1
	})
	byName := map[string]dashboardNode{}
	for _, node := range view.Nodes {
		byName[node.Name] = node
	}
	if _, listed := byName["gone"]; listed {
		t.Error("a revoked machine is listed")
	}
	if node := byName["never"]; node.Online || node.LastSeenAt != "" {
		t.Errorf("never = %+v", node)
	}
	if node := byName["away"]; node.Online || node.LastSeenAt == "" {
		t.Errorf("away = %+v", node)
	}
	node := byName["busy"]
	if !node.Online || node.OS == "" || node.Jobs.Used != 1 || node.Jobs.Total != 4 {
		t.Errorf("busy = %+v", node)
	}
	if use := node.Resources["heavy"]; use.Used != 1 || use.Total != 1 {
		t.Errorf("heavy = %+v", use)
	}
	if !contains(node.Capabilities, "heavy.job") || !contains(view.Capabilities, "heavy.job") || !contains(view.Capabilities, "tts.synthesize") {
		t.Errorf("capabilities = %v, %v", node.Capabilities, view.Capabilities)
	}
	if view.Active["running"] != 1 {
		t.Errorf("active = %+v", view.Active)
	}
	if view.Recent["succeeded"] != 1 {
		t.Errorf("recent = %+v", view.Recent)
	}
	if view.Storage.Used <= 0 || view.Storage.Total <= 0 {
		t.Errorf("storage = %+v", view.Storage)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestDashboardJobListFiltersAndPages(t *testing.T) {
	h := newHarness(t)
	h.startWorker("one", h.url, false)
	h.startWorker("two", h.url, false, `
  - name: other.job
    adapter: cli
    command: [@TOOL@, normal, "{{workdir}}/out.wav", "1"]
    env: { MARKER_DIR: @MARKERS@, TOOL_NODE: x }
    input_schema: { type: object }
    outputs: { audio: out.wav }
`)
	h.waitForCapability("other.job", 1)
	h.waitForCapability("tts.synthesize", 2)
	other := h.credential("key", "add", "someone-else", "--capabilities", "*")
	dashboard := h.dashboardKey()

	const total = 7
	ids := map[string]bool{}
	for i := 0; i < total; i++ {
		j := h.submit(map[string]any{"capability": "tts.synthesize", "input": map[string]any{"text": "x", "steps": 1}, "selector": map[string]string{"name": "one"}})
		ids[j.ID] = true
		h.waitTerminal(j.ID)
	}
	status, raw := h.request("POST", "/v1/jobs", other, map[string]any{"capability": "other.job", "input": map[string]any{}})
	if status != http.StatusAccepted {
		t.Fatalf("%d %s", status, raw)
	}
	var theirs job
	json.Unmarshal(raw, &theirs)
	eventually(t, 30*time.Second, "their job to finish", func() bool {
		status, raw := h.request("GET", "/v1/jobs/"+theirs.ID, other, nil)
		var j job
		json.Unmarshal(raw, &j)
		return status == 200 && j.Terminal
	})

	// Every job, across pages, exactly once and newest first.
	seen := map[string]bool{}
	var order []string
	cursor := ""
	for pages := 0; ; pages++ {
		var page dashboardJobList
		path := "jobs?limit=3"
		if cursor != "" {
			path += "&before=" + cursor
		}
		h.dashboardGet(dashboard, path, &page)
		for _, j := range page.Jobs {
			if seen[j.ID] {
				t.Errorf("%s appeared twice", j.ID)
			}
			seen[j.ID] = true
			order = append(order, j.ID)
		}
		if page.Next == "" {
			break
		}
		if pages > total {
			t.Fatal("paging never ended")
		}
		cursor = page.Next
	}
	if len(seen) != total+1 {
		t.Errorf("listed %d jobs, want %d", len(seen), total+1)
	}
	if order[0] != theirs.ID {
		t.Errorf("newest job should come first: %v", order)
	}

	type row = dashboardJobRow
	check := func(query string, want int, verify func(row) bool) {
		t.Helper()
		var page dashboardJobList
		h.dashboardGet(dashboard, "jobs?"+query, &page)
		if len(page.Jobs) != want {
			t.Errorf("%s: %d jobs, want %d", query, len(page.Jobs), want)
		}
		for _, j := range page.Jobs {
			if !verify(j) {
				t.Errorf("%s: wrong job %+v", query, j)
			}
		}
	}
	check("capability=other.job", 1, func(j row) bool { return j.ID == theirs.ID && j.SubmittedBy == "someone-else" })
	check("node=one", total, func(j row) bool { return j.Node == "one" && j.SubmittedBy == "agent" })
	check("node=two", 1, func(j row) bool { return j.Node == "two" })
	check("status=succeeded", total+1, func(j row) bool { return j.Status == "succeeded" })
	check("status=failed", 0, func(row) bool { return false })
	check("status=active", 0, func(row) bool { return false })
	check("status=succeeded&capability=other.job&node=one", 0, func(row) bool { return false })

	for _, bad := range []string{"status=nonsense", "limit=0", "limit=x", "before=nonsense"} {
		if status, _ := h.request("GET", "/v1/dashboard/jobs?"+bad, dashboard, nil); status != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, status)
		}
	}
	if status, _ := h.request("GET", "/v1/dashboard/jobs/job_nonexistent", dashboard, nil); status != http.StatusNotFound {
		t.Errorf("missing job: %d", status)
	}
}

func TestDashboardShowsFailureDetail(t *testing.T) {
	service := newFakeService(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "<img src=x onerror=alert(1)>")
	})
	h := newHarness(t)
	h.startWorker("mac", h.url, false, anyObject("svc.bad", "http", fmt.Sprintf("    request: { url: \"%s/x\" }\n", service.URL)))
	h.waitForCapability("svc.bad", 1)
	dashboard := h.dashboardKey()
	j := h.waitTerminal(h.runCapability("svc.bad", map[string]any{}).ID)

	var detail struct {
		Status string     `json:"status"`
		Error  *jobError  `json:"error"`
		Node   string     `json:"node"`
		Events []jobEvent `json:"events"`
	}
	h.dashboardGet(dashboard, "jobs/"+j.ID, &detail)
	if detail.Status != "failed" || detail.Error == nil || detail.Error.Code != "SERVICE_ERROR" || detail.Node != "mac" || len(detail.Events) != 2 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestDashboardPageIsServedWithProtections(t *testing.T) {
	h := newHarness(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(h.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/dashboard/" {
		t.Errorf("/ answered %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for path, wantType := range map[string]string{"/dashboard/": "text/html", "/dashboard/app.js": "javascript", "/dashboard/style.css": "text/css"} {
		resp, err := http.Get(h.url + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(body) == 0 || !strings.Contains(resp.Header.Get("Content-Type"), wantType) {
			t.Errorf("%s: %d, %d bytes, type %q", path, resp.StatusCode, len(body), resp.Header.Get("Content-Type"))
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: CSP = %q", path, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
	// The page holds no data, so it needs no key. Nothing else may leak through.
	for _, path := range []string{"/dashboard/../gateway.db", "/dashboard/nothing.js"} {
		resp, err := http.Get(h.url + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s answered 200", path)
		}
	}
	// A bad API path must still be an error rather than the page.
	if status, _ := h.request("GET", "/v1/nothing", h.apiKey, nil); status != http.StatusNotFound {
		t.Errorf("unknown path: %d", status)
	}
}

func TestWorkerLogFileIsRotated(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	w.stop()
	logPath := filepath.Join(h.dir, "worker.log")
	if err := os.WriteFile(logPath, []byte("old log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := func() *exec.Cmd {
		cmd := exec.Command(workerBin, "--config", w.config, "--log-file", logPath)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	stop := func(cmd *exec.Cmd) {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	}
	read := func(path string) string {
		raw, _ := os.ReadFile(path)
		return string(raw)
	}
	connected := func() bool { return strings.Contains(read(logPath), "connected to gateway") }

	cmd := start()
	eventually(t, 10*time.Second, "the worker to log its connection", connected)
	stop(cmd)
	if !strings.HasPrefix(read(logPath), "old log") {
		t.Errorf("a small log should be appended to: %q", read(logPath))
	}
	if _, err := os.Stat(logPath + ".1"); err == nil {
		t.Error("a small log shouldn't be rotated")
	}

	if err := os.Truncate(logPath, 11<<20); err != nil {
		t.Fatal(err)
	}
	cmd = start()
	eventually(t, 10*time.Second, "the worker to log to a new file", connected)
	stop(cmd)
	if info, err := os.Stat(logPath + ".1"); err != nil || info.Size() != 11<<20 {
		t.Errorf("the old log wasn't kept: %v %v", info, err)
	}
	if info, _ := os.Stat(logPath); info.Size() > 1<<20 {
		t.Errorf("the new log is %d bytes", info.Size())
	}
}
