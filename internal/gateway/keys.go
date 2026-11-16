package gateway

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// Agent keys are stored as a hash, which is what's checked, and also sealed with
// a key kept in its own file, so the dashboard can show a key again. A copy of
// the database alone, such as a backup, doesn't give the keys away.

const secretKeyFile = "secrets.key"

type secretBox struct{ aead cipher.AEAD }

func loadSecretBox(dataDir string) (*secretBox, error) {
	file := filepath.Join(dataDir, secretKeyFile)
	key, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// O_EXCL, so two processes starting at once can't each write a different key.
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return loadSecretBox(dataDir)
		}
		if err != nil {
			return nil, err
		}
		_, err = f.Write(key)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(file)
			return nil, err
		}
		// Made by root running a command for the gateway, it must still be the
		// gateway's to read.
		matchOwner(file, dataDir)
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s should hold 32 bytes, not %d", file, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead: aead}, nil
}

func (b *secretBox) seal(secret string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(secret), nil)), nil
}

func (b *secretBox) open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("the stored key is damaged")
	}
	plain, err := b.aead.Open(nil, raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("the stored key can't be opened; was %s replaced? %w", secretKeyFile, err)
	}
	return string(plain), nil
}

// --- Names and patterns ---

// Names starting with this are the gateway's own, such as the key that owns
// machines' connection tests.
const reservedKeyPrefix = "fleet:"

var errKeyNameTaken = errors.New("a key with this name already exists, or did and was revoked")

func checkKeyName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("a key needs a name, such as who it's for")
	case name != strings.TrimSpace(name):
		return errors.New("a key's name can't start or end with a space")
	case len(name) > 64:
		return errors.New("a key's name can be at most 64 characters")
	case strings.HasPrefix(name, reservedKeyPrefix):
		return fmt.Errorf("names starting with %q are kept for the gateway", reservedKeyPrefix)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("a key's name can't contain control characters")
		}
	}
	return nil
}

func keyInsertError(name string, err error) error {
	if strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("%q: %w", name, errKeyNameTaken)
	}
	return fmt.Errorf("add key %q: %w", name, err)
}

