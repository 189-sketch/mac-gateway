package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const (
	streamPollInterval = 250 * time.Millisecond
	streamPingInterval = 15 * time.Second
	streamWriteTimeout = 30 * time.Second
)

// eventName guards the SSE "event:" line. Event types come from workers, and a
// newline here could forge events in an agent's stream.
var eventName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// handleJobEvents streams a job as Server-Sent Events.
//
//   - Stored events carry an id, so a client that reconnects with Last-Event-ID
//     misses nothing and sees nothing twice.
//   - "status" events have no id. One is always sent on connect, then again on
//     each change, including ones no worker reported, such as "lost".
//   - "end" carries the final job, including its artifacts, and closes the stream.
func (g *Gateway) handleJobEvents(w http.ResponseWriter, r *http.Request, key *APIKey) {
	id := r.PathValue("id")
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	var cursor int64
	if raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the event cursor must be a non-negative integer", false)
			return
		}
		cursor = parsed
	}
	view, err := g.store.GetJob(r.Context(), id, key.ID, false)
	if err != nil {
		g.internalError(w, r, "read job", err)
		return
	}
	if view == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found", false)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)

	var lastStatus string
	var lastAttempts int
	nextPing := time.Now().Add(streamPingInterval)
	for {
		// Read the job before its events. A job's final event commits with its
		// final status, so a terminal job's events are all visible to this read.
		view, err := g.store.GetJob(r.Context(), id, key.ID, false)
		if err != nil || view == nil {
			g.logStreamError(r, err)
			return
		}
		events, err := g.store.JobEventsAfter(r.Context(), id, cursor)
		if err != nil {
			g.logStreamError(r, err)
			return
		}

		_ = controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		for _, ev := range events {
			name := ev.Type
			if !eventName.MatchString(name) {
				name = "unknown"
			}
			if err := writeSSE(w, strconv.FormatInt(ev.ID, 10), name, ev); err != nil {
				return
			}
			cursor = ev.ID
		}
		if view.Status != lastStatus || view.Attempts != lastAttempts {
			if err := writeSSE(w, "", "status", view); err != nil {
				return
			}
			lastStatus, lastAttempts = view.Status, view.Attempts
		}
		if view.Terminal {
			_ = writeSSE(w, "", "end", view)
			_ = controller.Flush()
			return
		}
		if time.Now().After(nextPing) {
			// Keeps proxies and NAT from closing an idle stream.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			nextPing = time.Now().Add(streamPingInterval)
		}
		if err := controller.Flush(); err != nil {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-time.After(streamPollInterval):
		}
	}
}

func (g *Gateway) logStreamError(r *http.Request, err error) {
	if err != nil {
		g.logError(r.Context(), "stream job events", err)
	}
}

func writeSSE(w io.Writer, id, name string, payload any) error {
	data, err := json.Marshal(payload) // Marshal never emits a raw newline.
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	return err
}

// cancelJob stops a job the key owns, and tells its worker to stop.
func (g *Gateway) cancelJob(ctx context.Context, jobID, keyID string) (found bool, err error) {
	found, abort, err := g.store.CancelJob(ctx, jobID, keyID)
	if err != nil || !found {
		return found, err
	}
	if abort != nil {
		g.sendAbort(*abort)
		g.wakeScheduler() // The cancelled job's capacity is free now.
	}
	return true, nil
}

// handleCancelJob stops a job. If its worker is offline, the worker is told to
// stop when it reconnects.
func (g *Gateway) handleCancelJob(w http.ResponseWriter, r *http.Request, key *APIKey) {
	id := r.PathValue("id")
	found, err := g.cancelJob(r.Context(), id, key.ID)
	if err != nil {
		g.internalError(w, r, "cancel job", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found", false)
		return
	}
	view, err := g.store.GetJob(r.Context(), id, key.ID, false)
	if err != nil {
		g.internalError(w, r, "read job", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
