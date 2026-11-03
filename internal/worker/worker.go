// Package worker implements the agent that runs on each machine. It only makes
// outbound connections and only runs what its local manifest declares.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/coder/websocket"

	"mactunnel/internal/protocol"
)

const (
	readLimitBytes     = 4 << 20
	handshakeTimeout   = 30 * time.Second
	writeTimeout       = 30 * time.Second
	minReconnectDelay  = time.Second
	maxReconnectDelay  = 30 * time.Second
	stableSessionAfter = 30 * time.Second
)

// attempt is one job run held on this machine until the gateway has its result.
type attempt struct {
	meta   attemptMeta
	dir    string
	file   *os.File
	events []protocol.Event // Every event so far; event N is at index N-1.
	sent   int64            // Highest sequence written to the current connection.

	terminalSeq int64
	cancel      context.CancelFunc
	done        chan struct{}
	aborted     bool
}

// Worker maintains the gateway connection and runs attempts.
type Worker struct {
	manifest   *Manifest
	token      string
	log        *slog.Logger
	httpClient *http.Client
	// serviceClient talks to local services. It's separate because httpClient's
	// header timeout would cut off a model that takes minutes to start answering.
	serviceClient *http.Client
	notify        chan struct{}

	mu       sync.Mutex
	attempts map[string]*attempt
	pids     map[string]int
	stopping bool
	// notReady holds why each capability that failed its last check can't run jobs.
	notReady map[string]string
	// statusChanged means the gateway hasn't been told about the latest notReady.
	statusChanged bool
}

// New creates a worker and recovers anything left by a previous run.
func New(manifest *Manifest, token string, log *slog.Logger) (*Worker, error) {
	if err := os.MkdirAll(attemptsDir(manifest), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 5 * time.Minute
	w := &Worker{
		manifest:   manifest,
		token:      token,
		log:        log,
		httpClient: &http.Client{Transport: transport},

		serviceClient: newServiceClient(),
		notify:        make(chan struct{}, 1),
		attempts:      map[string]*attempt{},
		pids:          map[string]int{},
		notReady:      map[string]string{},
	}
	if err := w.recover(); err != nil {
		return nil, fmt.Errorf("recover state: %w", err)
	}
	return w, nil
}

func attemptsDir(m *Manifest) string { return filepath.Join(m.StateDir, "attempts") }

// recover reloads attempts from disk. Any attempt without a final event died
// with the previous worker, so it is reported as failed; the gateway decides
// whether it can be retried.
func (w *Worker) recover() error {
	root := attemptsDir(w.manifest)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if !entry.IsDir() || !attemptIDRE.MatchString(entry.Name()) {
			continue
		}
		meta, ok, err := readMeta(dir)
		if err != nil {
			return err
		}
		if !ok || meta.AttemptID != entry.Name() {
			os.RemoveAll(dir)
			continue
		}
		events, file, err := loadEvents(filepath.Join(dir, eventsFile))
		if err != nil {
			return err
		}
		a := &attempt{meta: meta, dir: dir, file: file, events: events, cancel: func() {}, done: make(chan struct{})}
		close(a.done)
		for _, ev := range events {
			if isFinalEvent(ev.Type) {
				a.terminalSeq = ev.Seq
			}
		}
		w.attempts[meta.AttemptID] = a
		if a.terminalSeq == 0 {
			killOrphan(meta, w.log)
			w.cancelOrphanRemote(meta)
			w.fail(a, protocol.CodeWorkerRestarted, "the worker restarted while this job was running")
		}
		w.log.Info("recovered attempt", "attempt", meta.AttemptID, "events", len(a.events))
	}
	return nil
}

// killOrphan stops a command left over from a crashed worker, after confirming
// that the PID still belongs to the same process.
func killOrphan(meta attemptMeta, log *slog.Logger) {
	if meta.PID == 0 || meta.PIDStart == "" || processStart(meta.PID) != meta.PIDStart {
		return
	}
	log.Warn("stopping orphaned command", "attempt", meta.AttemptID, "pid", meta.PID)
	killOrphanProcess(meta.PID)
}

