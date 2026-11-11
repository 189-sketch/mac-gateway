package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mactunnel/internal/protocol"
)

const (
	// triesDir, in the state folder, holds tries from the configuration page.
	// They're runs of a tool on this machine alone; the gateway never sees them.
	triesDir        = "tries"
	maxRunningTries = 2
	keepTries       = 10
	jobFolderLabel  = "<job folder>"
)

// toolRequest names a tool by its text, which may not be saved yet, so a change
// can be tried before it's saved.
type toolRequest struct {
	YAML  string          `json:"yaml"`
	File  string          `json:"file"`
	Input json.RawMessage `json:"input"`
}

// readyTool checks a tool as if it were turned on, and its input.
func (s *uiServer) readyTool(req toolRequest) (*Capability, map[string]any, error) {
	m := s.w.config()
	path := ""
	if req.File != "" {
		if !toolFileRE.MatchString(req.File) {
			return nil, nil, errors.New("a tool file is named like tts.say.yaml")
		}
		path = filepath.Join(m.ToolsPath(), req.File)
	}
	c, parseErr, problem := m.checkToolText([]byte(req.YAML), path)
	if err := errors.Join(parseErr, problem); err != nil {
		return nil, nil, err
	}
	raw := []byte(req.Input)
	if len(strings.TrimSpace(string(raw))) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	input, err := c.readInput(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("input: %w", err)
	}
	return c, input, nil
}

// callView is what a tool would be run with.
type callView struct {
	Program string   `json:"program,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Stdin   string   `json:"stdin,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	// Env is as declared, because values filled from this machine's environment
	// may be secrets.
	Env    map[string]string `json:"env,omitempty"`
	Method string            `json:"method,omitempty"`
	URL    string            `json:"url,omitempty"`
	Body   string            `json:"body,omitempty"`
}

func describeCall(ctx context.Context, c *Capability, vars templateVars) (callView, error) {
	if c.isService() {
		if c.Request == nil {
			return callView{}, errors.New("the tool has no request")
		}
		view := callView{Method: c.Request.Method, URL: c.Request.URL}
		if c.Request.bodyField != "" {
			view.Body = "<input file " + c.Request.bodyField + ">"
			target, err := renderURL(c.Request.URL, vars)
			view.URL = target
			return view, err
		}
		req, err := buildRequest(ctx, c.Request, vars)
		if err != nil {
			return view, err
		}
		view.Method, view.URL = req.Method, req.URL.String()
		if req.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(req.Body, 64<<10))
			req.Body.Close()
			view.Body = string(body)
		}
		return view, nil
	}
	args, err := renderCommand(c.Command[1:], vars)
	if err != nil {
		return callView{}, err
	}
	extra, err := renderArgs(c.Args, vars)
	if err != nil {
		return callView{}, err
	}
	stdin, err := render(c.Stdin, vars)
	if err != nil {
		return callView{}, err
	}
	return callView{
		Program: c.executable,
		Argv:    append(append([]string{c.Command[0]}, args...), extra...),
		Stdin:   stdin,
		Cwd:     c.workingDir(vars.workdir),
		Env:     c.Env,
	}, nil
}

func (s *uiServer) preview(w http.ResponseWriter, r *http.Request) {
	var req toolRequest
	if !readUIJSON(w, r, &req) {
		return
	}
	c, input, err := s.readyTool(req)
	if err != nil {
		writeUIJSON(w, map[string]string{"problem": err.Error()})
		return
	}
	files := map[string]string{}
	for _, name := range c.InputFiles {
		files[name] = "<input file " + name + ">"
	}
	view, err := describeCall(r.Context(), c, templateVars{workdir: jobFolderLabel, input: input, files: files})
	if err != nil {
		writeUIJSON(w, map[string]string{"problem": err.Error()})
		return
	}
	writeUIJSON(w, map[string]any{"call": view})
}

// --- Trying a tool ---

type tryRunner struct {
	w   *Worker
	dir string

	mu    sync.Mutex
	runs  map[string]*tryRun
	order []string
}

type tryRun struct {
	id      string
	tool    string
	dir     string
	started time.Time
	call    callView
	a       *attempt

	mu       sync.Mutex
	tail     *logTail
	done     bool
	finished time.Time
	result   json.RawMessage
	exitCode *int
	failure  *protocol.Failure
	files    []tryFile
}

type tryFile struct {
	name string
	size int64
	dir  string
	rel  string
}

