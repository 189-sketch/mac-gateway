package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Only files directly in the tools folder can be changed from the page. Tools in
// worker.yaml are shown but not changed, so the page never rewrites the file that
// holds the gateway address and token setting.
var toolFileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.(yaml|yml)$`)

type uiTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Adapter     string `json:"adapter,omitempty"`
	// State is ready, not_ready, disabled or invalid.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// File is the tool's file in the tools folder, or "" for worker.yaml.
	File  string `json:"file,omitempty"`
	Error string `json:"error,omitempty"`
}

type uiRunning struct {
	Job        string `json:"job"`
	Attempt    string `json:"attempt"`
	Capability string `json:"capability"`
	Started    string `json:"started,omitempty"`
}

type uiStatus struct {
	Node           string      `json:"node"`
	Gateway        string      `json:"gateway"`
	Connected      bool        `json:"connected"`
	ConnectedSince string      `json:"connected_since,omitempty"`
	Version        string      `json:"version"`
	Platform       string      `json:"platform"`
	ConfigFile     string      `json:"config_file"`
	ToolsDir       string      `json:"tools_dir"`
	StateDir       string      `json:"state_dir"`
	MaxJobs        int         `json:"max_jobs"`
	Tools          []uiTool    `json:"tools"`
	Running        []uiRunning `json:"running"`
}

func (s *uiServer) status(w http.ResponseWriter, r *http.Request) {
	m := s.w.config()
	out := uiStatus{
		Node:       m.Node.Name,
		Gateway:    m.GatewayURL,
		Version:    s.opts.Version,
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
		ConfigFile: s.configPath,
		ToolsDir:   m.ToolsPath(),
		StateDir:   m.StateDir,
		MaxJobs:    m.Node.MaxJobs,
		Tools:      []uiTool{},
		Running:    []uiRunning{},
	}
	if since := s.w.connectedAt.Load(); since != 0 {
		out.Connected = true
		out.ConnectedSince = time.UnixMilli(since).UTC().Format(time.RFC3339)
	}

	s.w.mu.Lock()
	for _, c := range m.Capabilities {
		t := uiTool{Name: c.Name, Description: c.Description, Adapter: c.Adapter, State: "ready", File: filepath.Base(c.file)}
		if c.file == "" {
			t.File = ""
		}
		if reason, ok := s.w.notReady[c.Name]; ok {
			t.State, t.Reason, t.Detail = "not_ready", reason, s.w.notReadyDetail[c.Name]
		}
		out.Tools = append(out.Tools, t)
	}
	for _, a := range s.w.attempts {
		if a.terminalSeq != 0 {
			continue
		}
		run := uiRunning{Job: a.meta.JobID, Attempt: a.meta.AttemptID, Capability: a.meta.Capability}
		if !a.started.IsZero() {
			run.Started = a.started.UTC().Format(time.RFC3339)
		}
		out.Running = append(out.Running, run)
	}
	s.w.mu.Unlock()

	for _, name := range m.Disabled {
		t := uiTool{Name: name, State: "disabled"}
		if path := m.disabledFiles[name]; path != "" {
			t.File = filepath.Base(path)
			if c, err := readToolFile(path); err == nil {
				t.Description, t.Adapter = c.Description, c.Adapter
			}
		}
		out.Tools = append(out.Tools, t)
	}
	for _, e := range m.ToolErrors {
		t := uiTool{Name: strings.TrimSuffix(filepath.Base(e.Path), filepath.Ext(e.Path)), State: "invalid", File: filepath.Base(e.Path), Error: e.Err.Error()}
		if c, err := readToolFile(e.Path); err == nil && c.Name != "" {
			t.Name, t.Description, t.Adapter = c.Name, c.Description, c.Adapter
		}
		out.Tools = append(out.Tools, t)
	}
	sort.SliceStable(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	sort.Slice(out.Running, func(i, j int) bool { return out.Running[i].Started < out.Running[j].Started })
	writeUIJSON(w, out)
}

func (s *uiServer) logs(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lines := []LogLine{}
	if s.opts.Logs != nil {
		lines = s.opts.Logs.After(after)
	}
	writeUIJSON(w, map[string]any{"lines": lines})
}

func (s *uiServer) templates(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	out := []view{}
	for _, t := range Templates(runtime.GOOS) {
		out = append(out, view{Name: t.Name, Title: t.Title, Description: t.Description, Body: t.Body})
	}
	writeUIJSON(w, out)
}

// --- Tool files ---

// toolsRoot opens the tools folder so a name can't reach outside it, even
// through a link.
func (s *uiServer) toolsRoot(create bool) (*os.Root, error) {
	dir := s.w.config().ToolsPath()
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return os.OpenRoot(dir)
}

func fileVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12])
}

func toolFileName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("file")
	if !toolFileRE.MatchString(name) {
		writeUIError(w, http.StatusBadRequest, "a tool file is named like tts.say.yaml")
		return "", false
	}
	return name, true
}

type uiFile struct {
	File    string         `json:"file"`
	YAML    string         `json:"yaml"`
	Version string         `json:"version"`
	Tool    map[string]any `json:"tool,omitempty"`
	// Problem is why the tool won't load, or for a disabled tool, why it wouldn't
	// if it were turned on.
	Problem string `json:"problem,omitempty"`
}

func (s *uiServer) readFile(w http.ResponseWriter, r *http.Request) {
	name, ok := toolFileName(w, r)
	if !ok {
		return
	}
	root, err := s.toolsRoot(false)
	if err != nil {
		writeUIError(w, http.StatusNotFound, "the tools folder doesn't exist")
		return
	}
	defer root.Close()
	data, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		writeUIError(w, http.StatusNotFound, name+" doesn't exist")
		return
	}
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeUIJSON(w, s.describeFile(name, data))
}

func (s *uiServer) describeFile(name string, data []byte) uiFile {
	out := uiFile{File: name, YAML: string(data), Version: fileVersion(data)}
	if tool, err := yamlToTool(data); err == nil {
		out.Tool = tool
	}
	path := filepath.Join(s.w.config().ToolsPath(), name)
	if _, parseErr, problem := s.w.config().checkToolText(data, path); parseErr != nil {
		out.Problem = parseErr.Error()
	} else if problem != nil {
		out.Problem = problem.Error()
	}
	return out
}

// checkToolText reads a tool file's text the way the worker will. parseErr means
// it can't be read at all. problem is why it won't load, or for a disabled tool,
// why it wouldn't once turned on. path is where it is or will be saved.
func (m *Manifest) checkToolText(data []byte, path string) (c *Capability, parseErr, problem error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var parsed Capability
	if err := decoder.Decode(&parsed); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty"), nil
		}
		return nil, fmt.Errorf("parse: %w", err), nil
	}
	if other := m.byName[parsed.Name]; other != nil && other.file != path {
		return &parsed, nil, nameTakenError{parsed.Name, other.file}
	}
	if file, ok := m.disabledFiles[parsed.Name]; ok && file != path {
		return &parsed, nil, nameTakenError{parsed.Name, file}
	}
	checked := parsed
	if err := checked.validate(m.ToolsPath(), m.Node.Resources); err != nil {
		return &parsed, nil, err
	}
	checked.file = path
	return &checked, nil, nil
}

// nameTakenError means another file already has a tool by that name. Unlike
// other problems, it stops even a tool that's off from being saved, since the
// two files would then each stop the other being turned on.
type nameTakenError struct {
	name, file string
}

func (e nameTakenError) Error() string {
	where := "worker.yaml"
	if e.file != "" {
		where = filepath.Base(e.file)
	}
	return fmt.Sprintf("a tool named %q is already in %s", e.name, where)
}

// yamlToTool turns a tool file into the plain object the page's form edits.
func yamlToTool(data []byte) (map[string]any, error) {
	var tool map[string]any
	if err := yaml.Unmarshal(data, &tool); err != nil {
		return nil, err
	}
	if tool == nil {
		return nil, errors.New("the file is empty")
	}
	// Round-trip through JSON so every value is something JSON can hold.
	encoded, err := json.Marshal(tool)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(encoded, &out)
	return out, err
}

// toolToYAML writes the form's object as a tool file. Reading it into a
// Capability first refuses fields a tool doesn't have and puts the rest in the
// usual order.
func toolToYAML(tool map[string]any) ([]byte, error) {
	raw, err := yaml.Marshal(tool)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var c Capability
	if err := decoder.Decode(&c); err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := node.Encode(&c); err != nil {
		return nil, err
	}
	tidyYAML(&node, "")
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// schemaKeyOrder puts a schema's keys in the order people write them, rather
// than alphabetical.
var schemaKeyOrder = map[string]int{"type": 1, "title": 2, "description": 3, "required": 4, "additionalProperties": 5, "items": 6, "properties": 7}

// maxFlowWidth is how long a list or map of plain values can be and still go
// on one line, as in command: [/usr/bin/say, -o, out.aiff]. Anything holding
// another list or map is written out in full.
const maxFlowWidth = 72

// tidyYAML lays a saved tool out the way the templates are written.
func tidyYAML(n *yaml.Node, key string) {
	if n.Kind == yaml.MappingNode && (key == "input_schema" || key == "properties" || key == "items") {
		pairs := make([][2]*yaml.Node, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			pairs = append(pairs, [2]*yaml.Node{n.Content[i], n.Content[i+1]})
		}
		if key != "properties" {
			sort.SliceStable(pairs, func(i, j int) bool {
				a, b := schemaKeyOrder[pairs[i][0].Value], schemaKeyOrder[pairs[j][0].Value]
				if a == 0 {
					a = 100
				}
				if b == 0 {
					b = 100
				}
				return a < b
			})
		}
		n.Content = n.Content[:0]
		for _, pair := range pairs {
			n.Content = append(n.Content, pair[0], pair[1])
		}
	}
	width := 0
	plain := n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode
	for i, child := range n.Content {
		childKey := key
		if n.Kind == yaml.MappingNode && i%2 == 1 {
			childKey = n.Content[i-1].Value
			if key == "properties" {
				childKey = "items" // A property is a schema too.
			}
		}
		if child.Kind != yaml.ScalarNode {
			tidyYAML(child, childKey)
			plain = false
		}
		if child.Kind == yaml.ScalarNode && strings.ContainsAny(child.Value, "\n") {
			plain = false
		}
		width += len(child.Value) + 4
	}
	if plain && len(n.Content) > 0 && width <= maxFlowWidth {
		n.Style |= yaml.FlowStyle
	}
}

type saveRequest struct {
	YAML *string        `json:"yaml"`
	Tool map[string]any `json:"tool"`
	// Version is the version the page read, or "" for a new file. If the file
	// has changed since, nothing is saved.
	Version string `json:"version"`
}

func (s *uiServer) saveFile(w http.ResponseWriter, r *http.Request) {
	name, ok := toolFileName(w, r)
	if !ok {
		return
	}
	var req saveRequest
	if !readUIJSON(w, r, &req) {
		return
	}
	var data []byte
	switch {
	case req.YAML != nil && req.Tool == nil:
		data = []byte(*req.YAML)
	case req.Tool != nil && req.YAML == nil:
		var err error
		if data, err = toolToYAML(req.Tool); err != nil {
			writeUIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	default:
		writeUIError(w, http.StatusBadRequest, "send yaml or tool")
		return
	}
	m := s.w.config()
	path := filepath.Join(m.ToolsPath(), name)
	c, parseErr, problem := m.checkToolText(data, path)
	if parseErr != nil {
		writeUIError(w, http.StatusUnprocessableEntity, parseErr.Error())
		return
	}
	// A tool that's on must work. One that's off can be saved half-made.
	var taken nameTakenError
	if errors.As(problem, &taken) {
		writeUIError(w, http.StatusUnprocessableEntity, problem.Error()+"; choose another name")
		return
	}
	if problem != nil && c.isEnabled() {
		writeUIError(w, http.StatusUnprocessableEntity, problem.Error()+" (to save it unfinished, turn it off)")
		return
	}

	root, err := s.toolsRoot(true)
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer root.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := root.ReadFile(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if req.Version != "" {
			writeUIError(w, http.StatusConflict, name+" was deleted since you opened it")
			return
		}
	case err != nil:
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	case req.Version != fileVersion(current):
		writeUIError(w, http.StatusConflict, name+" was changed by something else since you opened it; reload it to see the change")
		return
	}
	if err := replaceFile(root, name, data); err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.w.log.Info("tool file saved from the configuration page", "file", name)
	s.w.RequestReload()
	writeUIJSON(w, s.describeFile(name, data))
}

// replaceFile writes a file in one step. The temporary file starts with a dot,
// which the worker skips when it reads the tools folder.
func replaceFile(root *os.Root, name string, data []byte) error {
	tmp := "." + name + ".saving"
	if err := root.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := root.Chmod(tmp, 0o600); err != nil {
		root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}

func (s *uiServer) deleteFile(w http.ResponseWriter, r *http.Request) {
	name, ok := toolFileName(w, r)
	if !ok {
		return
	}
	root, err := s.toolsRoot(false)
	if err != nil {
		writeUIError(w, http.StatusNotFound, "the tools folder doesn't exist")
		return
	}
	defer root.Close()
	s.mu.Lock()
	err = root.Remove(name)
	s.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		writeUIError(w, http.StatusNotFound, name+" doesn't exist")
		return
	}
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.w.log.Info("tool file deleted from the configuration page", "file", name)
	s.w.RequestReload()
	writeUIJSON(w, map[string]bool{"deleted": true})
}

func (s *uiServer) newFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Template string `json:"template"`
		Name     string `json:"name"`
	}
	if !readUIJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	path, err := NewToolFile(s.w.config(), req.Template, req.Name, runtime.GOOS)
	s.mu.Unlock()
	if err != nil {
		writeUIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.w.log.Info("tool file created from the configuration page", "file", filepath.Base(path), "template", req.Template)
	s.w.RequestReload()
	data, err := os.ReadFile(path)
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeUIJSON(w, s.describeFile(filepath.Base(path), data))
}

// setEnabled turns a tool on or off by changing just that line, so the rest of
// the file, comments included, stays as it was.
func (s *uiServer) setEnabled(w http.ResponseWriter, r *http.Request) {
	name, ok := toolFileName(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !readUIJSON(w, r, &req) {
		return
	}
	root, err := s.toolsRoot(false)
	if err != nil {
		writeUIError(w, http.StatusNotFound, "the tools folder doesn't exist")
		return
	}
	defer root.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		writeUIError(w, http.StatusNotFound, name+" doesn't exist")
		return
	}
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	data, err := setEnabledText(current, req.Enabled)
	if err != nil {
		writeUIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.Enabled {
		m := s.w.config()
		if _, parseErr, problem := m.checkToolText(data, filepath.Join(m.ToolsPath(), name)); parseErr != nil || problem != nil {
			writeUIError(w, http.StatusUnprocessableEntity, "it can't be turned on yet: "+errors.Join(parseErr, problem).Error())
			return
		}
	}
	if err := replaceFile(root, name, data); err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.w.log.Info("tool turned on or off from the configuration page", "file", name, "enabled", req.Enabled)
	s.w.RequestReload()
	writeUIJSON(w, s.describeFile(name, data))
}

// setEnabledText sets a tool file's top-level enabled value, adding the line if
// there isn't one.
func setEnabledText(data []byte, enabled bool) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode || doc.Content[0].Style&yaml.FlowStyle != 0 {
		return nil, errors.New("this file isn't laid out as usual; change enabled in its YAML instead")
	}
	top := doc.Content[0]
	if len(top.Content) == 0 {
		return nil, errors.New("the file has no tool in it")
	}
	value := strconv.FormatBool(enabled)
	lines := strings.SplitAfter(string(data), "\n")
	var changed string
	found := false
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, node := top.Content[i], top.Content[i+1]
		if key.Value != "enabled" {
			continue
		}
		found = true
		if node.Kind != yaml.ScalarNode || node.Line < 1 || node.Line > len(lines) {
			return nil, errors.New("enabled must be true or false")
		}
		line := []rune(lines[node.Line-1])
		start := node.Column - 1
		end := start + len([]rune(node.Value))
		if start < 0 || end > len(line) || string(line[start:end]) != node.Value {
			return nil, errors.New("this file isn't laid out as usual; change enabled in its YAML instead")
		}
		lines[node.Line-1] = string(line[:start]) + value + string(line[end:])
		changed = strings.Join(lines, "")
		break
	}
	if !found {
		// Put it on the line before the first setting.
		at := top.Content[0].Line - 1
		if at < 0 || at > len(lines) || top.Content[0].Column != 1 {
			return nil, errors.New("this file isn't laid out as usual; add enabled to its YAML instead")
		}
		lines = append(lines[:at], append([]string{"enabled: " + value + "\n"}, lines[at:]...)...)
		changed = strings.Join(lines, "")
	}
	// Make sure the edit did what it should, and nothing else.
	var c Capability
	decoder := yaml.NewDecoder(strings.NewReader(changed))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil || c.isEnabled() != enabled {
		return nil, errors.New("this file isn't laid out as usual; change enabled in its YAML instead")
	}
	return []byte(changed), nil
}

// readBuiltin shows a tool from worker.yaml, which the page can't change.
func (s *uiServer) readBuiltin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var file struct {
		Capabilities []yaml.Node `yaml:"capabilities"`
		Tools        []yaml.Node `yaml:"tools"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, node := range append(file.Capabilities, file.Tools...) {
		var named struct {
			Name string `yaml:"name"`
		}
		if node.Decode(&named) != nil || named.Name != name {
			continue
		}
		var out bytes.Buffer
		encoder := yaml.NewEncoder(&out)
		encoder.SetIndent(2)
		if err := encoder.Encode(&node); err != nil {
			writeUIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		encoder.Close()
		tool, _ := yamlToTool(out.Bytes())
		writeUIJSON(w, uiFile{YAML: out.String(), Tool: tool, Version: fileVersion(out.Bytes())})
		return
	}
	writeUIError(w, http.StatusNotFound, "worker.yaml has no tool named "+name)
}

