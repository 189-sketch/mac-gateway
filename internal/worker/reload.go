package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// ReloadEvery is how often the worker looks for changed tool files.
const ReloadEvery = 2 * time.Second

// WatchConfig reloads the manifest and tools folder when either changes, or when
// asked to through reload. A change that isn't valid is logged and ignored, so a
// half-saved file never takes tools away from a running machine.
func (w *Worker) WatchConfig(ctx context.Context, path string, every time.Duration) {
	last := configFingerprint(path, w.config())
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		forced := false
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.reloadNow:
			forced = true
		}
		now := configFingerprint(path, w.config())
		if now == last && !forced {
			continue
		}
		last = now
		next, err := LoadManifest(path)
		if err != nil {
			w.log.Error("the changed configuration isn't valid, so the worker keeps its current tools", "error", err)
			continue
		}
		w.Reload(ctx, next)
	}
}

// ReloadFrom reads the manifest again and switches to it before returning.
func (w *Worker) ReloadFrom(path string) error {
	next, err := LoadManifest(path)
	if err != nil {
		return err
	}
	w.Reload(*w.life.Load(), next)
	return nil
}

// RequestReload asks for the tools to be read again now, rather than at the
// next look for changes.
func (w *Worker) RequestReload() {
	select {
	case w.reloadNow <- struct{}{}:
	default:
	}
}

// Reload switches to a newly loaded manifest. Jobs already running finish with
// the tool definitions they started with.
func (w *Worker) Reload(ctx context.Context, next *Manifest) {
	current := w.config()
	if fixed := fixedChanges(current, next); len(fixed) > 0 {
		w.log.Warn("these settings change only when the worker restarts", "settings", strings.Join(fixed, ", "))
	}
	// Keep what can't change while running, so the new manifest is consistent
	// with the worker using it.
	next.StateDir = current.StateDir
	next.UploadRetryWindow, next.uploadWindow = current.UploadRetryWindow, current.uploadWindow
	next.LogLevel, next.UIListen, next.uiAddr = current.LogLevel, current.UIListen, current.uiAddr

	LogToolErrors(w.log, next)
	w.cfg.Store(next)
	// Pairing writes the gateway's address and a token, and takes effect here.
	// A token given only in the environment isn't in a file to read again, so
	// with none found the current one stays.
	gatewayURL, token := w.connection()
	if fresh, err := next.ReadToken(); err == nil {
		token = fresh
	} else if !errors.Is(err, ErrNoToken) {
		w.log.Error("couldn't read the node token, so the worker keeps the one it has", "error", err)
	}
	if next.GatewayURL != "" {
		gatewayURL = next.GatewayURL
	}
	w.SetConnection(gatewayURL, token)
	w.log.Info("tools reloaded", "tools", strings.Join(toolNames(next), ", "))
	w.restartChecks(ctx)
	if !sameHello(current, next) {
		select {
		case w.reconnect <- struct{}{}:
		default:
		}
	}
}

// LogToolErrors reports tool files the worker had to skip.
func LogToolErrors(log interface{ Error(string, ...any) }, m *Manifest) {
	for _, e := range m.ToolErrors {
		log.Error("skipped a tool file that isn't valid", "file", e.Path, "error", e.Err)
	}
}

func toolNames(m *Manifest) []string {
	names := make([]string, len(m.Capabilities))
	for i := range m.Capabilities {
		names[i] = m.Capabilities[i].Name
	}
	return names
}

func fixedChanges(a, b *Manifest) []string {
	var out []string
	for name, same := range map[string]bool{
		"state_dir":           a.StateDir == b.StateDir,
		"upload_retry_window": a.uploadWindow == b.uploadWindow,
		"log_level":           a.LogLevel == b.LogLevel,
		"ui_listen":           a.uiAddr == b.uiAddr,
	} {
		if !same {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// sameHello says whether the gateway's view of this machine is unchanged, in
// which case there's no need to reconnect.
func sameHello(a, b *Manifest) bool {
	if !reflect.DeepEqual(a.Node, b.Node) {
		return false
	}
	ja, errA := json.Marshal(a.protocolCapabilities())
	jb, errB := json.Marshal(b.protocolCapabilities())
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// configFingerprint changes when the manifest or any file in the tools folder
// does. Comparing sizes and times is enough, and much cheaper than reading.
func configFingerprint(path string, m *Manifest) string {
	toolsDir := m.ToolsPath()
	var b strings.Builder
	stamp := func(name string) {
		if info, err := os.Stat(name); err == nil {
			fmt.Fprintf(&b, "%s|%d|%d\n", name, info.Size(), info.ModTime().UnixNano())
		}
	}
	stamp(path)
	// The token file too, so pairing again with the same gateway takes effect.
	if m.TokenFile != "" {
		stamp(m.TokenFile)
	}
	if entries, err := os.ReadDir(toolsDir); err == nil {
		for _, entry := range entries {
			stamp(filepath.Join(toolsDir, entry.Name()))
		}
	}
	return b.String()
}
