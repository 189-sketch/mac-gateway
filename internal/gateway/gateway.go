// Package gateway implements the cloud control plane: the agent API, the job
// scheduler and the endpoint workers connect to.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"mactunnel/internal/protocol"
	"mactunnel/internal/schema"
)

// Config holds the gateway's settings.
type Config struct {
	Listen            string
	DataDir           string
	Heartbeat         time.Duration
	Lease             time.Duration
	LostGrace         time.Duration
	DeadlineGrace     time.Duration
	MaxAttempts       int
	Tick              time.Duration
	ArtifactTTL       time.Duration
	MaxArtifactBytes  int64
	MaxWebSocketBytes int64
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		Listen:            "127.0.0.1:8080",
		DataDir:           "./data",
		Heartbeat:         20 * time.Second,
		Lease:             2 * time.Minute,
		LostGrace:         30 * time.Minute,
		DeadlineGrace:     6 * time.Hour,
		MaxAttempts:       3,
		Tick:              time.Second,
		ArtifactTTL:       24 * time.Hour,
		MaxArtifactBytes:  20 << 30,
		MaxWebSocketBytes: 2 << 20,
	}
}

// Gateway ties together the store, scheduler and connected workers.
type Gateway struct {
	cfg    Config
	store  *Store
	policy RetryPolicy
	log    *slog.Logger
	wake   chan struct{}

	mu    sync.Mutex
	conns map[string]*nodeConn
}

