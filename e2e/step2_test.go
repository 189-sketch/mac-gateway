package e2e

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- Helpers ---

type sseEvent struct {
	ID   string
	Name string
	Data string
}

// readStream reads an event stream until stop returns true or the stream ends.
func (h *harness) readStream(jobID, lastEventID string, stop func(sseEvent) bool) []sseEvent {
	h.t.Helper()
	req, err := http.NewRequest("GET", h.url+"/v1/jobs/"+jobID+"/events", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		h.t.Fatalf("stream status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		h.t.Errorf("content type = %q", got)
	}
	var events []sseEvent
	var current sseEvent
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			current.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			current.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.Data = strings.TrimPrefix(line, "data: ")
		case line == "" && current.Name != "":
			events = append(events, current)
			if stop != nil && stop(current) {
				return events
			}
			current = sseEvent{}
		}
	}
	return events
}

func streamIDs(events []sseEvent) []string {
	var ids []string
	for _, ev := range events {
		if ev.ID != "" {
			ids = append(ids, ev.ID)
		}
	}
	return ids
}

func (h *harness) uploadInput(token, name string, content []byte) (int, []byte) {
	h.t.Helper()
	req, err := http.NewRequest("POST", h.url+"/v1/files?name="+name, bytes.NewReader(content))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func (h *harness) uploadInputID(name string, content []byte) string {
	h.t.Helper()
	status, raw := h.uploadInput(h.apiKey, name, content)
	if status != http.StatusCreated {
		h.t.Fatalf("upload: status %d: %s", status, raw)
	}
	var file struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		h.t.Fatal(err)
	}
	return file.ID
}

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func sha256Text(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (h *harness) logContains(text string) bool {
	raw, err := os.ReadFile(h.logs.Name())
	return err == nil && strings.Contains(string(raw), text)
}

func cancelJob(h *harness, id string) job {
	h.t.Helper()
	status, raw := h.request("POST", "/v1/jobs/"+id+"/cancel", h.apiKey, nil)
	if status != 200 {
		h.t.Fatalf("cancel: status %d: %s", status, raw)
	}
	var j job
	if err := json.Unmarshal(raw, &j); err != nil {
		h.t.Fatal(err)
	}
	return j
}

// --- Event stream ---

func TestEventStreamDeliversProgressThenEnds(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	j := h.submitTTS("streamed", 12)

	events := h.readStream(j.ID, "", nil)
	if len(events) == 0 || events[0].Name != "status" {
		t.Fatalf("the stream should open with the current status: %+v", events)
	}
	names := map[string]int{}
	for _, ev := range events {
		names[ev.Name]++
	}
	if names["started"] != 1 || names["completed"] != 1 || names["progress"] == 0 {
		t.Errorf("event counts = %v", names)
	}

	last := events[len(events)-1]
	if last.Name != "end" {
		t.Fatalf("the stream should finish with end: %+v", last)
	}
	var final job
	if err := json.Unmarshal([]byte(last.Data), &final); err != nil {
		t.Fatal(err)
	}
	requireSucceeded(t, final)
	if len(final.Artifacts) != 1 {
		t.Errorf("end event has no artifacts: %s", last.Data)
	}
}

func TestEventStreamResumesWithoutGapsOrRepeats(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	j := h.submitTTS("resumed stream", 16)

	// Disconnect after two stored events, then pick up from the last one.
	var seen int
	first := h.readStream(j.ID, "", func(ev sseEvent) bool {
		if ev.ID != "" {
			seen++
		}
		return seen == 2
	})
	firstIDs := streamIDs(first)
	if len(firstIDs) != 2 {
		t.Fatalf("first connection saw %v", firstIDs)
	}
	second := h.readStream(j.ID, firstIDs[1], nil)
	combined := append(firstIDs, streamIDs(second)...)

	done := h.waitTerminal(j.ID)
	if len(combined) != len(done.Events) {
		t.Fatalf("stream delivered %d events, the job has %d: %v", len(combined), len(done.Events), combined)
	}
	for i, ev := range done.Events {
		if combined[i] != fmt.Sprint(ev.ID) {
			t.Errorf("event %d: stream id %s, stored id %d", i, combined[i], ev.ID)
		}
	}
}

func TestEventStreamReportsLostWorker(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	h.startWorker("mac", p.url(), false)
	h.waitForCapability("tts.synthesize", 1)
	j := h.submitTTS("outage", 32)
	h.waitStatus(j.ID, "running")

	result := make(chan []sseEvent, 1)
	go func() { result <- h.readStream(j.ID, "", nil) }()
	p.cut()
	h.waitStatus(j.ID, "lost")
	p.restore()

	select {
	case events := <-result:
		statuses := []string{}
		for _, ev := range events {
			if ev.Name == "status" {
				var current job
				if err := json.Unmarshal([]byte(ev.Data), &current); err != nil {
					t.Fatal(err)
				}
				statuses = append(statuses, current.Status)
			}
		}
		joined := strings.Join(statuses, ",")
		if !strings.Contains(joined, "lost") || !strings.Contains(joined, "running") {
			t.Errorf("statuses = %s", joined)
		}
		if events[len(events)-1].Name != "end" {
			t.Errorf("last event = %s", events[len(events)-1].Name)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("the stream never ended")
	}
}

func TestEventStreamReplaysFinishedJob(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	done := runSimple(h, "tts.synthesize", map[string]any{"text": "x", "steps": 1})
	events := h.readStream(done.ID, "", nil)
	if got := len(streamIDs(events)); got != len(done.Events) {
		t.Errorf("replayed %d events, want %d", got, len(done.Events))
	}
	if events[len(events)-1].Name != "end" {
		t.Errorf("last event = %+v", events[len(events)-1])
	}
}

func TestEventStreamAccessControl(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	other := h.credential("key", "add", "other", "--capabilities", "*")
	j := h.submitTTS("private", 1)

	path := "/v1/jobs/" + j.ID + "/events"
	if status, _ := h.request("GET", path, "", nil); status != 401 {
		t.Errorf("no key: status %d", status)
	}
	if status, _ := h.request("GET", path, other, nil); status != 404 {
		t.Errorf("another key: status %d", status)
	}
	if status, _ := h.request("GET", path+"?after=banana", h.apiKey, nil); status != 400 {
		t.Errorf("bad cursor: status %d", status)
	}
}

// --- Cancellation ---

func TestCancelStopsRunningJobAndItsChildren(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)
	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	eventually(t, 5*time.Second, "markers", func() bool { return w.marker("pid") != 0 && w.marker("grandchild") != 0 })
	pid, grandchild := w.marker("pid"), w.marker("grandchild")

	cancelled := cancelJob(h, j.ID)
	if cancelled.Status != "cancelled" || !cancelled.Terminal {
		t.Fatalf("got %+v", cancelled)
	}
	if cancelled.Error != nil {
		t.Errorf("a cancellation isn't an error: %+v", cancelled.Error)
	}
	eventually(t, 10*time.Second, "command to stop", func() bool { return !processAlive(pid) && !processAlive(grandchild) })
	eventually(t, 10*time.Second, "worker cleanup", func() bool { return w.heldAttempts() == 0 })
	time.Sleep(500 * time.Millisecond)
	if status := h.getJob(j.ID).Status; status != "cancelled" {
		t.Errorf("status changed to %s after cancelling", status)
	}
}

func TestCancelQueuedJobNeverRuns(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	w.stop()
	eventually(t, 5*time.Second, "node to go offline", func() bool {
		_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
		return strings.Contains(string(raw), `"online_nodes":0`)
	})
	j := h.submitTTS("never", 1)
	if status := cancelJob(h, j.ID).Status; status != "cancelled" {
		t.Fatalf("status = %s", status)
	}
	w.start()
	h.waitForCapability("tts.synthesize", 1)
	time.Sleep(500 * time.Millisecond)
	if w.runs() != 0 {
		t.Errorf("cancelled job ran %d times", w.runs())
	}
}

func TestCancelFinishedJobChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	done := runSimple(h, "tts.synthesize", map[string]any{"text": "keep", "steps": 1})
	requireSucceeded(t, done)
	after := cancelJob(h, done.ID)
	if after.Status != "succeeded" || len(after.Artifacts) != 1 {
		t.Fatalf("got %+v", after)
	}
	if got := h.artifact(after); got != "node=mac text=keep" {
		t.Errorf("artifact = %q", got)
	}
}

