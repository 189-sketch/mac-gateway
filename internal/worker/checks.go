package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"mactunnel/internal/protocol"
)

const (
	defaultCheckEvery   = 30 * time.Second
	defaultCheckTimeout = 10 * time.Second

	// These reach the gateway, where agents can read them, so they say what's wrong
	// without naming this machine's addresses or paths. The log has the details.
	reasonProgramMissing = "the program can't be run"
	reasonServiceDown    = "the service isn't answering"
	reasonCheckFailed    = "its health check failed"
	reasonFolderMissing  = "its working folder is missing"
)

// CheckConfig says how a worker verifies that a capability really works. With no
// command or url, a command-line tool is checked by confirming its program can
// still run, and a service by connecting to its host and port.
type CheckConfig struct {
	RequestTemplate `yaml:",inline"`
	Command         []string `yaml:"command"`
	Every           string   `yaml:"every"`
	Timeout         string   `yaml:"timeout"`

	every      time.Duration
	timeout    time.Duration
	executable string
	dialAddr   string
}

// noPlaceholders applies to checks, which run with no job and so have no input.
var noPlaceholders = requestScope{
	inURL:  func(string) bool { return false },
	inBody: func(string) bool { return false },
	hint:   "a check has no job, so it can't use placeholders",
}

func (c *Capability) validateCheck() error {
	if c.Check == nil {
		c.Check = &CheckConfig{}
	}
	check := c.Check
	var err error
	if check.every, err = positiveDuration(check.Every, defaultCheckEvery); err != nil {
		return fmt.Errorf("check.every: %w", err)
	}
	if check.timeout, err = positiveDuration(check.Timeout, defaultCheckTimeout); err != nil {
		return fmt.Errorf("check.timeout: %w", err)
	}
	if len(check.Command) > 0 && check.URL != "" {
		return errors.New("check can set command or url, not both")
	}
	if check.JSON != nil || check.Body != "" || check.BodyFile != "" {
		return errors.New("check can't send a body")
	}
	if len(check.Command) > 0 {
		for _, arg := range check.Command {
			if placeholderRE.MatchString(arg) {
				return fmt.Errorf("check.command: %s", noPlaceholders.hint)
			}
		}
		executable, err := exec.LookPath(check.Command[0])
		if err != nil {
			return fmt.Errorf("check.command: cannot find program: %w", err)
		}
		if check.executable, err = filepath.Abs(executable); err != nil {
			return err
		}
		return checkExecutable(check.executable, runtime.GOOS)
	}
	if check.URL != "" {
		return check.RequestTemplate.validate("check", c.AllowRemote, noPlaceholders)
	}
	if check.Method != "" || len(check.Headers) > 0 {
		return errors.New("check.method and check.headers need check.url")
	}
	if c.isService() {
		check.dialAddr, err = dialAddress(c.Request.URL)
		return err
	}
	return nil
}

// dialAddress finds the host and port a request URL connects to. Only its path
// and query can contain placeholders, and those don't matter here.
func dialAddress(raw string) (string, error) {
	parsed, err := url.Parse(placeholderRE.ReplaceAllString(raw, "x"))
	if err != nil {
		return "", err
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(parsed.Hostname(), port), nil
}

// checkCapability reports why a capability can't run jobs right now, or "" if it
// can. reason is safe to send to the gateway, and detail is only for the log.
func checkCapability(ctx context.Context, client *http.Client, c *Capability) (reason, detail string) {
	ctx, cancel := context.WithTimeout(ctx, c.Check.timeout)
	defer cancel()
	check := c.Check
	switch {
	case len(check.Command) > 0:
		if err := runCheckCommand(ctx, c); err != nil {
			return reasonCheckFailed, err.Error()
		}
	case check.URL != "":
		req, err := buildRequest(ctx, &check.RequestTemplate, templateVars{})
		if err != nil {
			return reasonCheckFailed, err.Error()
		}
		resp, err := client.Do(req)
		if err != nil {
			return reasonServiceDown, err.Error()
		}
		defer resp.Body.Close()
		_, _ = io.CopyN(io.Discard, resp.Body, 1<<16)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return reasonCheckFailed, fmt.Sprintf("answered %d", resp.StatusCode)
		}
	case check.dialAddr != "":
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", check.dialAddr)
		if err != nil {
			return reasonServiceDown, err.Error()
		}
		conn.Close()
	default:
		if err := programRunnable(c.executable, runtime.GOOS); err != nil {
			return reasonProgramMissing, err.Error()
		}
		if err := c.checkWorkingDir(); err != nil {
			return reasonFolderMissing, err.Error()
		}
	}
	return "", ""
}