func newTryRunner(w *Worker, dir string) (*tryRunner, error) {
	// Tries don't outlive the worker, so anything here is left from a previous run.
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &tryRunner{w: w, dir: dir, runs: map[string]*tryRun{}}, nil
}

func (t *tryRunner) stopAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, run := range t.runs {
		run.a.cancel()
	}
}

func (s *uiServer) startTry(w http.ResponseWriter, r *http.Request) {
	var req toolRequest
	if !readUIJSON(w, r, &req) {
		return
	}
	c, input, err := s.readyTool(req)
	if err != nil {
		writeUIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if len(c.InputFiles) > 0 {
		writeUIError(w, http.StatusUnprocessableEntity, "trying a tool that takes files isn't supported here yet; use the gateway's test instead")
		return
	}
	run, err := s.tries.start(c, input)
	if err != nil {
		writeUIError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeUIJSON(w, map[string]string{"id": run.id})
}

func (t *tryRunner) start(c *Capability, input map[string]any) (*tryRun, error) {
	id, err := randomText(9)
	if err != nil {
		return nil, err
	}
	id = strings.NewReplacer("-", "a", "_", "b").Replace(id)
	t.mu.Lock()
	running := 0
	for _, run := range t.runs {
		run.mu.Lock()
		if !run.done {
			running++
		}
		run.mu.Unlock()
	}
	if running >= maxRunningTries {
		t.mu.Unlock()
		return nil, fmt.Errorf("%d tries are already running; wait for one to finish", running)
	}
	t.pruneLocked()
	dir := filepath.Join(t.dir, id)
	work := filepath.Join(dir, workDir)
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &tryRun{id: id, tool: c.Name, dir: dir, started: time.Now()}
	run.a = &attempt{
		meta:    attemptMeta{JobID: "try", AttemptID: "try-" + id, Capability: c.Name},
		dir:     dir,
		cancel:  cancel,
		started: run.started,
		done:    make(chan struct{}),
		watch: func(tail *logTail) {
			run.mu.Lock()
			run.tail = tail
			run.mu.Unlock()
		},
	}
	vars := templateVars{workdir: work, input: input}
	run.call, _ = describeCall(ctx, c, vars)
	t.runs[id] = run
	t.order = append(t.order, id)
	t.mu.Unlock()

	t.w.log.Info("trying a tool from the configuration page", "capability", c.Name, "try", id)
	go func() {
		defer close(run.a.done)
		defer cancel()
		result, exitCode, failure := t.w.invoke(ctx, run.a, c, vars, work)
		var files []tryFile
		if failure == nil {
			files, failure = collectTryOutputs(c, work, dir)
		}
		run.mu.Lock()
		run.done, run.finished = true, time.Now()
		run.result, run.exitCode, run.failure, run.files = result, exitCode, failure, files
		run.mu.Unlock()
		outcome := "succeeded"
		if failure != nil {
			outcome = "failed: " + failure.Code
		}
		t.w.log.Info("try finished", "capability", c.Name, "try", id, "outcome", outcome, "duration", time.Since(run.started).Round(time.Millisecond).String())
	}()
	return run, nil
}

// pruneLocked removes the oldest finished tries beyond those kept.
func (t *tryRunner) pruneLocked() {
	for len(t.order) >= keepTries {
		removed := false
		for i, id := range t.order {
			run := t.runs[id]
			run.mu.Lock()
			done := run.done
			run.mu.Unlock()
			if done {
				delete(t.runs, id)
				t.order = append(t.order[:i], t.order[i+1:]...)
				os.RemoveAll(run.dir)
				removed = true
				break
			}
		}
		if !removed {
			return
		}
	}
}

// collectTryOutputs finds each declared output, as a job would before sending it.
func collectTryOutputs(c *Capability, work, dir string) ([]tryFile, *protocol.Failure) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeOutputMissing, Message: err.Error()}
	}
	defer root.Close()
	names := make([]string, 0, len(c.Outputs))
	for name := range c.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	var files []tryFile
	for _, name := range names {
		out := c.Outputs[name]
		if _, err := root.Lstat(out.Path); errors.Is(err, fs.ErrNotExist) && out.Optional {
			continue
		}
		from, fromDir, rel := root, work, out.Path
		if out.Archive == archiveZip {
			fromDir = filepath.Join(dir, archivesDir)
			if from, rel, err = archiveOutput(root, out.Path, fromDir, name); err != nil {
				return files, &protocol.Failure{Code: protocol.CodeOutputMissing, Message: fmt.Sprintf("output %q: %v", name, err)}
			}
			defer from.Close()
		}
		ref, err := inspectOutput(from, rel, out.artifactName(name))
		if err != nil {
			return files, &protocol.Failure{Code: protocol.CodeOutputMissing, Message: fmt.Sprintf("output %q: %v", name, err)}
		}
		files = append(files, tryFile{name: ref.Name, size: ref.Size, dir: fromDir, rel: rel})
	}
	return files, nil
}