func TestCancelIsRepeatableAndPrivate(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	other := h.credential("key", "add", "other", "--capabilities", "*")
	j := h.submitTTS("mine", 200)
	h.waitStatus(j.ID, "running")
	if status, _ := h.request("POST", "/v1/jobs/"+j.ID+"/cancel", other, nil); status != 404 {
		t.Errorf("another key: status %d", status)
	}
	if status := h.getJob(j.ID).Status; status != "running" {
		t.Errorf("another key changed the job: %s", status)
	}
	cancelJob(h, j.ID)
	if status := cancelJob(h, j.ID).Status; status != "cancelled" {
		t.Errorf("second cancel: %s", status)
	}
}

func TestCancelWhileWorkerOfflineStopsItOnReconnect(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	w := h.startWorker("mac", p.url(), false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)
	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	eventually(t, 5*time.Second, "pid marker", func() bool { return w.marker("pid") != 0 })
	pid := w.marker("pid")

	p.cut()
	if status := cancelJob(h, j.ID).Status; status != "cancelled" {
		t.Fatalf("status = %s", status)
	}
	if !processAlive(pid) {
		t.Fatal("the command should still run while its worker is offline")
	}
	p.restore()
	eventually(t, 15*time.Second, "command to stop", func() bool { return !processAlive(pid) })
	eventually(t, 10*time.Second, "worker cleanup", func() bool { return w.heldAttempts() == 0 })
}

