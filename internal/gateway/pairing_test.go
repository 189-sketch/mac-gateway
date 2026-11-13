package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func pairingStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestPairingNeedsTheCodeTheServerShows(t *testing.T) {
	store := pairingStore(t)
	ctx := context.Background()
	p, err := store.CreatePairing(ctx, "Mac mini", "darwin", "arm64", "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Mac-mini" || len(p.Code) != pairingCodeLength {
		t.Fatalf("pairing = %+v", p)
	}
	if _, _, left, err := store.ConfirmPairing(ctx, p.ID, "AAAA-AAAA"); !errors.Is(err, ErrPairingCode) || left != pairingAttempts-1 {
		t.Fatalf("a wrong code: left %d, %v", left, err)
	}
	if _, _, _, err := store.ConfirmPairing(ctx, "pair_nothing", p.Code); !errors.Is(err, ErrPairingGone) {
		t.Fatalf("a request that doesn't exist: %v", err)
	}

	// People type codes in lower case, with or without the dash.
	typed := " " + strings.ToLower(FormatPairingCode(p.Code)) + " "
	name, token, _, err := store.ConfirmPairing(ctx, p.ID, typed)
	if err != nil || name != "Mac-mini" || !strings.HasPrefix(token, "fn_") {
		t.Fatalf("the right code: %q %q %v", name, token, err)
	}
	node, err := store.NodeByToken(ctx, token)
	if err != nil || node == nil || node.Name != "Mac-mini" {
		t.Fatalf("the token doesn't sign the machine in: %+v %v", node, err)
	}
	if _, _, _, err := store.ConfirmPairing(ctx, p.ID, p.Code); !errors.Is(err, ErrPairingGone) {
		t.Fatalf("a code worked twice: %v", err)
	}

	// The server shows that it paired, and no longer shows the code.
	shown, err := store.Pairings(ctx, time.Now().Add(-time.Minute))
	if err != nil || len(shown) != 1 || !shown[0].Paired || shown[0].NodeName != "Mac-mini" || shown[0].Code != "" {
		t.Fatalf("pairings = %+v %v", shown, err)
	}
}

func TestPairingAllowsFewGuesses(t *testing.T) {
	store := pairingStore(t)
	ctx := context.Background()
	p, err := store.CreatePairing(ctx, "mac", "", "", "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < pairingAttempts; i++ {
		if _, _, _, err := store.ConfirmPairing(ctx, p.ID, "AAAAAAAA"); !errors.Is(err, ErrPairingCode) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if _, _, _, err := store.ConfirmPairing(ctx, p.ID, "AAAAAAAA"); !errors.Is(err, ErrPairingGone) {
		t.Fatalf("the last guess: %v", err)
	}
	if _, _, _, err := store.ConfirmPairing(ctx, p.ID, p.Code); !errors.Is(err, ErrPairingGone) {
		t.Fatalf("the right code after too many wrong ones: %v", err)
	}
	if shown, _ := store.Pairings(ctx, time.Now()); len(shown) != 0 {
		t.Fatalf("a used-up request is still shown: %+v", shown)
	}
}

func TestPairingRequestsAreLimitedAndExpire(t *testing.T) {
	store := pairingStore(t)
	ctx := context.Background()
	var first *Pairing
	for i := range maxPairingsPerHost {
		p, err := store.CreatePairing(ctx, "mac", "", "", "203.0.113.5")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = p
		}
	}
	if _, err := store.CreatePairing(ctx, "mac", "", "", "203.0.113.5"); !errors.Is(err, ErrPairingBusy) {
		t.Fatalf("one address asked too often: %v", err)
	}
	if _, err := store.CreatePairing(ctx, "mac", "", "", "203.0.113.6"); err != nil {
		t.Fatalf("another address was refused too: %v", err)
	}

	if _, err := store.db.Exec(`UPDATE pairings SET expires_ms = ? WHERE remote = '203.0.113.5'`, nowMS()-1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.ConfirmPairing(ctx, first.ID, first.Code); !errors.Is(err, ErrPairingGone) {
		t.Fatalf("an expired request: %v", err)
	}
	// Expired requests stop counting against the address that made them.
	if _, err := store.CreatePairing(ctx, "mac", "", "", "203.0.113.5"); err != nil {
		t.Fatalf("after the old requests expired: %v", err)
	}
}

func TestPairingGivesATakenNameASuffix(t *testing.T) {
	store := pairingStore(t)
	ctx := context.Background()
	if _, err := store.AddNode(ctx, "mac"); err != nil {
		t.Fatal(err)
	}
	// A revoked name stays taken, so the machine that had it can't be mistaken
	// for a new one.
	if _, err := store.AddNode(ctx, "mac-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeNode(ctx, "mac-2"); err != nil {
		t.Fatal(err)
	}
	p, err := store.CreatePairing(ctx, "mac", "", "", "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	name, _, _, err := store.ConfirmPairing(ctx, p.ID, p.Code)
	if err != nil || name != "mac-3" {
		t.Fatalf("name = %q, %v", name, err)
	}
}

func TestCleanNodeName(t *testing.T) {
	for in, want := range map[string]string{
		"mac-mini-1":            "mac-mini-1",
		"Ann's MacBook Pro":     "Anns-MacBook-Pro",
		"  --box  ":             "box",
		"../../etc":             "etc",
		"":                      "worker",
		"名前":                    "worker",
		strings.Repeat("a", 90): strings.Repeat("a", 48),
	} {
		if got := cleanNodeName(in); got != want {
			t.Errorf("cleanNodeName(%q) = %q, want %q", in, got, want)
		}
	}
}