func (s *uiServer) findTry(w http.ResponseWriter, r *http.Request) *tryRun {
	s.tries.mu.Lock()
	run := s.tries.runs[r.PathValue("id")]
	s.tries.mu.Unlock()
	if run == nil {
		writeUIError(w, http.StatusNotFound, "that try is gone; tries are kept until the worker restarts, ten at most")
	}
	return run
}

type tryView struct {
	ID       string             `json:"id"`
	Tool     string             `json:"tool"`
	State    string             `json:"state"`
	Started  string             `json:"started"`
	Seconds  float64            `json:"seconds"`
	Call     callView           `json:"call"`
	Output   string             `json:"output"`
	Progress *protocol.Progress `json:"progress,omitempty"`
	Result   json.RawMessage    `json:"result,omitempty"`
	ExitCode *int               `json:"exit_code,omitempty"`
	Failure  *protocol.Failure  `json:"failure,omitempty"`
	Files    []tryFileView      `json:"files"`
}

type tryFileView struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (s *uiServer) tryStatus(w http.ResponseWriter, r *http.Request) {
	run := s.findTry(w, r)
	if run == nil {
		return
	}
	// The latest progress the tool reported.
	var progress *protocol.Progress
	s.w.mu.Lock()
	for i := len(run.a.events) - 1; i >= 0; i-- {
		if ev := run.a.events[i]; ev.Type == protocol.EventProgress {
			var p protocol.Progress
			if json.Unmarshal(ev.Payload, &p) == nil {
				progress = &p
			}
			break
		}
	}
	s.w.mu.Unlock()

	run.mu.Lock()
	defer run.mu.Unlock()
	view := tryView{
		ID: run.id, Tool: run.tool, State: "running", Started: run.started.UTC().Format(time.RFC3339),
		Call: run.call, Progress: progress, Result: run.result, ExitCode: run.exitCode, Files: []tryFileView{},
	}
	end := time.Now()
	if run.done {
		end = run.finished
		view.State = "succeeded"
		if run.failure != nil {
			view.State = "failed"
			failure := *run.failure
			failure.Output = "" // Shown once, below.
			view.Failure = &failure
			if view.ExitCode == nil {
				view.ExitCode = run.failure.ExitCode
			}
		}
	}
	view.Seconds = end.Sub(run.started).Seconds()
	if run.tail != nil {
		view.Output = run.tail.output()
	}
	for _, f := range run.files {
		view.Files = append(view.Files, tryFileView{Name: f.name, Size: f.size})
	}
	writeUIJSON(w, view)
}

func (s *uiServer) cancelTry(w http.ResponseWriter, r *http.Request) {
	run := s.findTry(w, r)
	if run == nil {
		return
	}
	run.a.cancel()
	writeUIJSON(w, map[string]bool{"cancelled": true})
}

// tryFile sends a file a try made. It's always sent as a download, so a page a
// tool wrote can't run as part of this one.
func (s *uiServer) tryFile(w http.ResponseWriter, r *http.Request) {
	run := s.findTry(w, r)
	if run == nil {
		return
	}
	name := r.PathValue("name")
	run.mu.Lock()
	var found *tryFile
	for i := range run.files {
		if run.files[i].name == name {
			found = &run.files[i]
		}
	}
	run.mu.Unlock()
	if found == nil {
		writeUIError(w, http.StatusNotFound, "the try made no file called "+name)
		return
	}
	root, err := os.OpenRoot(found.dir)
	if err != nil {
		writeUIError(w, http.StatusNotFound, err.Error())
		return
	}
	defer root.Close()
	file, err := root.Open(found.rel)
	if err != nil {
		writeUIError(w, http.StatusNotFound, err.Error())
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeUIError(w, http.StatusNotFound, "the file is gone")
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, _ = io.Copy(w, file)
}

// --- Finding programs ---

type browseEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir,omitempty"`
	Exec bool   `json:"exec,omitempty"`
}

