package gateway

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

// A machine can check that agents will be able to reach its tools: it asks for
// a key that lasts a couple of minutes, then talks to /mcp with it exactly as an
// agent would. The key shows only that machine's tools, and anything it calls
// runs only on that machine, so it gives a machine no way to reach another.

const (
	mcpTestKeyPrefix = "ft_"
	mcpTestKeyLife   = 2 * time.Minute
	// selfTestKeyName is the stored key that owns these tests' jobs. Its secret is
	// thrown away when it's made, so nothing can sign in as it.
	selfTestKeyName = "fleet:connection-test"
)

type mcpTestKey struct {
	nodeID  string
	expires time.Time
}

// SelfTestKeyID returns the stored key that owns connection tests' jobs, making
// it the first time.
func (s *Store) SelfTestKeyID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE name = ?`, selfTestKeyName).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = newID("key_")
	_, hash := newSecret("fk_")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, capabilities) VALUES (?, ?, ?, '[]')`, id, selfTestKeyName, hash); err != nil {
		return "", err
	}
	return id, nil
}

func (g *Gateway) handleWorkerMCPTest(w http.ResponseWriter, r *http.Request) {
	node, err := g.authNode(r)
	if err != nil {
		g.internalError(w, r, "check credentials", err)
		return
	}
	if node == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid node token", false)
		return
	}
	secret, _ := newSecret(mcpTestKeyPrefix)
	now := time.Now()
	g.testMu.Lock()
	for old, key := range g.testKeys {
		if now.After(key.expires) || key.nodeID == node.ID {
			delete(g.testKeys, old)
		}
	}
	g.testKeys[secret] = mcpTestKey{nodeID: node.ID, expires: now.Add(mcpTestKeyLife)}
	g.testMu.Unlock()
	g.log.Info("connection test started", "node", node.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"mcp_url":            g.publicBase(r) + "/mcp",
		"key":                secret,
		"expires_in_seconds": int(mcpTestKeyLife.Seconds()),
	})
}

// mcpTestKey returns the key a connection test's secret stands for, or nil.
func (g *Gateway) mcpTestKey(ctx context.Context, secret string) (*APIKey, error) {
	g.testMu.Lock()
	key, ok := g.testKeys[secret]
	g.testMu.Unlock()
	if !ok || time.Now().After(key.expires) {
		return nil, nil
	}
	id, err := g.store.SelfTestKeyID(ctx)
	if err != nil {
		return nil, err
	}
	return &APIKey{ID: id, Patterns: []string{"*"}, Node: key.nodeID}, nil
}
