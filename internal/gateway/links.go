package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Signed links let an agent download a result file, or upload an input file,
// without holding the API key: the link itself is the permission. They're for
// agents that reach the gateway through MCP, whose key lives in the client's
// configuration rather than anywhere the agent can read it.

const (
	linkKeyFile   = "link.key"
	uploadLinkTTL = time.Hour
	linkDownload  = "d"
	linkUpload    = "u"
)

// linkClaims is what a link allows. It's signed, so it can't be changed.
type linkClaims struct {
	Kind    string `json:"k"`
	KeyID   string `json:"a"`
	FileID  string `json:"f,omitempty"`
	Name    string `json:"n,omitempty"`
	Expires int64  `json:"e"`
}

// loadLinkKey reads the key that signs links, making it on first use. Replacing
// the file makes every link given out so far stop working.
func loadLinkKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, linkKeyFile)
	key, err := os.ReadFile(path)
	if err == nil && len(key) == 32 {
		return key, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (g *Gateway) signLink(c linkClaims) string {
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, g.linkKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (g *Gateway) readLink(token string) (*linkClaims, error) {
	encoded, sig, ok := strings.Cut(token, ".")
	if !ok {
		return nil, errors.New("this link isn't valid")
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(encoded)
	gotMAC, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return nil, errors.New("this link isn't valid")
	}
	mac := hmac.New(sha256.New, g.linkKey)
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return nil, errors.New("this link isn't valid")
	}
	var c linkClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("this link isn't valid")
	}
	if time.Now().Unix() >= c.Expires {
		return nil, errors.New("this link has expired")
	}
	return &c, nil
}

// publicBase is the address agents reach the gateway at, such as
// https://fleet.example.com. Behind the usual proxy on this machine, it comes
// from the headers the proxy adds.
func (g *Gateway) publicBase(r *http.Request) string {
	if g.cfg.PublicURL != "" {
		return strings.TrimRight(g.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	// Only a proxy on this machine is trusted to say how the request arrived.
	if remote, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && isLoopbackIP(remote) {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" || proto == "http" {
			scheme = proto
		}
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
			host = forwarded
		}
	}
	return scheme + "://" + host
}

func isLoopbackIP(text string) bool {
	ip := net.ParseIP(text)
	return ip != nil && ip.IsLoopback()
}

// downloadLink returns a link to a result or input file, valid until the file
// expires or the link's lifetime ends, whichever is first.
func (g *Gateway) downloadLink(base, keyID, fileID string, fileExpires time.Time) (string, time.Time) {
	expires := time.Now().Add(g.cfg.ArtifactTTL)
	if !fileExpires.IsZero() && fileExpires.Before(expires) {
		expires = fileExpires
	}
	token := g.signLink(linkClaims{Kind: linkDownload, KeyID: keyID, FileID: fileID, Expires: expires.Unix()})
	return base + "/v1/links/" + token, expires
}

func (g *Gateway) uploadLink(base, keyID, name string) (string, time.Time) {
	expires := time.Now().Add(uploadLinkTTL)
	token := g.signLink(linkClaims{Kind: linkUpload, KeyID: keyID, Name: name, Expires: expires.Unix()})
	return base + "/v1/links/" + token, expires
}

// linkOwner checks that the key a link was made for is still allowed. Revoking a
// key ends its links too.
func (g *Gateway) linkOwner(w http.ResponseWriter, r *http.Request, want string) *linkClaims {
	claims, err := g.readLink(r.PathValue("token"))
	if err != nil {
		writeError(w, http.StatusForbidden, "LINK_INVALID", err.Error(), false)
		return nil
	}
	if claims.Kind != want {
		writeError(w, http.StatusMethodNotAllowed, "BAD_REQUEST", "this link can't be used that way", false)
		return nil
	}
	active, err := g.store.APIKeyActive(r.Context(), claims.KeyID)
	if err != nil {
		g.internalError(w, r, "check link key", err)
		return nil
	}
	if !active {
		writeError(w, http.StatusForbidden, "LINK_INVALID", "the API key this link was made for has been revoked", false)
		return nil
	}
	return claims
}

func (g *Gateway) handleLinkDownload(w http.ResponseWriter, r *http.Request) {
	claims := g.linkOwner(w, r, linkDownload)
	if claims == nil {
		return
	}
	stored, err := g.store.ArtifactFor(r.Context(), claims.FileID, claims.KeyID)
	if err == nil && stored == nil {
		stored, err = g.store.InputFileFor(r.Context(), claims.FileID, claims.KeyID)
	}
	if err != nil {
		g.internalError(w, r, "read linked file", err)
		return
	}
	if stored == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "file not found or expired", false)
		return
	}
	g.serveStoredFile(w, r, stored)
}

// handleLinkUpload stores the body as an input file, as POST /v1/files does.
func (g *Gateway) handleLinkUpload(w http.ResponseWriter, r *http.Request) {
	claims := g.linkOwner(w, r, linkUpload)
	if claims == nil {
		return
	}
	g.receiveInputFile(w, r, claims.KeyID, claims.Name)
}

// APIKeyActive reports whether an agent key exists and hasn't been revoked.
func (s *Store) APIKeyActive(ctx context.Context, keyID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE id=? AND revoked_ms IS NULL AND dashboard=0`, keyID).Scan(&n)
	return n > 0, err
}

// ArtifactExpiry returns when a result file will be removed.
func (s *Store) ArtifactExpiry(ctx context.Context, fileID string) (time.Time, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT expires_ms FROM artifacts WHERE id=?`, fileID).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return time.UnixMilli(ms), err
}

// reuseFile turns a file the key can read into an input file for a new job.
// An input file is used as it is. A result file of an earlier job is copied,
// so one tool's output can be another's input.
func (g *Gateway) reuseFile(ctx context.Context, keyID, fileID string) (string, error) {
	if f, err := g.store.InputFileFor(ctx, fileID, keyID); err != nil || f != nil {
		return fileID, err
	}
	artifact, err := g.store.ArtifactFor(ctx, fileID, keyID)
	if err != nil {
		return "", err
	}
	if artifact == nil {
		return "", fmt.Errorf("file %s doesn't exist, has expired, or belongs to another key", fileID)
	}
	from := filepath.Join(g.cfg.filesDir(), artifact.ID)
	newID := newID("file_")
	to := filepath.Join(g.cfg.filesDir(), newID)
	// A hard link costs no space. Where the system can't make one, copy.
	if err := os.Link(from, to); err != nil {
		if err := copyFile(from, to); err != nil {
			return "", err
		}
	}
	info, err := os.Stat(to)
	if err != nil {
		os.Remove(to)
		return "", err
	}
	if _, err := g.store.AddInputFile(ctx, newID, keyID, artifact.Name, artifact.SHA256, info.Size(), g.cfg.ArtifactTTL); err != nil {
		os.Remove(to)
		return "", err
	}
	return newID, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(to)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(to)
		return err
	}
	return out.Close()
}