func TestCancelFreesCapacityForNextJob(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, heavyCapability)
	h.waitForCapability("heavy.task", 1)
	submit := func(steps int) job {
		return h.submit(map[string]any{"capability": "heavy.task", "input": map[string]any{"steps": steps}})
	}
	blocking, next := submit(400), submit(1)
	h.waitStatus(blocking.ID, "running")
	if status := h.getJob(next.ID).Status; status != "queued" {
		t.Fatalf("next job status = %s", status)
	}
	cancelJob(h, blocking.ID)
	requireSucceeded(t, h.waitTerminal(next.ID))
}

// --- Input files ---

func TestInputFileReachesCommandAndResultMatches(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.copy", 1)
	content := randomBytes(t, 300<<10)
	fileID := h.uploadInputID("clip.mp4", content)

	j := h.submit(map[string]any{"capability": "test.copy", "files": map[string]string{"source": fileID}})
	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	if got := h.artifact(done); got != string(content) {
		t.Errorf("result has %d bytes and sha %s, want %d and %s", len(got), sha256Text([]byte(got)), len(content), sha256Text(content))
	}
	// A rerun needs its input too, and the owner can read it back.
	status, raw := h.request("GET", "/v1/files/"+fileID, h.apiKey, nil)
	if status != 200 || !bytes.Equal(raw, content) {
		t.Errorf("reading the input back: status %d, %d bytes", status, len(raw))
	}
}

func TestInputFilesAreValidated(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.copy", 1)
	other := h.credential("key", "add", "other", "--capabilities", "*")
	_, raw := h.uploadInput(other, "theirs.bin", []byte("not yours"))
	var theirs struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &theirs); err != nil {
		t.Fatal(err)
	}
	mine := h.uploadInputID("mine.bin", []byte("mine"))

	cases := []struct {
		name  string
		cap   string
		files map[string]string
		code  string
	}{
		{"missing", "test.copy", nil, "INVALID_INPUT"},
		{"wrong field", "test.copy", map[string]string{"other": mine}, "INVALID_INPUT"},
		{"extra field", "test.copy", map[string]string{"source": mine, "other": mine}, "INVALID_INPUT"},
		{"not needed", "tts.synthesize", map[string]string{"source": mine}, "INVALID_INPUT"},
		{"unknown id", "test.copy", map[string]string{"source": "file_nope"}, "FILE_UNAVAILABLE"},
		{"another key's file", "test.copy", map[string]string{"source": theirs.ID}, "FILE_UNAVAILABLE"},
	}
	for _, c := range cases {
		input := map[string]any{}
		if c.cap == "tts.synthesize" {
			input = map[string]any{"text": "x", "steps": 1}
		}
		status, raw := h.request("POST", "/v1/jobs", h.apiKey, map[string]any{"capability": c.cap, "input": input, "files": c.files})
		if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), c.code) {
			t.Errorf("%s: status %d: %s", c.name, status, raw)
		}
	}

	if status, _ := h.request("GET", "/v1/files/"+theirs.ID, h.apiKey, nil); status != 404 {
		t.Errorf("reading another key's file: status %d", status)
	}
	for _, name := range []string{"../../evil", ".hidden", "a%2Fb", "with%20space"} {
		if status, raw := h.uploadInput(h.apiKey, name, []byte("x")); status != 400 {
			t.Errorf("name %q: status %d: %s", name, status, raw)
		}
	}
	if status, _ := h.uploadInput("", "fine.bin", []byte("x")); status != 401 {
		t.Errorf("upload without a key: status %d", status)
	}
}