// cancelOrphanRemote asks a service to stop a job this worker started before it
// crashed. It doesn't hold up startup, and failing only leaves that job running.
func (w *Worker) cancelOrphanRemote(meta attemptMeta) {
	if meta.RemoteID == "" {
		return
	}
	if c := w.manifest.byName[meta.Capability]; c != nil {
		w.log.Warn("stopping orphaned service job", "attempt", meta.AttemptID, "capability", meta.Capability)
		go w.cancelRemote(c, meta.RemoteID)
	}
}

func isFinalEvent(eventType string) bool {
	return eventType == protocol.EventCompleted || eventType == protocol.EventFailed
}

// Run connects to the gateway and reconnects until ctx is cancelled. Jobs keep
// running while it's disconnected. On shutdown it stops their commands; the
// next start reports those attempts as interrupted.
func (w *Worker) Run(ctx context.Context) error {
	defer w.stopAllCommands()
	w.startChecks(ctx)
	delay := minReconnectDelay
	for ctx.Err() == nil {
		started := time.Now()
		err := w.session(ctx)
		if ctx.Err() != nil {
			break
		}
		w.log.Warn("disconnected from gateway", "error", err)
		if time.Since(started) > stableSessionAfter {
			delay = minReconnectDelay
		}
		// Jitter stops machines reconnecting in lockstep after an outage.
		wait := delay/2 + rand.N(delay/2+1)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		delay = min(delay*2, maxReconnectDelay)
	}
	return nil
}

// stopAllCommands kills running commands for a shutdown. Setting stopping first
// keeps those deliberate kills from being reported as command failures; the next
// start reports the attempts as interrupted instead.
func (w *Worker) stopAllCommands() {
	w.mu.Lock()
	w.stopping = true
	pids := make([]int, 0, len(w.pids))
	for _, pid := range w.pids {
		pids = append(pids, pid)
	}
	w.mu.Unlock()
	for _, pid := range pids {
		killGroup(pid)
	}
}

func (w *Worker) session(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	dialCtx, cancelDial := context.WithTimeout(ctx, handshakeTimeout)
	defer cancelDial()
	ws, resp, err := websocket.Dial(dialCtx, w.manifest.GatewayURL+"/v1/worker/connect", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + w.token}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return errors.New("the gateway rejected this node's token")
		}
		return fmt.Errorf("connect: %w", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(readLimitBytes)

	heartbeat, err := w.handshake(dialCtx, ws)
	if err != nil {
		return err
	}
	w.log.Info("connected to gateway", "url", w.manifest.GatewayURL)

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- w.writeLoop(ctx, ws, heartbeat)
		cancel()
	}()
	err = w.readLoop(ctx, ws, 3*heartbeat)
	cancel()
	<-writeErr
	return err
}

func (w *Worker) handshake(ctx context.Context, ws *websocket.Conn) (time.Duration, error) {
	hello := protocol.Hello{
		ProtocolVersion: protocol.Version,
		NodeName:        w.manifest.Node.Name,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Labels:          w.manifest.Node.Labels,
		Resources:       w.manifest.Node.Resources,
		MaxJobs:         w.manifest.Node.MaxJobs,
		Capabilities:    w.helloCapabilities(),
		Attempts:        w.attemptStates(),
	}
	msg, err := protocol.Encode(protocol.TypeHello, hello)
	if err != nil {
		return 0, err
	}
	if err := ws.Write(ctx, websocket.MessageText, msg); err != nil {
		return 0, fmt.Errorf("send hello: %w", err)
	}

	_, data, err := ws.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("gateway refused the connection: %w", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, fmt.Errorf("read welcome: %w", err)
	}
	if env.Type != protocol.TypeWelcome {
		return 0, fmt.Errorf("expected welcome, got %q", env.Type)
	}
	var welcome protocol.Welcome
	if err := json.Unmarshal(env.Data, &welcome); err != nil {
		return 0, fmt.Errorf("read welcome: %w", err)
	}
	if welcome.HeartbeatMS <= 0 {
		return 0, errors.New("gateway sent an invalid heartbeat interval")
	}

	for _, id := range welcome.Abort {
		w.abortAttempt(id, "gateway no longer wants this attempt")
	}
	w.applyWelcomeAcks(welcome.Acks)
	return time.Duration(welcome.HeartbeatMS) * time.Millisecond, nil
}