// convert switches the editor between the form and YAML.
func (s *uiServer) convert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		YAML *string        `json:"yaml"`
		Tool map[string]any `json:"tool"`
	}
	if !readUIJSON(w, r, &req) {
		return
	}
	switch {
	case req.Tool != nil:
		data, err := toolToYAML(req.Tool)
		if err != nil {
			writeUIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeUIJSON(w, map[string]string{"yaml": string(data)})
	case req.YAML != nil:
		tool, err := yamlToTool([]byte(*req.YAML))
		if err != nil {
			writeUIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeUIJSON(w, map[string]any{"tool": tool})
	default:
		writeUIError(w, http.StatusBadRequest, "send yaml or tool")
	}
}

// check says whether a tool would load, without saving it.
func (s *uiServer) check(w http.ResponseWriter, r *http.Request) {
	var req struct {
		YAML string `json:"yaml"`
		File string `json:"file"`
	}
	if !readUIJSON(w, r, &req) {
		return
	}
	m := s.w.config()
	path := ""
	if req.File != "" {
		if !toolFileRE.MatchString(req.File) {
			writeUIError(w, http.StatusBadRequest, "a tool file is named like tts.say.yaml")
			return
		}
		path = filepath.Join(m.ToolsPath(), req.File)
	}
	_, parseErr, problem := m.checkToolText([]byte(req.YAML), path)
	out := map[string]string{}
	if err := errors.Join(parseErr, problem); err != nil {
		out["problem"] = err.Error()
	}
	writeUIJSON(w, out)
}