func TestWorkerFileEndpointIsScopedToTheAttempt(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)
	otherToken := h.credential("node", "add", "other")
	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	attempt := h.getJob(j.ID).Events[0].Attempt
	path := "/v1/worker/attempts/" + attempt + "/files/source"

	if status, _ := h.request("GET", path, h.apiKey, nil); status != 401 {
		t.Errorf("agent key: status %d", status)
	}
	if status, _ := h.request("GET", path, otherToken, nil); status != 410 {
		t.Errorf("another node: status %d", status)
	}
	if status, _ := h.request("GET", path, w.token, nil); status != 404 {
		t.Errorf("a field this job doesn't have: status %d", status)
	}
	cancelJob(h, j.ID)
	if status, _ := h.request("GET", path, w.token, nil); status != 410 {
		t.Errorf("after the attempt ended: status %d", status)
	}
}

func TestInputFileOutlivesExpiryWhileJobNeedsIt(t *testing.T) {
	h := newHarness(t, "--artifact-ttl", "1s")
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.copy", 1)
	w.stop()
	eventually(t, 5*time.Second, "node to go offline", func() bool {
		_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
		return strings.Contains(string(raw), `"online_nodes":0`)
	})

	content := []byte("needed later")
	fileID := h.uploadInputID("needed.bin", content)
	j := h.submit(map[string]any{"capability": "test.copy", "files": map[string]string{"source": fileID}})
	time.Sleep(2500 * time.Millisecond) // Well past the 1s expiry.
	w.start()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)

	// Once nothing needs it, the expired input is removed from disk too.
	eventually(t, 10*time.Second, "file removal", func() bool {
		path := filepath.Join(h.dir, "gateway", "files", fileID)
		_, err := os.Stat(path)
		return errors.Is(err, os.ErrNotExist)
	})
}

// --- Resumable transfers ---