func (w *Worker) attemptStates() []protocol.AttemptState {
	w.mu.Lock()
	defer w.mu.Unlock()
	states := make([]protocol.AttemptState, 0, len(w.attempts))
	for id, a := range w.attempts {
		states = append(states, protocol.AttemptState{AttemptID: id, LastSeq: int64(len(a.events))})
	}
	return states
}

// applyWelcomeAcks positions each attempt to resend what the gateway is missing.
func (w *Worker) applyWelcomeAcks(acks map[string]int64) {
	w.mu.Lock()
	for _, a := range w.attempts {
		a.sent = acks[a.meta.AttemptID]
	}
	for id, seq := range acks {
		w.applyAckLocked(id, seq)
	}
	w.mu.Unlock()
	w.wakeWriter()
}

func (w *Worker) wakeWriter() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *Worker) writeLoop(ctx context.Context, ws *websocket.Conn, heartbeat time.Duration) error {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	beat, err := protocol.Encode(protocol.TypeHeartbeat, struct{}{})
	if err != nil {
		return err
	}
	for {
		if err := w.sendPending(ctx, ws); err != nil {
			return err
		}
		send := func(ctx context.Context, msg []byte) error { return writeMessage(ctx, ws, msg) }
		if err := w.sendStatus(ctx, send); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.notify:
		case <-ticker.C:
			if err := writeMessage(ctx, ws, beat); err != nil {
				return err
			}
		}
	}
}

func writeMessage(ctx context.Context, ws *websocket.Conn, msg []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return ws.Write(writeCtx, websocket.MessageText, msg)
}

// sendPending writes events the gateway hasn't received on this connection.
// A failed write ends the session, and the next welcome resets the position.
func (w *Worker) sendPending(ctx context.Context, ws *websocket.Conn) error {
	for {
		w.mu.Lock()
		var pending []protocol.Event
		for _, a := range w.attempts {
			for a.sent < int64(len(a.events)) {
				pending = append(pending, a.events[a.sent])
				a.sent++
			}
		}
		w.mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		for _, ev := range pending {
			msg, err := protocol.Encode(protocol.TypeEvent, ev)
			if err != nil {
				return err
			}
			if err := writeMessage(ctx, ws, msg); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) readLoop(ctx context.Context, ws *websocket.Conn, timeout time.Duration) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, timeout)
		_, data, err := ws.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("bad message from gateway: %w", err)
		}
		switch env.Type {
		case protocol.TypeHeartAck:
		case protocol.TypeAssign:
			var assign protocol.Assign
			if err := json.Unmarshal(env.Data, &assign); err != nil {
				return fmt.Errorf("bad assign: %w", err)
			}
			w.startAttempt(assign)
		case protocol.TypeAck:
			var ack protocol.Ack
			if err := json.Unmarshal(env.Data, &ack); err != nil {
				return fmt.Errorf("bad ack: %w", err)
			}
			w.mu.Lock()
			w.applyAckLocked(ack.AttemptID, ack.Seq)
			w.mu.Unlock()
		case protocol.TypeAbort:
			var abort protocol.Abort
			if err := json.Unmarshal(env.Data, &abort); err != nil {
				return fmt.Errorf("bad abort: %w", err)
			}
			w.abortAttempt(abort.AttemptID, abort.Reason)
		default:
			w.log.Warn("unknown message from gateway", "type", env.Type)
		}
	}
}

// applyAckLocked discards an attempt once the gateway has its final event.
func (w *Worker) applyAckLocked(attemptID string, seq int64) {
	a := w.attempts[attemptID]
	if a == nil || a.terminalSeq == 0 || seq < a.terminalSeq {
		return
	}
	delete(w.attempts, attemptID)
	delete(w.pids, attemptID)
	if a.file != nil {
		a.file.Close()
	}
	go os.RemoveAll(a.dir)
}