const maxBrowseEntries = 2000

// browse lists a folder, to help pick a program or working folder.
func (s *uiServer) browse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	if !filepath.IsAbs(path) {
		writeUIError(w, http.StatusBadRequest, "give a full path")
		return
	}
	path = filepath.Clean(path)
	hidden := r.URL.Query().Get("hidden") == "1"
	entries, err := os.ReadDir(path)
	if err != nil {
		writeUIError(w, http.StatusNotFound, err.Error())
		return
	}
	out := []browseEntry{}
	for _, entry := range entries {
		if !hidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if target, err := os.Stat(filepath.Join(path, entry.Name())); err == nil {
				info = target
			}
		}
		out = append(out, browseEntry{Name: entry.Name(), Dir: info.IsDir(), Exec: !info.IsDir() && looksRunnable(entry.Name(), info.Mode())})
		if len(out) >= maxBrowseEntries {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	writeUIJSON(w, map[string]any{"path": path, "parent": parent, "separator": string(filepath.Separator), "entries": out})
}

func looksRunnable(name string, mode fs.FileMode) bool {
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".exe", ".bat", ".cmd", ".com", ".ps1":
			return true
		}
		return false
	}
	return mode&0o111 != 0
}

// detectPrograms are what tools usually run. The page shows where each is.
var detectPrograms = []string{
	"python3", "python", "uv", "node", "npx", "claude", "curl", "ffmpeg", "git", "docker",
	"xcodebuild", "xcrun", "swift", "pwsh", "powershell", "bash", "zsh",
	"conda", "nvidia-smi", "ollama",
}

type detectedProgram struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// OnPath is whether the worker finds it by name. If not, a tool must give the
	// full path, which is safer anyway.
	OnPath bool `json:"on_path"`
}

type simulator struct {
	Name    string `json:"name"`
	UDID    string `json:"udid"`
	Runtime string `json:"runtime"`
	State   string `json:"state"`
}

func (s *uiServer) detect(w http.ResponseWriter, r *http.Request) {
	found := []detectedProgram{}
	home, _ := os.UserHomeDir()
	var extra []string
	if runtime.GOOS != "windows" {
		extra = []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}
		if home != "" {
			extra = append(extra, filepath.Join(home, ".local", "bin"), filepath.Join(home, ".claude", "local"), filepath.Join(home, "bin"), filepath.Join(home, ".npm-global", "bin"))
			// Where conda is usually installed, which a service's PATH won't include.
			for _, dir := range []string{"miniconda3", "miniforge3", "anaconda3"} {
				extra = append(extra, filepath.Join(home, dir, "bin"))
			}
		}
		extra = append(extra, "/opt/conda/bin")
	}
	for _, name := range detectPrograms {
		if path, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
			found = append(found, detectedProgram{Name: name, Path: path, OnPath: true})
			continue
		}
		for _, dir := range extra {
			path := filepath.Join(dir, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				found = append(found, detectedProgram{Name: name, Path: path})
				break
			}
		}
	}
	out := map[string]any{"programs": found}
	// Listing simulators can take a few seconds, so it's done only when asked.
	if runtime.GOOS == "darwin" && r.URL.Query().Get("simulators") == "1" {
		sims, err := listSimulators(r.Context())
		if err != nil {
			out["simulators_error"] = err.Error()
		}
		out["simulators"] = sims
	}
	writeUIJSON(w, out)
}

func listSimulators(parent context.Context) ([]simulator, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "xcrun", "simctl", "list", "devices", "available", "-j").Output()
	if err != nil {
		return []simulator{}, fmt.Errorf("xcrun simctl: %w", err)
	}
	var list struct {
		Devices map[string][]struct {
			Name  string `json:"name"`
			UDID  string `json:"udid"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return []simulator{}, err
	}
	out := []simulator{}
	for runtimeID, devices := range list.Devices {
		// com.apple.CoreSimulator.SimRuntime.iOS-26-0 reads as iOS 26.0.
		name := runtimeID[strings.LastIndex(runtimeID, ".")+1:]
		if system, version, ok := strings.Cut(name, "-"); ok {
			name = system + " " + strings.ReplaceAll(version, "-", ".")
		}
		for _, d := range devices {
			out = append(out, simulator{Name: d.Name, UDID: d.UDID, Runtime: name, State: d.State})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Runtime != out[j].Runtime {
			return out[i].Runtime > out[j].Runtime
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
