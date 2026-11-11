package gateway

import (
	"context"
	"testing"
)

// A test proves something about one machine, so no other may run it, however free.
func TestPickNodeKeepsATestOnItsMachine(t *testing.T) {
	offers := []CapabilityRow{
		{NodeID: "free", Name: "tts", MaxJobs: 4},
		{NodeID: "busy", Name: "tts", MaxJobs: 4},
	}
	online := onlineSet{ids: map[string]bool{"free": true, "busy": true}}
	loads := map[string]*NodeLoad{"busy": {Jobs: 3, Resources: map[string]int{}}}

	if got := pickNode(QueuedJob{Capability: "tts"}, offers, online, loads); got == nil || got.NodeID != "free" {
		t.Errorf("an ordinary job went to %+v", got)
	}
	if got := pickNode(QueuedJob{Capability: "tts", TestNode: "busy"}, offers, online, loads); got == nil || got.NodeID != "busy" {
		t.Errorf("a test for busy went to %+v", got)
	}

	loads["busy"].Jobs = 4
	if got := pickNode(QueuedJob{Capability: "tts", TestNode: "busy"}, offers, online, loads); got != nil {
		t.Errorf("a test for a full machine went to %s rather than waiting", got.NodeID)
	}

	delete(online.ids, "busy")
	if got := pickNode(QueuedJob{Capability: "tts", TestNode: "busy"}, offers, online, loads); got != nil {
		t.Errorf("a test for a machine that's gone went to %s", got.NodeID)
	}
}

// When a queued test runs out of time, the reason must be about its own machine.
func TestOffersConsidersOnlyATestsOwnMachine(t *testing.T) {
	online := onlineSet{nodes: []onlineNode{{id: "pc", capabilities: []string{"tts"}}}}
	if !online.offers("tts", nil, "") || !online.offers("tts", nil, "pc") {
		t.Error("should find the connected machine")
	}
	if online.offers("tts", nil, "mac") {
		t.Error("another machine being connected says nothing about this one")
	}
}

func TestTestBlockedExplainsWhy(t *testing.T) {
	online := onlineSet{ids: map[string]bool{"mac": true}}
	schema := `{"type":"object","required":["text"]}`
	for name, tc := range map[string]struct {
		offer    CapabilityRow
		want     string
		waitable bool
	}{
		"can run":      {CapabilityRow{NodeID: "mac", InputSchema: schema, TestInput: `{"text":"a"}`}, "", false},
		"no test":      {CapabilityRow{NodeID: "mac", InputSchema: schema}, "it doesn't declare a test", false},
		"offline":      {CapabilityRow{NodeID: "pc", InputSchema: schema, TestInput: `{"text":"a"}`}, "it isn't connected", true},
		"not ready":    {CapabilityRow{NodeID: "mac", InputSchema: schema, TestInput: `{"text":"a"}`, NotReady: "the service isn't answering"}, "it isn't ready: the service isn't answering", true},
		"bad input":    {CapabilityRow{NodeID: "mac", InputSchema: schema, TestInput: `{}`}, "its test can't run: ", false},
		"needs a file": {CapabilityRow{NodeID: "mac", InputSchema: schema, TestInput: `{"text":"a"}`, InputFiles: []string{"clip"}}, "its test can't run: ", false},
	} {
		reason, waitable := testBlocked(tc.offer, online)
		if waitable != tc.waitable || (tc.want == "" && reason != "") || (tc.want != "" && len(reason) < len(tc.want)) || (tc.want != "" && reason[:len(tc.want)] != tc.want) {
			t.Errorf("%s: %q waitable=%v, want %q waitable=%v", name, reason, waitable, tc.want, tc.waitable)
		}
	}
}

// A gateway being upgraded already has a database without the new columns.
func TestOpenStoreUpgradesADatabaseWithoutTests(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX jobs_test`,
		`ALTER TABLE jobs DROP COLUMN test_node`,
		`ALTER TABLE capabilities DROP COLUMN test_input`,
		`ALTER TABLE capabilities DROP COLUMN description`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	store.Close()

	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.LastTests(context.Background()); err != nil {
		t.Error(err)
	}
	if _, err := store.CapabilityRows(context.Background(), ""); err != nil {
		t.Error(err)
	}
}

func TestLastTestsReportsTheNewestPerMachineAndCapability(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.Exec(`INSERT INTO api_keys (id, name, key_hash, capabilities) VALUES ('k','k','h','[]')`); err != nil {
		t.Fatal(err)
	}
	add := func(id, node, capability, status string, created int64, code any) {
		t.Helper()
		var testNode any
		if node != "" {
			testNode = node
		}
		_, err := store.db.Exec(`INSERT INTO jobs (id, api_key_id, capability, input, request_hash, status, queue_timeout_ms, queue_deadline_ms, created_ms, finished_ms, error_code, test_node)
			VALUES (?, 'k', ?, '{}', 'h', ?, 0, 0, ?, ?, ?, ?)`, id, capability, status, created, created+1, code, testNode)
		if err != nil {
			t.Fatal(err)
		}
	}
	add("old", "mac", "tts", StatusFailed, 100, "START_FAILED")
	add("new", "mac", "tts", StatusSucceeded, 200, nil)
	add("other", "pc", "tts", StatusFailed, 150, "COMMAND_FAILED")
	add("plain", "", "tts", StatusFailed, 300, "COMMAND_FAILED")

	tests, err := store.LastTests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) != 2 {
		t.Fatalf("got %d results", len(tests))
	}
	if got := tests[testKey{"mac", "tts"}]; got.JobID != "new" || got.Status != StatusSucceeded || got.ErrorCode != "" || !got.Finished {
		t.Errorf("mac = %+v", got)
	}
	if got := tests[testKey{"pc", "tts"}]; got.JobID != "other" || got.ErrorCode != "COMMAND_FAILED" {
		t.Errorf("pc = %+v", got)
	}
}
