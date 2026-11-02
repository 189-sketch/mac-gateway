package worker

import (
	"testing"

	"mactunnel/internal/protocol"
)

// During shutdown the worker kills its own commands. Those exits must not be
// recorded as command failures, or the job would fail permanently instead of
// being reported as interrupted on the next start.
func TestEmitRecordsNothingDuringShutdown(t *testing.T) {
	w := &Worker{notify: make(chan struct{}, 1), attempts: map[string]*attempt{}, pids: map[string]int{}}
	a := &attempt{meta: attemptMeta{AttemptID: "att_test"}}

	w.emit(a, protocol.EventStarted, nil)
	if len(a.events) != 1 {
		t.Fatalf("events before shutdown = %d, want 1", len(a.events))
	}

	w.stopping = true
	w.emit(a, protocol.EventFailed, protocol.Failure{Code: protocol.CodeCommandFailed, Message: "killed"})
	if len(a.events) != 1 || a.terminalSeq != 0 {
		t.Errorf("events=%d terminalSeq=%d: the shutdown kill was recorded", len(a.events), a.terminalSeq)
	}
}
