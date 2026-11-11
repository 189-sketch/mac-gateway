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
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mactunnel/internal/protocol"
	"mactunnel/internal/schema"
	"mactunnel/internal/transfer"
)

const (
	termGrace        = 5 * time.Second
	progressInterval = time.Second
	logTailLines     = 20
	// maxOutputBytes is how much of a command's output a job returns. With the
	// largest result, it still fits the gateway's 2 MiB message limit even if
	// every byte needs escaping.
	maxOutputBytes     = 64 << 10
	maxLineBytes       = 32 << 10
	maxTransferBackoff = time.Minute
)

func itoa(n int) string { return strconv.Itoa(n) }

// run executes one attempt from validation to its final event.
func (w *Worker) run(ctx context.Context, a *attempt, assign protocol.Assign) {
	defer close(a.done)

	rawInput := []byte(assign.Input)
	capability := w.config().byName[assign.Capability]
	if capability == nil {
		w.fail(a, protocol.CodeUnknownCapability, "this node does not offer "+assign.Capability)
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
	capability.applyDefaults(input)

	work := filepath.Join(a.dir, workDir)
	files, failure := w.fetchInputs(ctx, a, capability, assign.Files, work)
	if failure != nil {
		// A fenced or aborted attempt has nobody left to report to.
		if ctx.Err() == nil && !a.isAborted(w) {
			w.fail(a, failure.Code, failure.Message)
		}
		return
	}
	vars := templateVars{workdir: work, input: input, files: files}
	var result json.RawMessage
	var exitCode *int
	var outcome *protocol.Failure
	if capability.isService() {
		result, outcome = w.callService(ctx, a, capability, vars, work)
	} else {
		result, exitCode, outcome = w.runCommand(ctx, a, capability, vars, work)
	}
	if outcome != nil {
		if ctx.Err() == nil || outcome.Code == protocol.CodeTimedOut {
			w.failWith(a, *outcome)
		}
		return
	}
	w.deliverOutputs(ctx, a, capability, work, result, exitCode)
}

func (w *Worker) runCommand(ctx context.Context, a *attempt, c *Capability, vars templateVars, work string) (json.RawMessage, *int, *protocol.Failure) {
	invalid := func(err error) (json.RawMessage, *int, *protocol.Failure) {
		return nil, nil, &protocol.Failure{Code: protocol.CodeInvalidInput, Message: err.Error()}
	}
	args, err := renderCommand(c.Command[1:], vars)
	if err != nil {
		return invalid(err)
	}
	extra, err := renderArgs(c.Args, vars)
	if err != nil {
		return invalid(err)
	}
	args = append(args, extra...)
	stdin, err := render(c.Stdin, vars)
	if err != nil {
		return invalid(err)
	}
	env, err := renderEnv(c.Env, vars)
	if err != nil {
		return nil, nil, &protocol.Failure{Code: protocol.CodeStartFailed, Message: err.Error()}
	}
	if err := c.checkWorkingDir(); err != nil {
		w.log.Warn("the tool's working folder can't be used", "capability", c.Name, "cwd", c.cwd, "error", err)
		return nil, nil, &protocol.Failure{Code: protocol.CodeStartFailed, Message: "the tool's working folder is missing"}
	}
	return w.execute(ctx, a, c, args, stdin, env, work)
}

func (w *Worker) execute(ctx context.Context, a *attempt, c *Capability, args []string, stdin string, env map[string]string, work string) (json.RawMessage, *int, *protocol.Failure) {
	fail := func(code, message string) (json.RawMessage, *int, *protocol.Failure) {
		return nil, nil, &protocol.Failure{Code: code, Message: message}
	}
	tail := &logTail{}
	cmd := exec.Command(c.executable, args...)
	cmd.Args = append([]string{c.Command[0]}, args...)
	cmd.Dir = c.workingDir(work)
	cmd.Env = buildChildEnv(runtime.GOOS, os.LookupEnv, env, work)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.WaitDelay = termGrace
	setProcessGroup(cmd)

	// One pipe for both streams keeps their lines in order. With *os.File as the
	// destination, Wait returns when the process exits even if a stray child
	// still holds the pipe open. A result read from stdout gets a pipe of its own.
	reader, writer, err := os.Pipe()
	if err != nil {
		return fail(protocol.CodeStartFailed, err.Error())
	}
	defer reader.Close()
	cmd.Stdout, cmd.Stderr = writer, writer
	var stdoutReader, stdoutWriter *os.File
	var captured *outputCapture
	if c.Result != nil && c.Result.From == resultFromStdout {
		if stdoutReader, stdoutWriter, err = os.Pipe(); err != nil {
			writer.Close()
			return fail(protocol.CodeStartFailed, err.Error())
		}
		defer stdoutReader.Close()
		cmd.Stdout = stdoutWriter
		captured = &outputCapture{limit: maxResultBytes, keepEnd: c.Result.Format == formatText}
	}

	err = cmd.Start()
	writer.Close()
	if stdoutWriter != nil {
		stdoutWriter.Close()
	}
	if err != nil {
		return fail(protocol.CodeStartFailed, err.Error())
	}
	pid := cmd.Process.Pid
	groupStarted(cmd)
	w.recordProcess(a, pid)
	w.emit(a, protocol.EventStarted, nil)

	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		w.readOutput(a, c, reader, tail)
	}()
	if stdoutReader != nil {
		readers.Add(1)
		go func() {
			defer readers.Done()
			w.readOutput(a, c, io.TeeReader(stdoutReader, captured), tail)
		}()
	}
	readerDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readerDone)
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

	// From here on, every outcome carries what the command printed.
	output := tail.output()
	switch {
	case timedOut:
		return nil, nil, &protocol.Failure{Code: protocol.CodeTimedOut, Message: fmt.Sprintf("the command ran longer than its %s timeout", c.timeout), Output: output}
	case ctx.Err() != nil:
		return fail(protocol.CodeCommandFailed, "aborted")
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return nil, nil, &protocol.Failure{Code: protocol.CodeCommandFailed, Message: fmt.Sprintf("%v\n%s", waitErr, tail.String()), Output: output}
	}
	code := cmd.ProcessState.ExitCode()
	if !slices.Contains(c.successCodes, code) {
		message := fmt.Sprintf("exit status %d", code)
		if code < 0 {
			message = cmd.ProcessState.String()
		}
		return nil, &code, &protocol.Failure{Code: protocol.CodeCommandFailed, Message: message + "\n" + tail.String(), Output: output, ExitCode: &code}
	}
	var result json.RawMessage
	var failure *protocol.Failure
	switch {
	case c.Result == nil:
	case captured != nil:
		result, failure = commandResult(c.Result, captured.data, captured.cut())
	default:
		result, failure = fileResult(c.Result, work)
	}
	if failure != nil {
		failure.Output, failure.ExitCode = output, &code
	}
	a.output = output
	return result, &code, failure
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