func (w *Worker) startAttempt(assign protocol.Assign) {
	if !attemptIDRE.MatchString(assign.AttemptID) {
		w.log.Error("refusing attempt with invalid ID")
		return
	}
	w.mu.Lock()
	if w.attempts[assign.AttemptID] != nil {
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &attempt{
		meta:   attemptMeta{JobID: assign.JobID, AttemptID: assign.AttemptID, Capability: assign.Capability},
		dir:    filepath.Join(attemptsDir(w.manifest), assign.AttemptID),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	w.attempts[assign.AttemptID] = a
	w.mu.Unlock()

	// Persist before running, so a restart can always account for this attempt.
	if err := prepareAttemptDir(a); err != nil {
		w.log.Error("could not prepare attempt", "attempt", assign.AttemptID, "error", err)
		w.fail(a, protocol.CodeStartFailed, "the worker could not write its state directory")
		close(a.done)
		return
	}
	w.log.Info("attempt started", "job", assign.JobID, "attempt", assign.AttemptID, "capability", assign.Capability)
	go w.run(ctx, a, assign)
}

func prepareAttemptDir(a *attempt) error {
	if err := os.MkdirAll(filepath.Join(a.dir, workDir), 0o700); err != nil {
		return err
	}
	if err := writeMeta(a.dir, a.meta); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(a.dir, eventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	a.file = file
	return nil
}

// recordProcess saves the PID so a later run can clean up after a crash.
func (w *Worker) recordProcess(a *attempt, pid int) {
	w.mu.Lock()
	w.pids[a.meta.AttemptID] = pid
	a.meta.PID = pid
	meta := a.meta
	w.mu.Unlock()
	meta.PIDStart = processStart(pid)
	if err := writeMeta(a.dir, meta); err != nil {
		w.log.Warn("could not record process", "attempt", meta.AttemptID, "error", err)
	}
}

// recordRemote saves a service's job ID so a later run can cancel it after a crash.
func (w *Worker) recordRemote(a *attempt, remoteID string) {
	w.mu.Lock()
	a.meta.RemoteID = remoteID
	meta := a.meta
	w.mu.Unlock()
	if err := writeMeta(a.dir, meta); err != nil {
		w.log.Warn("could not record the service's job ID", "attempt", meta.AttemptID, "error", err)
	}
}

func (a *attempt) isAborted(w *Worker) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return a.aborted
}

func (w *Worker) forgetProcess(a *attempt) {
	w.mu.Lock()
	delete(w.pids, a.meta.AttemptID)
	w.mu.Unlock()
}

// emit durably records an event and queues it for sending. Disk errors are
// logged but don't stop the event, since delivery is still possible while this
// process lives.
func (w *Worker) emit(a *attempt, eventType string, payload any) {
	var raw json.RawMessage
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			w.log.Error("encode event", "type", eventType, "error", err)
			return
		}
		raw = encoded
	}
	w.mu.Lock()
	if a.aborted || w.stopping || a.terminalSeq != 0 {
		w.mu.Unlock()
		return
	}
	ev := protocol.Event{AttemptID: a.meta.AttemptID, Seq: int64(len(a.events) + 1), Type: eventType, Payload: raw}
	if a.file != nil {
		if err := appendEvent(a.file, ev); err != nil {
			w.log.Error("could not persist event", "attempt", a.meta.AttemptID, "error", err)
		}
	}
	a.events = append(a.events, ev)
	if isFinalEvent(eventType) {
		a.terminalSeq = ev.Seq
	}
	w.mu.Unlock()
	w.wakeWriter()
}

func (w *Worker) fail(a *attempt, code, message string) {
	w.log.Warn("attempt failed", "attempt", a.meta.AttemptID, "code", code)
	w.emit(a, protocol.EventFailed, protocol.Failure{Code: code, Message: message})
}

// abortAttempt stops an attempt the gateway has given up on and deletes it. No
// further events from it are recorded or sent.
func (w *Worker) abortAttempt(attemptID, reason string) {
	w.mu.Lock()
	a := w.attempts[attemptID]
	if a == nil {
		w.mu.Unlock()
		return
	}
	a.aborted = true
	delete(w.attempts, attemptID)
	delete(w.pids, attemptID)
	w.mu.Unlock()

	w.log.Info("attempt aborted", "attempt", attemptID, "reason", reason)
	a.cancel()
	go func() {
		<-a.done
		w.mu.Lock()
		if a.file != nil {
			a.file.Close()
			a.file = nil
		}
		w.mu.Unlock()
		os.RemoveAll(a.dir)
	}()
}