// runCheckCommand runs with the same limited environment as a job's command. It
// can be stopped along with anything it starts, so a stuck check can't pile up.
func runCheckCommand(ctx context.Context, c *Capability) error {
	dir := os.TempDir()
	cmd := exec.Command(c.Check.executable, c.Check.Command[1:]...)
	cmd.Args = c.Check.Command
	cmd.Dir = dir
	cmd.Env = buildChildEnv(runtime.GOOS, os.LookupEnv, c.staticEnv(), dir)
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	groupStarted(cmd)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		killGroup(pid)
		return err
	case <-ctx.Done():
		killGroup(pid)
		<-waited
		return fmt.Errorf("didn't finish within %s", c.Check.timeout)
	}
}

func programRunnable(path, goos string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	// Windows has no execute bit to look at.
	if goos != "windows" && info.Mode().Perm()&0o111 == 0 {
		return errors.New("not executable")
	}
	return nil
}

// restartChecks verifies every tool once, so the next hello reports the truth,
// then keeps checking in the background until ctx ends or the tools change.
func (w *Worker) restartChecks(parent context.Context) {
	w.checksMu.Lock()
	defer w.checksMu.Unlock()
	if w.stopChecks != nil {
		w.stopChecks()
	}
	ctx, cancel := context.WithCancel(parent)
	w.stopChecks = cancel

	m := w.config()
	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		w.log.Info("capability offered", "capability", c.Name, "adapter", c.Adapter, "check_every", c.Check.every.String())
	}
	if len(m.Capabilities) == 0 {
		w.log.Warn("this worker offers no tools yet", "tools_dir", m.ToolsPath())
	}
	var first sync.WaitGroup
	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		first.Add(1)
		go func() {
			w.recordCheck(ctx, c)
			first.Done()
			ticker := time.NewTicker(c.Check.every)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					w.recordCheck(ctx, c)
				}
			}
		}()
	}
	first.Wait()

	// A tool that's gone has nothing to report.
	w.mu.Lock()
	for name := range w.notReady {
		if m.byName[name] == nil {
			delete(w.notReady, name)
			w.statusChanged = true
		}
	}
	w.mu.Unlock()
}

func (w *Worker) recordCheck(ctx context.Context, c *Capability) {
	reason, detail := checkCapability(ctx, w.serviceClient, c)
	if ctx.Err() != nil {
		return // A check cut short by shutdown says nothing about the capability.
	}
	w.log.Debug("capability checked", "capability", c.Name, "ready", reason == "", "detail", detail)
	w.mu.Lock()
	previous := w.notReady[c.Name]
	if reason == previous {
		w.mu.Unlock()
		return
	}
	if reason == "" {
		delete(w.notReady, c.Name)
	} else {
		w.notReady[c.Name] = reason
	}
	w.statusChanged = true
	w.mu.Unlock()

	if reason == "" {
		w.log.Info("capability is ready", "capability", c.Name)
	} else {
		w.log.Warn("capability isn't ready", "capability", c.Name, "reason", reason, "detail", detail)
	}
	w.wakeWriter()
}

// helloCapabilities describes every capability as of this moment. The hello
// carries the full picture, so nothing needs resending for this connection.
func (w *Worker) helloCapabilities() []protocol.Capability {
	declared := w.config().protocolCapabilities()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.statusChanged = false
	for i := range declared {
		declared[i].NotReady = w.notReady[declared[i].Name]
	}
	return declared
}

// sendStatus tells the gateway about a change in what's ready. If this fails the
// session ends, and the next hello reports the current state.
func (w *Worker) sendStatus(ctx context.Context, send func(context.Context, []byte) error) error {
	w.mu.Lock()
	if !w.statusChanged {
		w.mu.Unlock()
		return nil
	}
	w.statusChanged = false
	snapshot := make(map[string]string, len(w.notReady))
	for name, reason := range w.notReady {
		snapshot[name] = reason
	}
	w.mu.Unlock()
	msg, err := protocol.Encode(protocol.TypeCapabilityStatus, protocol.CapabilityStatus{NotReady: snapshot})
	if err != nil {
		return err
	}
	return send(ctx, msg)
}

// CheckResult is one capability's state, for `fleet-worker --check`.
type CheckResult struct {
	Name   string
	Reason string // Empty means ready.
	Detail string
}

// VerifyCapabilities runs every capability's check once, for a person looking at
// this machine, so the details can be shown.
func VerifyCapabilities(ctx context.Context, m *Manifest) []CheckResult {
	client := newServiceClient()
	results := make([]CheckResult, len(m.Capabilities))
	var wg sync.WaitGroup
	for i := range m.Capabilities {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &m.Capabilities[i]
			reason, detail := checkCapability(ctx, client, c)
			results[i] = CheckResult{Name: c.Name, Reason: reason, Detail: detail}
		}()
	}
	wg.Wait()
	return results
}
