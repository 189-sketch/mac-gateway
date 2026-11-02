package gateway

import (
	"context"
	"testing"
	"time"

	"mactunnel/internal/protocol"
)

var testPolicy = RetryPolicy{Lease: time.Minute, LostGrace: time.Minute, MaxAttempts: 3, DeadlineGrace: time.Minute}

type lifecycleFixture struct {
	store  *Store
	keyID  string
	nodeID string
	jobID  string
}

func newNode(t *testing.T, store *Store, name string) string {
	t.Helper()
	token, err := store.AddNode(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.NodeByToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return node.ID
}

func newFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	secret, err := store.AddAPIKey(ctx, "agent", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.APIKeyBySecret(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _, err := store.CreateJob(ctx, NewJob{
		APIKeyID: key.ID, Capability: "tts.say", Input: "{}", Selector: "{}", RequestHash: "h", QueueTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleFixture{store: store, keyID: key.ID, nodeID: newNode(t, store, "mac"), jobID: jobID}
}

// assign creates the job's next attempt on a node.
func (f *lifecycleFixture) assign(t *testing.T, nodeID string) string {
	t.Helper()
	attemptID, _, ok, err := f.store.CreateAttempt(context.Background(),
		QueuedJob{ID: f.jobID, Capability: "tts.say"},
		CapabilityRow{NodeID: nodeID, TimeoutSeconds: 60, RetryIfLost: true}, testPolicy)
	if err != nil || !ok {
		t.Fatalf("create attempt: ok=%v err=%v", ok, err)
	}
	return attemptID
}

func (f *lifecycleFixture) apply(t *testing.T, nodeID, attemptID string, seq int64, eventType string) EventResult {
	t.Helper()
	res, err := f.store.ApplyEvent(context.Background(), nodeID, protocol.Event{AttemptID: attemptID, Seq: seq, Type: eventType}, testPolicy)
	if err != nil {
		t.Fatalf("apply event %d: %v", seq, err)
	}
	return res
}

func (f *lifecycleFixture) eventCount(t *testing.T) int {
	t.Helper()
	view, err := f.store.GetJob(context.Background(), f.jobID, f.keyID, true)
	if err != nil {
		t.Fatal(err)
	}
	return len(view.Events)
}

func TestApplyEventIgnoresDuplicatesAndGaps(t *testing.T) {
	f := newFixture(t)
	attempt := f.assign(t, f.nodeID)

	if res := f.apply(t, f.nodeID, attempt, 1, protocol.EventStarted); res.Ack != 1 || res.Abort {
		t.Fatalf("first event: %+v", res)
	}
	// A worker resending after a lost ack gets the ack it missed, with no error.
	if res := f.apply(t, f.nodeID, attempt, 1, protocol.EventStarted); res.Ack != 1 || res.Abort {
		t.Errorf("duplicate: %+v", res)
	}
	// An out-of-order event isn't stored, and the ack shows how far the gateway got.
	if res := f.apply(t, f.nodeID, attempt, 5, protocol.EventProgress); res.Ack != 1 || res.Abort {
		t.Errorf("gap: %+v", res)
	}
	if got := f.eventCount(t); got != 1 {
		t.Errorf("stored %d events, want 1", got)
	}
	if res := f.apply(t, f.nodeID, attempt, 2, protocol.EventProgress); res.Ack != 2 {
		t.Errorf("next in sequence: %+v", res)
	}
}

func TestApplyEventAbortsAttemptsThatDoNotCount(t *testing.T) {
	f := newFixture(t)
	attempt := f.assign(t, f.nodeID)
	other := newNode(t, f.store, "other")

	if res := f.apply(t, other, attempt, 1, protocol.EventStarted); !res.Abort {
		t.Errorf("event from a node that doesn't hold the attempt: %+v", res)
	}
	if res := f.apply(t, f.nodeID, "att_unknown", 1, protocol.EventStarted); !res.Abort {
		t.Errorf("event for an unknown attempt: %+v", res)
	}
	if got := f.eventCount(t); got != 0 {
		t.Errorf("stored %d events, want 0", got)
	}
}
