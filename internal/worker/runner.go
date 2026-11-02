package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mactunnel/internal/protocol"
	"mactunnel/internal/schema"
)

const (
	termGrace        = 5 * time.Second
	progressInterval = time.Second
	logTailLines     = 20
	maxLineBytes     = 32 << 10
	uploadStall      = 60 * time.Second
	maxUploadBackoff = time.Minute
)

func itoa(n int) string { return strconv.Itoa(n) }

// run executes one attempt from validation to its final event.
func (w *Worker) run(ctx context.Context, a *attempt, capabilityName string, rawInput []byte) {
	defer close(a.done)

	capability := w.manifest.byName[capabilityName]
	if capability == nil {
		w.fail(a, protocol.CodeUnknownCapability, "this node does not offer "+capabilityName)
		return
	}
	// The gateway checked this too, but this machine trusts only its own check.
	if err := schema.Validate(capability.compiled, rawInput); err != nil {
		w.fail(a, protocol.CodeInvalidInput, err.Error())
		return
	}
	var input map[string]any
	decoder := json.NewDecoder(bytes.NewReader(rawInput))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		w.fail(a, protocol.CodeInvalidInput, "input must be a JSON object")
		return
	}

	work := filepath.Join(a.dir, workDir)
	args, err := renderCommand(capability.Command[1:], work, input)
	if err != nil {
		w.fail(a, protocol.CodeInvalidInput, err.Error())
		return
	}
	stdin, err := render(capability.Stdin, work, input)
	if err != nil {
		w.fail(a, protocol.CodeInvalidInput, err.Error())
		return
	}

	outcome := w.execute(ctx, a, capability, args, stdin, work)
	if outcome != nil {
		if ctx.Err() == nil || outcome.Code == protocol.CodeTimedOut {
			w.fail(a, outcome.Code, outcome.Message)
		}
		return
	}
	w.deliverOutputs(ctx, a, capability, work)
}

func (w *Worker) execute(ctx context.Context, a *attempt, c *Capability, args []string, stdin, work string) *protocol.Failure {
	cmd := exec.Command(c.executable, args...)
	cmd.Args = append([]string{c.Command[0]}, args...)
	cmd.Dir = work
	cmd.Env = childEnv(c, work)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.WaitDelay = termGrace
	setProcessGroup(cmd)

	// One pipe for both streams keeps their lines in order. With *os.File as the
	// destination, Wait returns when the process exits even if a stray child
	// still holds the pipe open.
	reader, writer, err := os.Pipe()
	if err != nil {
		return &protocol.Failure{Code: protocol.CodeStartFailed, Message: err.Error()}
	}
	defer reader.Close()
	cmd.Stdout, cmd.Stderr = writer, writer

	if err := cmd.Start(); err != nil {
		writer.Close()
		return &protocol.Failure{Code: protocol.CodeStartFailed, Message: err.Error()}
	}
	writer.Close()
	pid := cmd.Process.Pid
	w.recordProcess(a, pid)
	w.emit(a, protocol.EventStarted, nil)

	tail := &logTail{}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		w.readOutput(a, c, reader, tail)
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	timeout := time.NewTimer(c.timeout)
	defer timeout.Stop()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-waited:
	case <-timeout.C:
		timedOut = true
		waitErr = stopGroup(pid, waited)
	case <-ctx.Done():
		waitErr = stopGroup(pid, waited)
	}

	// Clear anything the command left behind. This also ends the pipe reader.
	killGroup(pid)
	w.forgetProcess(a)
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
	}

	switch {
	case timedOut:
		return &protocol.Failure{Code: protocol.CodeTimedOut, Message: fmt.Sprintf("the command ran longer than its %s timeout", c.timeout)}
	case ctx.Err() != nil:
		return &protocol.Failure{Code: protocol.CodeCommandFailed, Message: "aborted"}
	case waitErr != nil:
		return &protocol.Failure{Code: protocol.CodeCommandFailed, Message: fmt.Sprintf("%v\n%s", waitErr, tail.String())}
	}
	return nil
}