// New creates a gateway and prepares its data directories.
func New(cfg Config, store *Store, log *slog.Logger) (*Gateway, error) {
	for _, dir := range []string{cfg.filesDir(), cfg.tmpDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// Uploads interrupted by the last shutdown can never finish.
	if entries, err := os.ReadDir(cfg.tmpDir()); err == nil {
		for _, e := range entries {
			os.Remove(filepath.Join(cfg.tmpDir(), e.Name()))
		}
	}
	return &Gateway{
		cfg:   cfg,
		store: store,
		log:   log,
		wake:  make(chan struct{}, 1),
		conns: map[string]*nodeConn{},
		policy: RetryPolicy{
			Lease:         cfg.Lease,
			LostGrace:     cfg.LostGrace,
			MaxAttempts:   cfg.MaxAttempts,
			DeadlineGrace: cfg.DeadlineGrace,
		},
	}, nil
}

func (c Config) filesDir() string { return filepath.Join(c.DataDir, "files") }
func (c Config) tmpDir() string   { return filepath.Join(c.DataDir, "tmp") }

// Run serves until ctx is cancelled.
func (g *Gateway) Run(ctx context.Context) error {
	if err := g.store.ExtendActiveLeases(ctx, g.policy); err != nil {
		return fmt.Errorf("extend leases: %w", err)
	}
	listener, err := net.Listen("tcp", g.cfg.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           g.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go g.maintenanceLoop(ctx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	g.log.Info("gateway listening", "addr", listener.Addr().String())

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return nil
	}
}

func (g *Gateway) wakeScheduler() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

func (g *Gateway) maintenanceLoop(ctx context.Context) {
	ticker := time.NewTicker(g.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.wake:
			g.schedule(ctx)
		case <-ticker.C:
			g.maintain(ctx)
			g.schedule(ctx)
		}
	}
}

func (g *Gateway) maintain(ctx context.Context) {
	if _, err := g.store.ExpireLeases(ctx, g.policy); err != nil {
		g.logError(ctx, "expire leases", err)
	}
	steps := []struct {
		name string
		run  func(context.Context) ([]AttemptRef, error)
	}{
		{"expire lost jobs", g.store.ExpireLost},
		{"expire hard deadlines", g.store.ExpireHardDeadlines},
		{"expire queued jobs", func(ctx context.Context) ([]AttemptRef, error) {
			// ExpireQueued calls this inside a transaction, so it can't query the
			// store. Connected nodes come from memory instead.
			return g.store.ExpireQueued(ctx, g.onlineNodes().offers)
		}},
	}
	for _, step := range steps {
		aborts, err := step.run(ctx)
		if err != nil {
			g.logError(ctx, step.name, err)
			continue
		}
		for _, a := range aborts {
			g.sendAbort(a)
		}
	}
	dead, err := g.store.RemoveDeadArtifacts(ctx)
	if err != nil {
		g.logError(ctx, "remove artifacts", err)
	}
	for _, id := range dead {
		if err := os.Remove(filepath.Join(g.cfg.filesDir(), id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.log.Warn("could not delete artifact file", "file", id, "error", err)
		}
	}
}

func (g *Gateway) logError(ctx context.Context, what string, err error) {
	if ctx.Err() == nil {
		g.log.Error(what, "error", err)
	}
}

type onlineSet struct {
	ids       map[string]bool
	offerings map[string]bool
}

func (o onlineSet) offers(capability string) bool { return o.offerings[capability] }

func (g *Gateway) onlineNodes() onlineSet {
	g.mu.Lock()
	defer g.mu.Unlock()
	set := onlineSet{ids: map[string]bool{}, offerings: map[string]bool{}}
	for id, conn := range g.conns {
		set.ids[id] = true
		for _, name := range conn.capabilities {
			set.offerings[name] = true
		}
	}
	return set
}

// schedule assigns queued jobs to connected nodes with free capacity.
func (g *Gateway) schedule(ctx context.Context) {
	online := g.onlineNodes()
	if len(online.ids) == 0 {
		return
	}
	jobs, err := g.store.QueuedJobs(ctx, 200)
	if err != nil {
		g.logError(ctx, "list queued jobs", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	loads, err := g.store.NodeLoads(ctx)
	if err != nil {
		g.logError(ctx, "load nodes", err)
		return
	}
	offers, err := g.store.CapabilityRows(ctx, "")
	if err != nil {
		g.logError(ctx, "list capabilities", err)
		return
	}
	for _, job := range jobs {
		offer := pickNode(job, offers, online, loads)
		if offer == nil {
			continue
		}
		attemptID, previous, ok, err := g.store.CreateAttempt(ctx, job, *offer, g.policy)
		if err != nil {
			g.logError(ctx, "create attempt", err)
			continue
		}
		if !ok {
			continue
		}
		if previous != nil {
			g.sendAbort(*previous)
		}
		load := loads[offer.NodeID]
		if load == nil {
			load = &NodeLoad{Resources: map[string]int{}}
			loads[offer.NodeID] = load
		}
		load.Jobs++
		for k, v := range offer.Requires {
			load.Resources[k] += v
		}
		g.sendTo(offer.NodeID, protocol.TypeAssign, protocol.Assign{
			JobID:      job.ID,
			AttemptID:  attemptID,
			Capability: job.Capability,
			Input:      json.RawMessage(job.Input),
		})
		g.log.Info("job assigned", "job", job.ID, "attempt", attemptID, "node", offer.NodeName)
	}
}

// pickNode chooses the least busy online node that can run the job right now.
func pickNode(job QueuedJob, offers []CapabilityRow, online onlineSet, loads map[string]*NodeLoad) *CapabilityRow {
	var eligible []*CapabilityRow
	for i := range offers {
		offer := &offers[i]
		if offer.Name == job.Capability && online.ids[offer.NodeID] &&
			labelsMatch(job.Selector, offer.Labels) && hasCapacity(offer, loads[offer.NodeID]) {
			eligible = append(eligible, offer)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	jobsOn := func(offer *CapabilityRow) int {
		if load := loads[offer.NodeID]; load != nil {
			return load.Jobs
		}
		return 0
	}
	sort.SliceStable(eligible, func(i, j int) bool { return jobsOn(eligible[i]) < jobsOn(eligible[j]) })
	return eligible[0]
}

func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func hasCapacity(offer *CapabilityRow, load *NodeLoad) bool {
	used := 0
	if load != nil {
		used = load.Jobs
	}
	if used >= offer.MaxJobs {
		return false
	}
	for name, need := range offer.Requires {
		inUse := 0
		if load != nil {
			inUse = load.Resources[name]
		}
		if inUse+need > offer.Resources[name] {
			return false
		}
	}
	return true
}

// --- Worker connections ---

type nodeConn struct {
	node         *Node
	send         chan []byte
	cancel       context.CancelFunc
	capabilities []string
}

// enqueue drops a slow connection rather than block the scheduler. The worker
// reconnects and reconciles, so nothing is lost.
func (c *nodeConn) enqueue(msg []byte) {
	select {
	case c.send <- msg:
	default:
		c.cancel()
	}
}

func (g *Gateway) sendTo(nodeID, msgType string, payload any) {
	msg, err := protocol.Encode(msgType, payload)
	if err != nil {
		g.log.Error("encode message", "type", msgType, "error", err)
		return
	}
	g.mu.Lock()
	conn := g.conns[nodeID]
	g.mu.Unlock()
	if conn != nil {
		conn.enqueue(msg)
	}
}

func (g *Gateway) sendAbort(a AttemptRef) {
	g.sendTo(a.NodeID, protocol.TypeAbort, protocol.Abort{AttemptID: a.AttemptID, Reason: a.Reason})
}

var capabilityName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func validateHello(h protocol.Hello, node *Node) error {
	if h.ProtocolVersion != protocol.Version {
		return fmt.Errorf("protocol version %d is not supported (gateway uses %d)", h.ProtocolVersion, protocol.Version)
	}
	if h.NodeName != node.Name {
		return fmt.Errorf("this token belongs to node %q, but the manifest says %q", node.Name, h.NodeName)
	}
	if h.MaxJobs < 1 {
		return errors.New("max_jobs must be at least 1")
	}
	for _, c := range h.Capabilities {
		if !capabilityName.MatchString(c.Name) {
			return fmt.Errorf("invalid capability name %q", c.Name)
		}
		if _, err := schema.Compile(c.InputSchema); err != nil {
			return fmt.Errorf("capability %s: %w", c.Name, err)
		}
	}
	return nil
}

func (g *Gateway) handleWorkerConnect(w http.ResponseWriter, r *http.Request) {
	node, err := g.authNode(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not check credentials", true)
		return
	}
	if node == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid node token", false)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(g.cfg.MaxWebSocketBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hello, err := readHello(ctx, ws)
	if err != nil {
		ws.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	if err := validateHello(hello, node); err != nil {
		g.log.Warn("rejected worker", "node", node.Name, "error", err)
		ws.Close(websocket.StatusPolicyViolation, truncate(err.Error(), 120))
		return
	}

	conn := &nodeConn{node: node, send: make(chan []byte, 256), cancel: cancel}
	for _, c := range hello.Capabilities {
		conn.capabilities = append(conn.capabilities, c.Name)
	}
	if err := g.attach(ctx, conn, hello); err != nil {
		g.logError(ctx, "attach worker", err)
		ws.Close(websocket.StatusInternalError, "could not register")
		return
	}
	defer g.detach(conn)
	g.log.Info("worker connected", "node", node.Name, "attempts", len(hello.Attempts))
	g.wakeScheduler()

	go g.writeLoop(ctx, ws, conn)
	g.readLoop(ctx, ws, conn)
	g.log.Info("worker disconnected", "node", node.Name)
}

func readHello(ctx context.Context, ws *websocket.Conn) (protocol.Hello, error) {
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var hello protocol.Hello
	_, data, err := ws.Read(readCtx)
	if err != nil {
		return hello, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return hello, err
	}
	if env.Type != protocol.TypeHello {
		return hello, fmt.Errorf("unexpected message %q", env.Type)
	}
	return hello, json.Unmarshal(env.Data, &hello)
}

// attach records the worker's capabilities, reconciles its attempts and only
// then makes it schedulable, so a surviving attempt is revived before a new one
// could replace it.
func (g *Gateway) attach(ctx context.Context, conn *nodeConn, hello protocol.Hello) error {
	if err := g.store.RegisterNode(ctx, conn.node.ID, hello); err != nil {
		return err
	}
	acks, abort, err := g.store.Reconcile(ctx, conn.node.ID, hello.Attempts, g.policy)
	if err != nil {
		return err
	}
	welcome, err := protocol.Encode(protocol.TypeWelcome, protocol.Welcome{
		Acks:        acks,
		Abort:       abort,
		HeartbeatMS: g.cfg.Heartbeat.Milliseconds(),
	})
	if err != nil {
		return err
	}
	conn.send <- welcome

	g.mu.Lock()
	defer g.mu.Unlock()
	if old := g.conns[conn.node.ID]; old != nil {
		old.cancel()
	}
	g.conns[conn.node.ID] = conn
	return nil
}

func (g *Gateway) detach(conn *nodeConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conns[conn.node.ID] == conn {
		delete(g.conns, conn.node.ID)
	}
}

func (g *Gateway) writeLoop(ctx context.Context, ws *websocket.Conn, conn *nodeConn) {
	defer conn.cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-conn.send:
			writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := ws.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (g *Gateway) readLoop(ctx context.Context, ws *websocket.Conn, conn *nodeConn) {
	defer conn.cancel()
	timeout := 3 * g.cfg.Heartbeat
	for {
		readCtx, cancel := context.WithTimeout(ctx, timeout)
		_, data, err := ws.Read(readCtx)
		cancel()
		if err != nil {
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			g.log.Warn("bad message from worker", "node", conn.node.Name, "error", err)
			return
		}
		switch env.Type {
		case protocol.TypeHeartbeat:
			authorized, err := g.store.Heartbeat(ctx, conn.node.ID, g.policy)
			if err != nil {
				g.logError(ctx, "heartbeat", err)
				return
			}
			if !authorized {
				g.log.Warn("closing revoked node", "node", conn.node.Name)
				ws.Close(websocket.StatusPolicyViolation, "node token revoked")
				return
			}
			g.reply(conn, protocol.TypeHeartAck, struct{}{})
		case protocol.TypeEvent:
			var ev protocol.Event
			if err := json.Unmarshal(env.Data, &ev); err != nil {
				g.log.Warn("bad event from worker", "node", conn.node.Name, "error", err)
				return
			}
			res, err := g.store.ApplyEvent(ctx, conn.node.ID, ev, g.policy)
			if err != nil {
				// No ack, so the worker resends this event after reconnecting.
				g.logError(ctx, "apply event", err)
				return
			}
			if res.Abort {
				g.reply(conn, protocol.TypeAbort, protocol.Abort{AttemptID: ev.AttemptID, Reason: "attempt is no longer current"})
			} else {
				g.reply(conn, protocol.TypeAck, protocol.Ack{AttemptID: ev.AttemptID, Seq: res.Ack})
			}
			if res.Changed {
				g.wakeScheduler()
			}
		default:
			g.log.Warn("unknown message from worker", "node", conn.node.Name, "type", env.Type)
		}
	}
}

func (g *Gateway) reply(conn *nodeConn, msgType string, payload any) {
	msg, err := protocol.Encode(msgType, payload)
	if err != nil {
		g.log.Error("encode reply", "type", msgType, "error", err)
		return
	}
	conn.enqueue(msg)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
