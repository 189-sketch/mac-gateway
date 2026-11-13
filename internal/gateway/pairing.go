package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Pairing lets a new machine join without anyone copying a token to it. The
// machine asks to pair, the gateway makes a code and shows it only on the server
// (`fleet-gateway pair`), and whoever types that code on the machine has shown
// they can use the server. The machine then gets a node token of its own.
//
// Anyone on the internet can ask to pair, so a request does nothing by itself:
// it can't be confirmed without the code, the code allows few guesses, and
// requests are limited in number and expire quickly.

const (
	pairingLife        = 10 * time.Minute
	pairingAttempts    = 5
	maxPairingsPerHost = 3
	maxPairingsPending = 20
	pairingCodeLength  = 8
	// No 0, 1, I or O, which are easy to misread.
	pairingAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

const pairingSchemaSQL = `
CREATE TABLE IF NOT EXISTS pairings (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	remote TEXT NOT NULL DEFAULT '',
	code TEXT NOT NULL,
	attempts_left INTEGER NOT NULL,
	created_ms INTEGER NOT NULL,
	expires_ms INTEGER NOT NULL,
	paired_ms INTEGER,
	node_name TEXT
);
`

var (
	// ErrPairingGone means the request expired, was used, or never existed.
	ErrPairingGone = errors.New("this pairing request has expired; start again")
	// ErrPairingCode means the code was wrong.
	ErrPairingCode = errors.New("that code isn't right")
	// ErrPairingBusy means too many requests are waiting.
	ErrPairingBusy = errors.New("too many pairing requests are waiting; try again in a few minutes")
)

// Pairing is a machine's request to join, as shown on the server.
type Pairing struct {
	ID       string
	Name     string
	OS       string
	Arch     string
	Remote   string
	Code     string
	Created  time.Time
	Expires  time.Time
	Paired   bool
	NodeName string
}

func newPairingCode() string {
	raw := make([]byte, pairingCodeLength)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	// The alphabet has 32 letters, so each byte picks one without bias.
	for i, b := range raw {
		raw[i] = pairingAlphabet[int(b)%len(pairingAlphabet)]
	}
	return string(raw)
}

// FormatPairingCode writes a code the way it's shown to people: ABCD-EFGH.
func FormatPairingCode(code string) string {
	if len(code) != pairingCodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// cleanPairingCode undoes whatever a person did when typing a code.
func cleanPairingCode(text string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(text) {
		if strings.ContainsRune(pairingAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanNodeName turns what a machine calls itself into a valid node name.
func cleanNodeName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if len(out) > 48 {
		out = out[:48]
	}
	if out == "" {
		out = "worker"
	}
	return out
}

// CreatePairing records a machine's request to join.
func (s *Store) CreatePairing(ctx context.Context, name, os, arch, remote string) (*Pairing, error) {
	now := time.Now()
	p := &Pairing{
		ID:      newID("pair_"),
		Name:    cleanNodeName(name),
		OS:      truncate(os, 32),
		Arch:    truncate(arch, 32),
		Remote:  truncate(remote, 64),
		Code:    newPairingCode(),
		Created: now,
		Expires: now.Add(pairingLife),
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Finished requests are kept for a day, so the server can say what happened.
		if _, err := tx.Exec(`DELETE FROM pairings WHERE (paired_ms IS NULL AND expires_ms < ?) OR created_ms < ?`,
			now.UnixMilli(), now.Add(-24*time.Hour).UnixMilli()); err != nil {
			return err
		}
		var total, fromHost int
		if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(remote = ?), 0) FROM pairings WHERE paired_ms IS NULL`, p.Remote).Scan(&total, &fromHost); err != nil {
			return err
		}
		if total >= maxPairingsPending || fromHost >= maxPairingsPerHost {
			return ErrPairingBusy
		}
		_, err := tx.Exec(`INSERT INTO pairings (id, name, os, arch, remote, code, attempts_left, created_ms, expires_ms) VALUES (?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Name, p.OS, p.Arch, p.Remote, p.Code, pairingAttempts, now.UnixMilli(), p.Expires.UnixMilli())
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ConfirmPairing checks a code and, if it's right, registers the machine and
// returns its name and token. A wrong code uses up one of the request's guesses,
// and attemptsLeft says how many remain.
func (s *Store) ConfirmPairing(ctx context.Context, id, code string) (nodeName, token string, attemptsLeft int, err error) {
	code = cleanPairingCode(code)
	// A wrong guess must be recorded even though the request is refused, so the
	// outcome is carried out of the transaction rather than returned as its error.
	var outcome error
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var want, name string
		var expires int64
		row := tx.QueryRow(`SELECT code, name, attempts_left, expires_ms FROM pairings WHERE id = ? AND paired_ms IS NULL`, id)
		if err := row.Scan(&want, &name, &attemptsLeft, &expires); errors.Is(err, sql.ErrNoRows) {
			outcome = ErrPairingGone
			return nil
		} else if err != nil {
			return err
		}
		if attemptsLeft <= 0 || nowMS() > expires {
			outcome = ErrPairingGone
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) != 1 {
			attemptsLeft--
			outcome = ErrPairingCode
			if attemptsLeft == 0 {
				outcome = ErrPairingGone
			}
			_, err := tx.Exec(`UPDATE pairings SET attempts_left = ? WHERE id = ?`, attemptsLeft, id)
			return err
		}
		// A name can be registered once, even after it's revoked, so a machine that
		// pairs again gets the next free one.
		nodeName = name
		for n := 2; ; n++ {
			var taken int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name = ?`, nodeName).Scan(&taken); err != nil {
				return err
			}
			if taken == 0 {
				break
			}
			nodeName = fmt.Sprintf("%s-%d", name, n)
		}
		var hash string
		token, hash = newSecret("fn_")
		if _, err := tx.Exec(`INSERT INTO nodes (id, name, token_hash) VALUES (?, ?, ?)`, newID("node_"), nodeName, hash); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE pairings SET paired_ms = ?, node_name = ?, code = '' WHERE id = ?`, nowMS(), nodeName, id)
		return err
	})
	if err != nil {
		return "", "", 0, err
	}
	if outcome != nil {
		return "", "", attemptsLeft, outcome
	}
	return nodeName, token, attemptsLeft, nil
}

// Pairings lists requests still waiting for their code, and those that finished
// since the given time, oldest first.
func (s *Store) Pairings(ctx context.Context, finishedSince time.Time) ([]Pairing, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, os, arch, remote, code, created_ms, expires_ms, paired_ms, node_name FROM pairings
		WHERE (paired_ms IS NULL AND expires_ms >= ? AND attempts_left > 0) OR paired_ms >= ?
		ORDER BY created_ms, id`, nowMS(), finishedSince.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pairing
	for rows.Next() {
		var p Pairing
		var created, expires int64
		var paired sql.NullInt64
		var nodeName sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.OS, &p.Arch, &p.Remote, &p.Code, &created, &expires, &paired, &nodeName); err != nil {
			return nil, err
		}
		p.Created, p.Expires = time.UnixMilli(created), time.UnixMilli(expires)
		p.Paired, p.NodeName = paired.Valid, nodeName.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// SeenNode is a machine that has connected.
type SeenNode struct {
	Name  string
	Tools int
}

// NodesSeenSince lists machines that connected or sent a heartbeat since then.
func (s *Store) NodesSeenSince(ctx context.Context, since time.Time) ([]SeenNode, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.name, (SELECT COUNT(*) FROM capabilities c WHERE c.node_id = n.id)
		FROM nodes n WHERE n.revoked_ms IS NULL AND n.last_seen_ms >= ? ORDER BY n.name`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeenNode
	for rows.Next() {
		var n SeenNode
		if err := rows.Scan(&n.Name, &n.Tools); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// clientAddress is who sent a request. Behind the usual proxy on this machine,
// that's the last address the proxy added, which a client can't forge.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isLoopbackIP(host) {
		forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if last := strings.TrimSpace(forwarded[len(forwarded)-1]); net.ParseIP(last) != nil {
			return last
		}
	}
	return host
}

func (g *Gateway) handlePairRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		OS   string `json:"os"`
		Arch string `json:"arch"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body", false)
		return
	}
	p, err := g.store.CreatePairing(r.Context(), req.Name, req.OS, req.Arch, clientAddress(r))
	if errors.Is(err, ErrPairingBusy) {
		w.Header().Set("Retry-After", "120")
		writeError(w, http.StatusTooManyRequests, "TOO_MANY_PAIRINGS", err.Error(), true)
		return
	}
	if err != nil {
		g.internalError(w, r, "create pairing", err)
		return
	}
	// The code itself is never logged, and never sent to the machine asking.
	g.log.Info("pairing requested; run `fleet-pair` on this server to see its code", "name", p.Name, "from", p.Remote)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":                 p.ID,
		"name":               p.Name,
		"expires_in_seconds": int(pairingLife.Seconds()),
	})
}

func (g *Gateway) handlePairConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body", false)
		return
	}
	name, token, left, err := g.store.ConfirmPairing(r.Context(), r.PathValue("id"), req.Code)
	switch {
	case errors.Is(err, ErrPairingGone):
		writeError(w, http.StatusGone, "PAIRING_EXPIRED", err.Error(), false)
	case errors.Is(err, ErrPairingCode):
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":         JobError{Code: "WRONG_CODE", Message: err.Error()},
			"attempts_left": left,
		})
	case err != nil:
		g.internalError(w, r, "confirm pairing", err)
	default:
		g.log.Info("machine paired", "node", name, "from", clientAddress(r))
		writeJSON(w, http.StatusOK, map[string]string{"node_name": name, "node_token": token})
	}
}

// handleWorkerIdentity tells a machine the name its token was issued for, so a
// machine joined with a token needs nothing else typed in.
func (g *Gateway) handleWorkerIdentity(w http.ResponseWriter, r *http.Request) {
	node, err := g.authNode(r)
	if err != nil {
		g.internalError(w, r, "check credentials", err)
		return
	}
	if node == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid node token", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"node_name": node.Name})
}