// stopGroup asks the process group to exit, then forces it after a grace period.
func stopGroup(pid int, waited <-chan error) error {
	terminateGroup(pid)
	grace := time.NewTimer(termGrace)
	defer grace.Stop()
	select {
	case err := <-waited:
		return err
	case <-grace.C:
		killGroup(pid)
		return <-waited
	}
}

// childEnv deliberately starts empty, so the worker's own environment, which may
// hold the node token, isn't inherited by commands.
func childEnv(c *Capability, work string) []string {
	env := map[string]string{"TMPDIR": work}
	for _, key := range []string{"PATH", "HOME", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	for key, value := range c.Env {
		env[key] = value
	}
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

type logTail struct {
	lines []string
}

func (t *logTail) add(line string) {
	if len(line) > 500 {
		line = line[:500]
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > logTailLines {
		t.lines = t.lines[1:]
	}
}

func (t *logTail) String() string { return strings.Join(t.lines, "\n") }

// readOutput drains command output, which must never stop or the command would
// block on a full pipe.
func (w *Worker) readOutput(a *attempt, c *Capability, r io.Reader, tail *logTail) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 2*maxLineBytes), 2*maxLineBytes)
	scanner.Split(splitOutputLines)
	var lastEmit time.Time
	for scanner.Scan() {
		line := scanner.Text()
		tail.add(line)
		if c.progressRE == nil || time.Since(lastEmit) < progressInterval {
			continue
		}
		if progress, ok := parseProgress(c, line); ok {
			w.emit(a, protocol.EventProgress, progress)
			lastEmit = time.Now()
		}
	}
	// Reading stops only at EOF or a closed pipe; keep the pipe drained either way.
	_, _ = io.Copy(io.Discard, r)
}

// splitOutputLines splits on \n or \r, since tools like ffmpeg redraw progress
// with \r, and returns over-long runs in pieces so scanning never gives up.
func splitOutputLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	if len(data) >= maxLineBytes {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func parseProgress(c *Capability, line string) (protocol.Progress, bool) {
	match := c.progressRE.FindStringSubmatch(line)
	if match == nil {
		return protocol.Progress{}, false
	}
	progress := protocol.Progress{Stage: "running"}
	for i, name := range c.progressRE.SubexpNames() {
		switch name {
		case "percent":
			if value, err := strconv.ParseFloat(match[i], 64); err == nil && value >= 0 && value <= 100 {
				progress.Percent = &value
			}
		case "current":
			progress.Current = match[i]
		case "total":
			progress.Total = match[i]
		case "message":
			progress.Message = match[i]
		}
	}
	return progress, true
}

// --- Output delivery ---

// deliverOutputs uploads every declared output, then reports completion. The
// completed event is sent only after the gateway has the files.
func (w *Worker) deliverOutputs(ctx context.Context, a *attempt, c *Capability, work string) {
	root, err := os.OpenRoot(work)
	if err != nil {
		w.fail(a, protocol.CodeOutputMissing, err.Error())
		return
	}
	defer root.Close()

	names := make([]string, 0, len(c.Outputs))
	for name := range c.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)

	refs := make([]protocol.ArtifactRef, 0, len(names))
	for _, name := range names {
		rel := c.Outputs[name]
		ref, err := inspectOutput(root, rel, name+filepath.Ext(rel))
		if err != nil {
			w.fail(a, protocol.CodeOutputMissing, fmt.Sprintf("output %q: %v", name, err))
			return
		}
		w.emit(a, protocol.EventProgress, protocol.Progress{Stage: "uploading", Message: ref.Name})
		if err := w.uploadWithRetry(ctx, a, root, rel, ref); err != nil {
			if !errors.Is(err, errAttemptFenced) && ctx.Err() == nil {
				w.fail(a, protocol.CodeUploadFailed, err.Error())
			}
			return
		}
		refs = append(refs, ref)
	}
	w.emit(a, protocol.EventCompleted, protocol.Completion{Artifacts: refs})
}