// buildChildEnv deliberately starts empty, so the worker's own environment, which
// may hold the node token, isn't inherited by commands. It picks what a command gets. Windows programs can fail in odd ways
// without its system variables: for one, networking needs SystemRoot.
func buildChildEnv(goos string, lookup func(string) (string, bool), extra map[string]string, work string) []string {
	inherit := []string{"PATH", "HOME", "LANG", "LC_ALL"}
	env := map[string]string{"TMPDIR": work}
	if goos == "windows" {
		inherit = []string{"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "windir", "ComSpec", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"}
		env = map[string]string{"TEMP": work, "TMP": work}
	}
	for _, key := range inherit {
		if value, ok := lookup(key); ok {
			env[key] = value
		}
	}
	for key, value := range extra {
		env[key] = value
	}
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// logTail keeps what a command printed: the last few lines for an error message,
// and the last maxOutputBytes for the job's output. Two readers can add to it.
type logTail struct {
	mu        sync.Mutex
	lines     []string
	kept      []string
	keptBytes int
	cut       bool
}

func (t *logTail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.kept = append(t.kept, line)
	t.keptBytes += len(line) + 1
	for t.keptBytes > maxOutputBytes && len(t.kept) > 1 {
		t.keptBytes -= len(t.kept[0]) + 1
		t.kept = t.kept[1:]
		t.cut = true
	}
	if len(line) > 500 {
		line = line[:500]
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > logTailLines {
		t.lines = t.lines[1:]
	}
}

// output is everything kept, marked if the start was cut.
func (t *logTail) output() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := strings.Join(t.kept, "\n")
	if len(text) > maxOutputBytes {
		text = text[len(text)-maxOutputBytes:]
		t.cut = true
	}
	if t.cut {
		text = cutNote + text
	}
	return strings.ToValidUTF8(text, "\uFFFD")
}

func (t *logTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

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
		// Only at debug level, because a command can print what it was given.
		w.log.Debug("command output", "attempt", a.meta.AttemptID, "line", line)
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
func (w *Worker) deliverOutputs(ctx context.Context, a *attempt, c *Capability, work string, result json.RawMessage, exitCode *int) {
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
		out := c.Outputs[name]
		if _, err := root.Lstat(out.Path); errors.Is(err, fs.ErrNotExist) && out.Optional {
			w.log.Debug("optional output wasn't made", "attempt", a.meta.AttemptID, "output", name)
			continue
		}
		from, rel := root, out.Path
		if out.Archive == archiveZip {
			if from, rel, err = archiveOutput(root, out.Path, filepath.Join(a.dir, archivesDir), name); err != nil {
				w.fail(a, protocol.CodeOutputMissing, fmt.Sprintf("output %q: %v", name, err))
				return
			}
			defer from.Close()
		}
		ref, err := inspectOutput(from, rel, out.artifactName(name))
		if err != nil {
			w.fail(a, protocol.CodeOutputMissing, fmt.Sprintf("output %q: %v", name, err))
			return
		}
		w.log.Info("uploading result", "attempt", a.meta.AttemptID, "file", ref.Name, "bytes", ref.Size)
		w.emit(a, protocol.EventProgress, protocol.Progress{Stage: "uploading", Message: ref.Name})
		err = w.retryTransfer(ctx, a, ref.Name, func() error {
			return w.uploadOnce(ctx, a, from, rel, ref)
		})
		if err != nil {
			if !errors.Is(err, errAttemptFenced) && ctx.Err() == nil {
				w.fail(a, protocol.CodeUploadFailed, err.Error())
			}
			return
		}
		refs = append(refs, ref)
	}
	w.log.Info("attempt completed", "job", a.meta.JobID, "attempt", a.meta.AttemptID, "capability", a.meta.Capability, "files", len(refs), "duration", a.elapsed())
	w.emit(a, protocol.EventCompleted, protocol.Completion{Artifacts: refs, Result: result, ExitCode: exitCode, Output: a.output})
}

// inspectOutput hashes an output file. os.Root stops a symlink the command
// created from pulling a file from elsewhere on this machine into the result.
func inspectOutput(root *os.Root, rel, artifactName string) (protocol.ArtifactRef, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return protocol.ArtifactRef{}, err
	}
	if info.IsDir() {
		return protocol.ArtifactRef{}, errors.New("is a folder; to return a folder, set archive: zip on this output")
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

// transferError marks a failure that retrying can't fix.
type transferError struct {
	message string
}

func (e *transferError) Error() string { return e.message }

// retryTransfer repeats a file transfer until it succeeds, can't succeed, or runs
// out the retry window. Home connections drop often, so a transfer may wait out
// a long outage rather than throw away a finished job's work.
func (w *Worker) retryTransfer(ctx context.Context, a *attempt, what string, transfer func() error) error {
	deadline := time.Now().Add(w.uploadWindow)
	backoff := time.Second
	for {
		err := transfer()
		if err == nil {
			return nil
		}
		if errors.Is(err, errAttemptFenced) {
			w.abortAttempt(a.meta.AttemptID, "gateway refused the transfer")
			return err
		}
		var permanent *transferError
		if errors.As(err, &permanent) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s: %w", w.uploadWindow, err)
		}
		w.log.Warn("transfer failed, will retry", "attempt", a.meta.AttemptID, "file", what, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, maxTransferBackoff)
	}
}

func (w *Worker) artifactURL(a *attempt, name string) string {
	return fmt.Sprintf("%s/v1/worker/attempts/%s/artifacts/%s", w.gatewayURL, url.PathEscape(a.meta.AttemptID), url.PathEscape(name))
}

func (w *Worker) newUploadRequest(ctx context.Context, method, target string, body io.Reader, ref protocol.ArtifactRef) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("X-Content-SHA256", ref.SHA256)
	return req, nil
}