// The cut lands mid-transfer because the proxy is throttled. Finding the log line
// proves the worker continued from an offset rather than starting over.
func TestUploadResumesAfterNetworkCut(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	p.bytesPerSecond.Store(2 << 20)
	h.startWorker("mac", p.url(), false, scenarioCapabilities)
	h.waitForCapability("test.large", 1)

	j := h.submit(map[string]any{"capability": "test.large", "input": map[string]any{"megabytes": 8}})
	eventually(t, 20*time.Second, "upload to start", func() bool {
		current := h.getJob(j.ID)
		return current.Progress != nil && current.Progress.Stage == "uploading"
	})
	time.Sleep(time.Second)
	p.cut()
	time.Sleep(500 * time.Millisecond)
	p.restore()

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	if !h.logContains("resuming upload") {
		t.Error("the upload restarted from the beginning instead of resuming")
	}
	got := h.artifact(done)
	if len(got) != 8<<20 || sha256Text([]byte(got)) != done.Artifacts[0].SHA256 {
		t.Errorf("result has %d bytes and doesn't match its checksum", len(got))
	}
	// A finished upload leaves nothing behind.
	entries, err := os.ReadDir(filepath.Join(h.dir, "gateway", "partial"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d partial uploads left behind", len(entries))
	}
}

func TestDownloadResumesAfterNetworkCut(t *testing.T) {
	h := newHarness(t)
	p := h.newProxy()
	p.bytesPerSecond.Store(2 << 20)
	h.startWorker("mac", p.url(), false, scenarioCapabilities)
	h.waitForCapability("test.copy", 1)
	content := randomBytes(t, 8<<20)
	fileID := h.uploadInputID("big.bin", content)

	j := h.submit(map[string]any{"capability": "test.copy", "files": map[string]string{"source": fileID}})
	eventually(t, 20*time.Second, "download to start", func() bool {
		current := h.getJob(j.ID)
		return current.Progress != nil && current.Progress.Stage == "downloading"
	})
	time.Sleep(time.Second)
	p.cut()
	time.Sleep(500 * time.Millisecond)
	p.restore()
	p.bytesPerSecond.Store(0) // The result upload isn't what's being tested here.

	done := h.waitTerminal(j.ID)
	requireSucceeded(t, done)
	if !h.logContains("resuming download") {
		t.Error("the download restarted from the beginning instead of resuming")
	}
	if got := h.artifact(done); got != string(content) {
		t.Errorf("result has %d bytes and doesn't match the input", len(got))
	}
}

func TestResumableUploadProtocol(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)
	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	attempt := h.getJob(j.ID).Events[0].Attempt
	content := []byte("0123456789")
	sum := sha256Text(content)

	send := func(method, name, checksum string, offset, length int, body []byte) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, h.url+"/v1/worker/attempts/"+attempt+"/artifacts/"+name, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+w.token)
		req.Header.Set("X-Content-SHA256", checksum)
		if method == "PUT" {
			req.Header.Set("Upload-Offset", fmt.Sprint(offset))
			req.Header.Set("Upload-Length", fmt.Sprint(length))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	expect := func(step string, resp *http.Response, status int, offset string) {
		t.Helper()
		if resp.StatusCode != status {
			t.Errorf("%s: status %d, want %d", step, resp.StatusCode, status)
		}
		if offset != "" && resp.Header.Get("Upload-Offset") != offset {
			t.Errorf("%s: offset %q, want %q", step, resp.Header.Get("Upload-Offset"), offset)
		}
	}

	expect("nothing sent yet", send("HEAD", "a.bin", sum, 0, 0, nil), 200, "0")
	expect("first half", send("PUT", "a.bin", sum, 0, 10, content[:5]), 202, "5")
	expect("position", send("HEAD", "a.bin", sum, 0, 0, nil), 200, "5")
	expect("wrong position", send("PUT", "a.bin", sum, 3, 10, content[3:]), 409, "5")
	expect("position unchanged", send("HEAD", "a.bin", sum, 0, 0, nil), 200, "5")
	expect("second half", send("PUT", "a.bin", sum, 5, 10, content[5:]), 201, "")
	if got := send("HEAD", "a.bin", sum, 0, 0, nil).Header.Get("Upload-Complete"); got != "true" {
		t.Errorf("a stored file should report complete, got %q", got)
	}

	// A file that fails its checksum is thrown away, so the retry starts clean.
	wrong := sha256Text([]byte("something else"))
	expect("bad checksum", send("PUT", "b.bin", wrong, 0, 10, content), 422, "")
	expect("bad checksum discarded", send("HEAD", "b.bin", wrong, 0, 0, nil), 200, "0")

	// Sending more than promised can't grow a partial file past its length.
	expect("too long", send("PUT", "c.bin", sum, 0, 3, content), 400, "")
	expect("too long discarded", send("HEAD", "c.bin", sum, 0, 0, nil), 200, "0")
}

// --- CLI ---

// lockedBuffer lets a test read a running process's output safely.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type cliResult struct {
	stdout, stderr string
	code           int
}

func (h *harness) fleetCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command(fleetBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FLEET_URL="+h.url, "FLEET_API_KEY="+h.apiKey)
	return cmd
}

func runCommand(t *testing.T, cmd *exec.Cmd) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return cliResult{stdout.String(), stderr.String(), code}
}

func (h *harness) fleet(dir string, args ...string) cliResult {
	h.t.Helper()
	return runCommand(h.t, h.fleetCommand(dir, args...))
}

func parseJob(t *testing.T, stdout string) job {
	t.Helper()
	var j job
	if err := json.Unmarshal([]byte(stdout), &j); err != nil {
		t.Fatalf("stdout isn't JSON: %v\n%s", err, stdout)
	}
	return j
}