// inspectOutput hashes an output file. os.Root stops a symlink the command
// created from pulling a file from elsewhere on this machine into the result.
func inspectOutput(root *os.Root, rel, artifactName string) (protocol.ArtifactRef, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return protocol.ArtifactRef{}, err
	}
	if !info.Mode().IsRegular() {
		return protocol.ArtifactRef{}, errors.New("not a regular file")
	}
	file, err := root.Open(rel)
	if err != nil {
		return protocol.ArtifactRef{}, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return protocol.ArtifactRef{}, err
	}
	return protocol.ArtifactRef{Name: artifactName, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}, nil
}

var errAttemptFenced = errors.New("attempt was fenced by the gateway")

type uploadError struct {
	permanent bool
	message   string
}

func (e *uploadError) Error() string { return e.message }

func (w *Worker) uploadWithRetry(ctx context.Context, a *attempt, root *os.Root, rel string, ref protocol.ArtifactRef) error {
	deadline := time.Now().Add(w.manifest.uploadWindow)
	backoff := time.Second
	for {
		err := w.uploadOnce(ctx, a, root, rel, ref)
		if err == nil {
			return nil
		}
		if errors.Is(err, errAttemptFenced) {
			w.abortAttempt(a.meta.AttemptID, "gateway refused the upload")
			return err
		}
		var upload *uploadError
		if errors.As(err, &upload) && upload.permanent {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s: %w", w.manifest.uploadWindow, err)
		}
		w.log.Warn("upload failed, will retry", "attempt", a.meta.AttemptID, "artifact", ref.Name, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, maxUploadBackoff)
	}
}

func (w *Worker) uploadOnce(ctx context.Context, a *attempt, root *os.Root, rel string, ref protocol.ArtifactRef) error {
	file, err := root.Open(rel)
	if err != nil {
		return &uploadError{permanent: true, message: err.Error()}
	}
	// The HTTP transport closes the file when it finishes with the body.

	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := &stallReader{reader: file, closer: file}
	body.touch()
	stop := watchStall(uploadCtx, body, cancel)
	defer stop()

	target := fmt.Sprintf("%s/v1/worker/attempts/%s/artifacts/%s", w.manifest.GatewayURL, url.PathEscape(a.meta.AttemptID), url.PathEscape(ref.Name))
	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPut, target, body)
	if err != nil {
		file.Close()
		return &uploadError{permanent: true, message: err.Error()}
	}
	req.ContentLength = ref.Size
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Content-SHA256", ref.SHA256)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	message, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	switch {
	case resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusGone:
		return errAttemptFenced
	case resp.StatusCode == http.StatusInsufficientStorage, resp.StatusCode == http.StatusRequestEntityTooLarge:
		return &uploadError{permanent: true, message: fmt.Sprintf("gateway refused the upload: %s", message)}
	default:
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, message)
	}
}

// stallReader records when the network last accepted data. A total timeout
// wouldn't work, because a large video can legitimately take hours to upload.
type stallReader struct {
	reader io.Reader
	closer io.Closer
	lastNS atomic.Int64
	done   atomic.Bool
}

func (s *stallReader) touch() { s.lastNS.Store(time.Now().UnixNano()) }

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	s.touch()
	if err != nil {
		s.done.Store(true)
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.done.Store(true)
	return s.closer.Close()
}

// watchStall cancels the upload if no data moves for uploadStall. Once the body
// is fully read, the transport's response timeout takes over.
func watchStall(ctx context.Context, body *stallReader, cancel context.CancelFunc) (stop func()) {
	stopped := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !body.done.Load() && time.Since(time.Unix(0, body.lastNS.Load())) > uploadStall {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(stopped) }
}