// uploadPosition asks the gateway how much of a file it already has.
func (w *Worker) uploadPosition(ctx context.Context, a *attempt, ref protocol.ArtifactRef) (offset int64, complete bool, err error) {
	statusCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := w.newUploadRequest(statusCtx, http.MethodHead, w.artifactURL(a, ref.Name), nil, ref)
	if err != nil {
		return 0, false, &transferError{message: err.Error()}
	}
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return 0, false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusGone:
		return 0, false, errAttemptFenced
	default:
		return 0, false, fmt.Errorf("gateway returned %d for the upload status", resp.StatusCode)
	}
	if resp.Header.Get("Upload-Complete") == "true" {
		return 0, true, nil
	}
	offset, err = strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 || offset > ref.Size {
		// Nothing useful on the gateway's side matches this file, so start over.
		return 0, false, nil
	}
	return offset, false, nil
}

func (w *Worker) uploadOnce(ctx context.Context, a *attempt, root *os.Root, rel string, ref protocol.ArtifactRef) error {
	offset, complete, err := w.uploadPosition(ctx, a, ref)
	if err != nil {
		return err
	}
	if complete {
		return nil
	}
	if offset > 0 {
		w.log.Info("resuming upload", "attempt", a.meta.AttemptID, "artifact", ref.Name, "offset", offset, "size", ref.Size)
	}

	file, err := root.Open(rel)
	if err != nil {
		return &transferError{message: err.Error()}
	}
	// The HTTP transport closes the file when it finishes with the body.
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return &transferError{message: err.Error()}
	}

	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := &transfer.StallReader{Reader: file, Closer: file}
	stop := transfer.WatchStall(uploadCtx, body, cancel)
	defer stop()

	req, err := w.newUploadRequest(uploadCtx, http.MethodPut, w.artifactURL(a, ref.Name), body, ref)
	if err != nil {
		file.Close()
		return &transferError{message: err.Error()}
	}
	req.ContentLength = ref.Size - offset
	if req.ContentLength == 0 {
		// A nil-length body would otherwise be sent as chunked.
		body.Close()
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	req.Header.Set("Upload-Length", strconv.FormatInt(ref.Size, 10))

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
		return &transferError{message: fmt.Sprintf("gateway refused the upload: %s", message)}
	default:
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, message)
	}
}