func TestCLIRunSavesResultAndPrintsJSON(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	dir := t.TempDir()

	result := h.fleet(dir, "run", "tts.synthesize", "--input", `{"text":"from the cli","steps":8}`, "-o", "voice.wav")
	if result.code != 0 {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	j := parseJob(t, result.stdout)
	requireSucceeded(t, j)
	if got := readFile(t, filepath.Join(dir, "voice.wav")); got != "node=mac text=from the cli" {
		t.Errorf("saved file = %q", got)
	}
	if !strings.Contains(result.stderr, "progress: running") {
		t.Errorf("no progress on stderr: %s", result.stderr)
	}
	if !strings.Contains(result.stdout, `"saved"`) {
		t.Errorf("stdout doesn't say where the file went: %s", result.stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "voice.wav.part")); err == nil {
		t.Error("a .part file was left behind")
	}
}

func TestCLISendsInputFiles(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.copy", 1)
	dir := t.TempDir()
	content := randomBytes(t, 200<<10)
	// A name the gateway wouldn't accept as is must still upload.
	source := filepath.Join(dir, "my clip (final).mp4")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result := h.fleet(dir, "run", "test.copy", "-f", "source="+source, "-o", "copy.bin", "--quiet")
	if result.code != 0 {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if result.stderr != "" {
		t.Errorf("--quiet still printed: %s", result.stderr)
	}
	if got := readFile(t, filepath.Join(dir, "copy.bin")); got != string(content) {
		t.Errorf("result has %d bytes, want %d", len(got), len(content))
	}
}

func TestCLIExitCodes(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.fail", 1)
	dir := t.TempDir()

	if result := h.fleet(dir, "run", "test.fail"); result.code != 1 {
		t.Errorf("failed job: exit %d: %s", result.code, result.stderr)
	} else if j := parseJob(t, result.stdout); j.Error == nil || j.Error.Code != "COMMAND_FAILED" {
		t.Errorf("failed job's stdout: %s", result.stdout)
	}
	if result := h.fleet(dir, "run", "tts.synthesize", "--input", `{"text":""}`); result.code != 1 {
		t.Errorf("invalid input: exit %d: %s", result.code, result.stderr)
	}
	for _, args := range [][]string{
		{},
		{"nonsense"},
		{"run"},
		{"run", "tts.synthesize", "--input", "not json"},
		{"run", "tts.synthesize", "-f", "no-equals-sign"},
		{"run", "test.copy", "-f", "source=/no/such/file"},
		{"status"},
		{"cancel", "a", "b"},
	} {
		if result := h.fleet(dir, args...); result.code != 2 {
			t.Errorf("%v: exit %d: %s", args, result.code, result.stderr)
		}
	}

	bad := h.fleetCommand(dir, "capabilities")
	bad.Env = append(bad.Env, "FLEET_API_KEY=fk_wrong")
	if result := runCommand(t, bad); result.code != 1 {
		t.Errorf("wrong key: exit %d: %s", result.code, result.stderr)
	}

	w.stop()
	eventually(t, 5*time.Second, "node to go offline", func() bool {
		_, raw := h.request("GET", "/v1/capabilities", h.apiKey, nil)
		return !strings.Contains(string(raw), `"online_nodes":1`)
	})
	result := h.fleet(dir, "run", "tts.synthesize", "--input", `{"text":"x","steps":1}`, "--queue-timeout", "300ms")
	if result.code != 75 {
		t.Errorf("no node online: exit %d, want 75: %s", result.code, result.stderr)
	}
}

func TestCLIRefusesPlainHTTPToRemoteGateway(t *testing.T) {
	h := newHarness(t)
	cmd := h.fleetCommand(t.TempDir(), "capabilities")
	cmd.Env = append(cmd.Env, "FLEET_URL=http://gateway.example.com")
	result := runCommand(t, cmd)
	if result.code != 2 || !strings.Contains(result.stderr, "https") {
		t.Errorf("exit %d: %s", result.code, result.stderr)
	}
}

func TestCLIWontOverwriteWithoutForce(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	dir := t.TempDir()
	existing := filepath.Join(dir, "out.wav")
	if err := os.WriteFile(existing, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := h.fleet(dir, "run", "tts.synthesize", "--input", `{"text":"new","steps":1}`, "-o", "out.wav")
	// Exit 75 here would tell an agent to run the whole job again.
	if result.code != 1 {
		t.Errorf("exit %d, want 1: %s", result.code, result.stderr)
	}
	if got := readFile(t, existing); got != "precious" {
		t.Errorf("file was overwritten: %q", got)
	}
	j := parseJob(t, result.stdout)
	requireSucceeded(t, j)
	if !strings.Contains(result.stderr, "fleet download "+j.ID) {
		t.Errorf("no recovery hint: %s", result.stderr)
	}

	if result := h.fleet(dir, "download", j.ID, "-o", "out.wav"); result.code != 1 {
		t.Errorf("download without --force: exit %d: %s", result.code, result.stderr)
	}
	if result := h.fleet(dir, "download", j.ID, "-o", "out.wav", "--force"); result.code != 0 {
		t.Errorf("download with --force: exit %d: %s", result.code, result.stderr)
	}
	if got := readFile(t, existing); got != "node=mac text=new" {
		t.Errorf("file = %q", got)
	}
	if w.runs() != 1 {
		t.Errorf("runs = %d", w.runs())
	}
}

func TestCLIDetachStatusWatchAndCancel(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	dir := t.TempDir()

	result := h.fleet(dir, "run", "tts.synthesize", "--input", `{"text":"x","steps":400}`, "--detach")
	if result.code != 0 {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	j := parseJob(t, result.stdout)
	h.waitStatus(j.ID, "running")
	if status := parseJob(t, h.fleet(dir, "status", j.ID).stdout).Status; status != "running" {
		t.Errorf("status = %s", status)
	}
	if status := parseJob(t, h.fleet(dir, "cancel", j.ID).stdout).Status; status != "cancelled" {
		t.Errorf("after cancel = %s", status)
	}
	// Watching a cancelled job prints its history and reports failure.
	watched := h.fleet(dir, "watch", j.ID)
	if watched.code != 1 {
		t.Errorf("watch exit %d: %s", watched.code, watched.stderr)
	}
	lines := strings.Split(strings.TrimSpace(watched.stdout), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("watch output isn't JSON lines: %v\n%s", err, watched.stdout)
	}
	if last["type"] != "end" {
		t.Errorf("last line = %v", last)
	}
	if !strings.Contains(watched.stdout, `"started"`) {
		t.Errorf("history missing from watch: %s", watched.stdout)
	}
}

func TestCLIRunSurvivesGatewayRestart(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)
	dir := t.TempDir()

	cmd := h.fleetCommand(dir, "run", "tts.synthesize", "--input", `{"text":"survivor","steps":32}`, "-o", "out.wav")
	var stdout, stderr lockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()

	eventually(t, 15*time.Second, "job to run", func() bool { return strings.Contains(stderr.String(), "event: started") })
	h.killGateway()
	time.Sleep(time.Second)
	h.startGateway()

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("%v\n%s", err, stderr.String())
		}
	case <-time.After(45 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("never finished:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "reconnecting") {
		t.Errorf("expected a reconnect: %s", stderr.String())
	}
	if got := readFile(t, filepath.Join(dir, "out.wav")); got != "node=mac text=survivor" {
		t.Errorf("saved file = %q", got)
	}
	// Each stored event must appear once, despite the reconnect.
	if count := strings.Count(stderr.String(), "event: started"); count != 1 {
		t.Errorf("started printed %d times: %s", count, stderr.String())
	}
}

func TestCLIInterruptLeavesJobRunning(t *testing.T) {
	h := newHarness(t)
	h.startWorker("mac", h.url, false)
	h.waitForCapability("tts.synthesize", 1)

	cmd := h.fleetCommand(t.TempDir(), "run", "tts.synthesize", "--input", `{"text":"x","steps":200}`)
	var stdout, stderr lockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	eventually(t, 15*time.Second, "job to run", func() bool { return strings.Contains(stderr.String(), "event: started") })
	cmd.Process.Signal(syscall.SIGINT)

	select {
	case err := <-finished:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 130 {
			t.Errorf("exit = %v\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("interrupt didn't stop the CLI")
	}
	if !strings.Contains(stderr.String(), "keeps running") {
		t.Errorf("no hint: %s", stderr.String())
	}
}

// --- Storage and interrupted uploads ---

// partialUpload sends part of a file and leaves it unfinished.
func partialUpload(t *testing.T, h *harness, token, attempt, name string, part []byte, total int) {
	t.Helper()
	req, err := http.NewRequest("PUT", h.url+"/v1/worker/attempts/"+attempt+"/artifacts/"+name, bytes.NewReader(part))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Content-SHA256", strings.Repeat("a", 64))
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Upload-Length", fmt.Sprint(total))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("partial upload: status %d", resp.StatusCode)
	}
}

func TestStorageLimitCountsInputsAndUnfinishedUploads(t *testing.T) {
	h := newHarness(t, "--max-artifact-bytes", "1000")
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)

	if status, raw := h.uploadInput(h.apiKey, "first.bin", make([]byte, 600)); status != http.StatusCreated {
		t.Fatalf("first upload: status %d: %s", status, raw)
	}
	if status, _ := h.uploadInput(h.apiKey, "second.bin", make([]byte, 600)); status != http.StatusInsufficientStorage {
		t.Errorf("past the limit: status %d", status)
	}

	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	attempt := h.getJob(j.ID).Events[0].Attempt
	partialUpload(t, h, w.token, attempt, "a.bin", make([]byte, 300), 350)
	// 600 stored and 300 unfinished leave 100 free.
	if status, _ := h.uploadInput(h.apiKey, "third.bin", make([]byte, 200)); status != http.StatusInsufficientStorage {
		t.Errorf("unfinished uploads weren't counted: status %d", status)
	}
	if status, _ := h.uploadInput(h.apiKey, "fourth.bin", make([]byte, 50)); status != http.StatusCreated {
		t.Errorf("within the limit: status %d", status)
	}
}

func TestAbandonedUploadsAreRemoved(t *testing.T) {
	h := newHarness(t, "--artifact-ttl", "1s")
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)
	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	partialUpload(t, h, w.token, h.getJob(j.ID).Events[0].Attempt, "a.bin", []byte("part"), 100)

	partials := filepath.Join(h.dir, "gateway", "partial")
	entries, err := os.ReadDir(partials)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
	eventually(t, 10*time.Second, "abandoned upload removal", func() bool {
		entries, err := os.ReadDir(partials)
		return err == nil && len(entries) == 0
	})
}

// When a network dies silently, the gateway still thinks the old upload is open.
// The worker's retry must take over right away instead of being refused.
func TestRetryTakesOverFromDeadUploadConnection(t *testing.T) {
	h := newHarness(t)
	w := h.startWorker("mac", h.url, false, scenarioCapabilities)
	h.waitForCapability("test.silent", 1)
	j := h.submit(map[string]any{"capability": "test.silent", "input": map[string]any{}})
	h.waitStatus(j.ID, "running")
	attempt := h.getJob(j.ID).Events[0].Attempt
	content := randomBytes(t, 256<<10)
	sum := sha256Text(content)
	target := h.url + "/v1/worker/attempts/" + attempt + "/artifacts/a.bin"

	// This connection sends part of the file, then goes quiet without closing.
	reader, writer := io.Pipe()
	defer writer.Close()
	stuck, err := http.NewRequest("PUT", target, reader)
	if err != nil {
		t.Fatal(err)
	}
	stuck.ContentLength = int64(len(content))
	stuck.Header.Set("Authorization", "Bearer "+w.token)
	stuck.Header.Set("X-Content-SHA256", sum)
	stuckDone := make(chan struct{})
	go func() {
		defer close(stuckDone)
		if resp, err := http.DefaultClient.Do(stuck); err == nil {
			resp.Body.Close()
		}
	}()
	first := 128 << 10
	if _, err := writer.Write(content[:first]); err != nil {
		t.Fatal(err)
	}

	position := func() int {
		req, err := http.NewRequest("HEAD", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+w.token)
		req.Header.Set("X-Content-SHA256", sum)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var offset int
		fmt.Sscan(resp.Header.Get("Upload-Offset"), &offset)
		return offset
	}
	eventually(t, 10*time.Second, "first part to arrive", func() bool { return position() > 0 })
	time.Sleep(200 * time.Millisecond) // Let the data already in flight land.
	offset := position()

	retry, err := http.NewRequest("PUT", target, bytes.NewReader(content[offset:]))
	if err != nil {
		t.Fatal(err)
	}
	retry.Header.Set("Authorization", "Bearer "+w.token)
	retry.Header.Set("X-Content-SHA256", sum)
	retry.Header.Set("Upload-Offset", fmt.Sprint(offset))
	retry.Header.Set("Upload-Length", fmt.Sprint(len(content)))
	start := time.Now()
	resp, err := http.DefaultClient.Do(retry)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("retry: status %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the retry waited %s for the dead connection", elapsed)
	}
	writer.CloseWithError(errors.New("test finished"))
	<-stuckDone
}