// cleanPatterns trims glob patterns and drops empty ones. None at all means all.
func cleanPatterns(what string, patterns []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if len(p) > 128 {
			return nil, fmt.Errorf("%s pattern %.20q… is longer than 128 characters", what, p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%s pattern %q isn't valid: %v", what, p, err)
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) > 100 {
		return nil, fmt.Errorf("a key can list at most 100 %s patterns", what)
	}
	if len(out) == 0 || seen["*"] {
		return []string{"*"}, nil
	}
	return out, nil
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// nodeAllowed says whether a machine may run work limited to these patterns.
// No patterns means any machine.
func nodeAllowed(patterns []string, nodeName string) bool {
	return len(patterns) == 0 || matchesAny(patterns, nodeName)
}

// keyAllowsOffer says whether a key may use this machine's offer of a capability.
func keyAllowsOffer(key *APIKey, offer CapabilityRow) bool {
	return keyAllows(key, offer.Name) && nodeAllowed(key.NodePatterns, offer.NodeName) &&
		(key.Node == "" || offer.NodeID == key.Node)
}

// --- Listing and changing keys ---

// KeyInfo is a key as the dashboard lists it. The secret itself is fetched one
// key at a time.
type KeyInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // "agent", "dashboard" or "admin".
	Tools     []string `json:"tools"`
	Nodes     []string `json:"nodes"`
	Viewable  bool     `json:"viewable"`
	CreatedAt string   `json:"created_at,omitempty"`
	Jobs      int      `json:"jobs"`
	LastJobAt string   `json:"last_job_at,omitempty"`
}

// ErrKeyNotFound means no active key has that ID.
var ErrKeyNotFound = errors.New("no such key; it may have been revoked")

// ListKeys returns the active keys, except the gateway's own, by name.
func (s *Store) ListKeys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT k.id, k.name, k.capabilities, k.nodes, k.dashboard, k.admin, k.secret_box != '', k.created_ms,
			(SELECT COUNT(*) FROM jobs j WHERE j.api_key_id = k.id),
			(SELECT MAX(created_ms) FROM jobs j WHERE j.api_key_id = k.id)
		FROM api_keys k WHERE k.revoked_ms IS NULL AND k.name NOT LIKE ? ORDER BY k.name`, reservedKeyPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyInfo{}
	for rows.Next() {
		var k KeyInfo
		var tools, nodes string
		var dashboard, admin bool
		var created, lastJob sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &tools, &nodes, &dashboard, &admin, &k.Viewable, &created, &k.Jobs, &lastJob); err != nil {
			return nil, err
		}
		k.Kind = "agent"
		k.Tools, k.Nodes = []string{}, []string{}
		if dashboard {
			k.Kind = "dashboard"
			if admin {
				k.Kind = "admin"
			}
		} else {
			if err := json.Unmarshal([]byte(tools), &k.Tools); err != nil {
				return nil, err
			}
			if err := json.Unmarshal([]byte(nodes), &k.Nodes); err != nil {
				return nil, err
			}
		}
		if created.Valid {
			k.CreatedAt = formatMS(created.Int64)
		}
		if lastJob.Valid {
			k.LastJobAt = formatMS(lastJob.Int64)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeySecret returns an active agent key's name and secret. A key made before
// keys were kept sealed can't be shown, and gives viewable=false.
func (s *Store) KeySecret(ctx context.Context, id string) (name, secret string, viewable bool, err error) {
	var sealed string
	err = s.db.QueryRowContext(ctx, `SELECT name, secret_box FROM api_keys WHERE id = ? AND revoked_ms IS NULL AND dashboard = 0 AND name NOT LIKE ?`,
		id, reservedKeyPrefix+"%").Scan(&name, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, ErrKeyNotFound
	}
	if err != nil || sealed == "" {
		return name, "", false, err
	}
	secret, err = s.box.open(sealed)
	return name, secret, err == nil, err
}

// UpdateKeyScope changes which tools and machines an agent key may use. It
// applies to jobs submitted from then on.
func (s *Store) UpdateKeyScope(ctx context.Context, id string, tools, nodes []string) error {
	tools, err := cleanPatterns("tools", tools)
	if err != nil {
		return err
	}
	if nodes, err = cleanPatterns("machines", nodes); err != nil {
		return err
	}
	encodedTools, _ := json.Marshal(tools)
	encodedNodes, _ := json.Marshal(nodes)
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET capabilities = ?, nodes = ? WHERE id = ? AND revoked_ms IS NULL AND dashboard = 0 AND name NOT LIKE ?`,
		string(encodedTools), string(encodedNodes), id, reservedKeyPrefix+"%")
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// RevokeKeyByID invalidates a key and forgets its secret.
func (s *Store) RevokeKeyByID(ctx context.Context, id string) (name string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT name FROM api_keys WHERE id = ? AND revoked_ms IS NULL AND name NOT LIKE ?`, id, reservedKeyPrefix+"%").Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrKeyNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE api_keys SET revoked_ms = ?, secret_box = '' WHERE id = ?`, nowMS(), id)
		return err
	})
	return name, err
}

// RemoveNode revokes a machine's token so it can't connect again, and stops
// offering its tools. It's the same as `fleet-gateway node revoke`.
func (s *Store) RemoveNode(ctx context.Context, name string) (id string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT id FROM nodes WHERE name = ? AND revoked_ms IS NULL`, name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNodeNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE nodes SET revoked_ms = ? WHERE id = ?`, nowMS(), id); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM capabilities WHERE node_id = ?`, id)
		return err
	})
	return id, err
}

// ErrNodeNotFound means no active machine has that name.
var ErrNodeNotFound = errors.New("no such machine; it may have been removed already")