// --- Input files ---

var inputExtRE = regexp.MustCompile(`^\.[A-Za-z0-9]{1,16}$`)

// fetchInputs downloads a job's input files and returns their local paths. Local
// names come from the manifest's field name plus a checked extension, so an
// uploaded file's name can't influence the path beyond that.
func (w *Worker) fetchInputs(ctx context.Context, a *attempt, c *Capability, files []protocol.InputFile, work string) (map[string]string, *protocol.Failure) {
	paths := make(map[string]string, len(files))
	if len(files) == 0 && len(c.InputFiles) == 0 {
		return paths, nil
	}
	byField := make(map[string]protocol.InputFile, len(files))
	for _, f := range files {
		byField[f.Field] = f
	}
	if len(byField) != len(c.InputFiles) {
		return nil, &protocol.Failure{Code: protocol.CodeInvalidInput, Message: "the job's input files don't match this capability"}
	}
	dir := filepath.Join(work, "inputs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeDownloadFailed, Message: err.Error()}
	}
	for _, field := range c.InputFiles {
		f, ok := byField[field]
		if !ok {
			return nil, &protocol.Failure{Code: protocol.CodeInvalidInput, Message: fmt.Sprintf("input file %q is missing", field)}
		}
		name := field
		if ext := filepath.Ext(f.Name); inputExtRE.MatchString(ext) {
			name += ext
		}
		dest := filepath.Join(dir, name)
		w.emit(a, protocol.EventProgress, protocol.Progress{Stage: "downloading", Message: field})
		err := w.retryTransfer(ctx, a, field, func() error {
			return w.downloadOnce(ctx, a, f, dest)
		})
		if err != nil {
			return nil, &protocol.Failure{Code: protocol.CodeDownloadFailed, Message: err.Error()}
		}
		paths[field] = dest
	}
	return paths, nil
}

func (w *Worker) downloadOnce(ctx context.Context, a *attempt, f protocol.InputFile, dest string) error {
	err := transfer.Download(ctx, w.httpClient, transfer.DownloadRequest{
		URL:    fmt.Sprintf("%s/v1/worker/attempts/%s/files/%s", w.gatewayURL, url.PathEscape(a.meta.AttemptID), url.PathEscape(f.Field)),
		Token:  w.token,
		Dest:   dest,
		Size:   f.Size,
		SHA256: f.SHA256,
		OnResume: func(offset int64) {
			w.log.Info("resuming download", "attempt", a.meta.AttemptID, "file", f.Field, "offset", offset, "size", f.Size)
		},
	})
	var status *transfer.StatusError
	if errors.As(err, &status) {
		switch status.Code {
		case http.StatusGone:
			return errAttemptFenced
		case http.StatusNotFound:
			return &transferError{message: "the gateway no longer has this input file"}
		}
	}
	return err
}
