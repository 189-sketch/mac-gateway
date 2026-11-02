package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"mactunnel/internal/transfer"
)

const (
	uploadIdleTimeout = 2 * time.Minute
	uploadOffsetField = "Upload-Offset"
	uploadLengthField = "Upload-Length"
	uploadDoneField   = "Upload-Complete"
)

var (
	storedFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	sha256Hex      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	inputFieldName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
)

// idleReader fails a read once the client has sent nothing for a while, so a
// connection that silently died can't hold an upload open forever. Stopping it
// through its slot lets a retry take over from a dead connection right away.
type idleReader struct {
	reader     io.Reader
	controller *http.ResponseController
	slot       *uploadSlot
}

func (r *idleReader) Read(p []byte) (int, error) {
	if r.slot != nil && r.slot.replaced.Load() {
		return 0, errors.New("a newer upload took over")
	}
	_ = r.controller.SetReadDeadline(time.Now().Add(uploadIdleTimeout))
	return r.reader.Read(p)
}

// uploadSlot is the one request currently allowed to append to a partial file.
type uploadSlot struct {
	controller *http.ResponseController
	replaced   atomic.Bool
	done       chan struct{}
}

// claimUpload makes this request the only writer of a partial file. An earlier
// request for the same file is almost always a connection the network has lost,
// so it gets stopped rather than the retry being refused.
func (g *Gateway) claimUpload(key string, controller *http.ResponseController) (*uploadSlot, func(), bool) {
	for try := 0; try < 3; try++ {
		g.uploadsMu.Lock()
		current := g.uploads[key]
		if current == nil {
			slot := &uploadSlot{controller: controller, done: make(chan struct{})}
			g.uploads[key] = slot
			g.uploadsMu.Unlock()
			return slot, func() {
				g.uploadsMu.Lock()
				delete(g.uploads, key)
				g.uploadsMu.Unlock()
				close(slot.done)
			}, true
		}
		g.uploadsMu.Unlock()
		current.replaced.Store(true)
		_ = current.controller.SetReadDeadline(time.Now())
		select {
		case <-current.done:
		case <-time.After(5 * time.Second):
			return nil, nil, false
		}
	}
	return nil, nil, false
}

func (g *Gateway) partialPath(attemptID, name, sum string) string {
	key := sha256.Sum256([]byte(attemptID + "\x00" + name + "\x00" + sum))
	return filepath.Join(g.cfg.partialDir(), hex.EncodeToString(key[:]))
}

// storageRemaining returns the free space under the storage limit, counting
// unfinished uploads other than the one being continued.
func (g *Gateway) storageRemaining(ctx context.Context, exceptPartial string) (int64, error) {
	used, err := g.store.TotalStoredBytes(ctx)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(g.cfg.partialDir())
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if filepath.Join(g.cfg.partialDir(), entry.Name()) == exceptPartial {
			continue
		}
		if info, err := entry.Info(); err == nil {
			used += info.Size()
		}
	}
	return g.cfg.MaxArtifactBytes - used, nil
}

// removeStalePartials deletes uploads abandoned long enough to have outlived
// any result they could have become.
func (g *Gateway) removeStalePartials() {
	entries, err := os.ReadDir(g.cfg.partialDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > g.cfg.ArtifactTTL {
			os.Remove(filepath.Join(g.cfg.partialDir(), entry.Name()))
		}
	}
}

type uploadTarget struct {
	node      *Node
	attemptID string
	name      string
	sum       string
	partial   string
}

// checkUpload authenticates a worker upload and confirms its attempt still counts.
func (g *Gateway) checkUpload(w http.ResponseWriter, r *http.Request) (*uploadTarget, bool) {
	node, err := g.authNode(r)
	if err != nil {
		g.internalError(w, r, "check node token", err)
		return nil, false
	}
	if node == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid node token", false)
		return nil, false
	}
	t := &uploadTarget{node: node, attemptID: r.PathValue("attempt"), name: r.PathValue("name"), sum: r.Header.Get("X-Content-SHA256")}
	if !storedFileName.MatchString(t.name) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid artifact name", false)
		return nil, false
	}
	if !sha256Hex.MatchString(t.sum) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "X-Content-SHA256 must be a lowercase hex digest", false)
		return nil, false
	}
	allowed, err := g.store.CanUpload(r.Context(), t.attemptID, node.ID)
	if err != nil {
		g.internalError(w, r, "check attempt", err)
		return nil, false
	}
	if !allowed {
		writeError(w, http.StatusGone, "ATTEMPT_FENCED", "this attempt can no longer upload", false)
		return nil, false
	}
	t.partial = g.partialPath(t.attemptID, t.name, t.sum)
	return t, true
}

// handleUploadStatus tells a worker where to continue an upload from.
func (g *Gateway) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	t, ok := g.checkUpload(w, r)
	if !ok {
		return
	}
	stored, err := g.store.ArtifactStored(r.Context(), t.attemptID, t.name, t.sum)
	if err != nil {
		g.internalError(w, r, "check artifact", err)
		return
	}
	if stored {
		w.Header().Set(uploadDoneField, "true")
	}
	var offset int64
	if info, err := os.Stat(t.partial); err == nil {
		offset = info.Size()
	}
	w.Header().Set(uploadOffsetField, strconv.FormatInt(offset, 10))
	w.WriteHeader(http.StatusOK)
}

func headerInt(r *http.Request, name string, fallback int64) (int64, bool) {
	raw := r.Header.Get(name)
	if raw == "" {
		return fallback, fallback >= 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil && value >= 0
}

// handleUploadArtifact appends to an artifact starting at Upload-Offset. Once it
// holds Upload-Length bytes, it verifies the checksum and records the result.
func (g *Gateway) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	t, ok := g.checkUpload(w, r)
	if !ok {
		return
	}
	offset, okOffset := headerInt(r, uploadOffsetField, 0)
	length, okLength := headerInt(r, uploadLengthField, r.ContentLength)
	if !okOffset || !okLength || offset > length {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Upload-Offset and Upload-Length are invalid", false)
		return
	}
	remaining, err := g.storageRemaining(r.Context(), t.partial)
	if err != nil {
		g.internalError(w, r, "measure storage", err)
		return
	}
	if length > remaining {
		writeError(w, http.StatusInsufficientStorage, "STORAGE_FULL", "artifact storage is full", false)
		return
	}

	controller := http.NewResponseController(w)
	slot, release, claimed := g.claimUpload(t.partial, controller)
	if !claimed {
		writeError(w, http.StatusConflict, "UPLOAD_BUSY", "another upload of this file is still running", true)
		return
	}
	defer release()

	file, err := os.OpenFile(t.partial, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		g.internalError(w, r, "open partial upload", err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		g.internalError(w, r, "stat partial upload", err)
		return
	}
	if info.Size() != offset {
		w.Header().Set(uploadOffsetField, strconv.FormatInt(info.Size(), 10))
		writeError(w, http.StatusConflict, "WRONG_OFFSET", "the upload must continue from Upload-Offset", true)
		return
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		g.internalError(w, r, "seek partial upload", err)
		return
	}

	body := &idleReader{reader: http.MaxBytesReader(w, r.Body, length-offset), controller: controller, slot: slot}
	received, copyErr := io.Copy(file, body)
	// Keep whatever arrived, even after an error, so the next try can continue.
	if err := file.Sync(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		var tooBig *http.MaxBytesError
		if errors.As(copyErr, &tooBig) {
			os.Remove(t.partial)
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the upload is longer than Upload-Length", false)
			return
		}
		writeError(w, http.StatusBadRequest, "UPLOAD_INTERRUPTED", "could not receive the whole upload", true)
		return
	}
	size := offset + received
	if size < length {
		w.Header().Set(uploadOffsetField, strconv.FormatInt(size, 10))
		writeJSON(w, http.StatusAccepted, map[string]int64{"offset": size})
		return
	}

	_, gotSum, err := transfer.HashFile(t.partial)
	if err != nil {
		g.internalError(w, r, "hash upload", err)
		return
	}
	if gotSum != t.sum {
		os.Remove(t.partial)
		writeError(w, http.StatusUnprocessableEntity, "CHECKSUM_MISMATCH", "upload does not match X-Content-SHA256", true)
		return
	}

	fileID := newID("file_")
	target := filepath.Join(g.cfg.filesDir(), fileID)
	if err := os.Rename(t.partial, target); err != nil {
		g.internalError(w, r, "store artifact", err)
		return
	}
	replaced, err := g.store.PutArtifact(r.Context(), t.attemptID, t.node.ID, fileID, t.name, gotSum, size, g.cfg.ArtifactTTL)
	if err != nil {
		os.Remove(target)
		if errors.Is(err, ErrFenced) {
			writeError(w, http.StatusGone, "ATTEMPT_FENCED", "this attempt can no longer upload", false)
			return
		}
		g.internalError(w, r, "record artifact", err)
		return
	}
	if replaced != "" {
		os.Remove(filepath.Join(g.cfg.filesDir(), replaced))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": fileID, "size": size, "sha256": gotSum})
}

// handleCreateFile stores an input file an agent wants a job to use. The body is
// the raw file, and ?name= gives its name.
func (g *Gateway) handleCreateFile(w http.ResponseWriter, r *http.Request, key *APIKey) {
	name := r.URL.Query().Get("name")
	if !storedFileName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "name must use letters, digits, '.', '_' or '-' and start with a letter or digit", false)
		return
	}
	wantSum := r.Header.Get("X-Content-SHA256")
	if wantSum != "" && !sha256Hex.MatchString(wantSum) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "X-Content-SHA256 must be a lowercase hex digest", false)
		return
	}
	remaining, err := g.storageRemaining(r.Context(), "")
	if err != nil {
		g.internalError(w, r, "measure storage", err)
		return
	}
	if r.ContentLength > remaining {
		writeError(w, http.StatusInsufficientStorage, "STORAGE_FULL", "file storage is full", false)
		return
	}
	body := &idleReader{reader: http.MaxBytesReader(w, r.Body, remaining), controller: http.NewResponseController(w)}
	size, gotSum, tmpPath, err := g.receiveFile(r.Context(), body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusInsufficientStorage, "STORAGE_FULL", "file storage is full", false)
			return
		}
		writeError(w, http.StatusBadRequest, "UPLOAD_INTERRUPTED", "could not receive the upload", true)
		return
	}
	defer os.Remove(tmpPath) // A no-op once the file has been renamed.
	if wantSum != "" && gotSum != wantSum {
		writeError(w, http.StatusUnprocessableEntity, "CHECKSUM_MISMATCH", "upload does not match X-Content-SHA256", true)
		return
	}
	fileID := newID("file_")
	target := filepath.Join(g.cfg.filesDir(), fileID)
	if err := os.Rename(tmpPath, target); err != nil {
		g.internalError(w, r, "store file", err)
		return
	}
	view, err := g.store.AddInputFile(r.Context(), fileID, key.ID, name, gotSum, size, g.cfg.ArtifactTTL)
	if err != nil {
		os.Remove(target)
		g.internalError(w, r, "record file", err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// handleWorkerFile lets a worker download an input file for an attempt it holds.
// Range requests come with http.ServeContent, so downloads can resume.
func (g *Gateway) handleWorkerFile(w http.ResponseWriter, r *http.Request) {
	node, err := g.authNode(r)
	if err != nil {
		g.internalError(w, r, "check node token", err)
		return
	}
	if node == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid node token", false)
		return
	}
	attemptID, field := r.PathValue("attempt"), r.PathValue("field")
	allowed, err := g.store.CanUpload(r.Context(), attemptID, node.ID)
	if err != nil {
		g.internalError(w, r, "check attempt", err)
		return
	}
	if !allowed {
		writeError(w, http.StatusGone, "ATTEMPT_FENCED", "this attempt can no longer read its files", false)
		return
	}
	file, err := g.store.AttemptInputFile(r.Context(), attemptID, field)
	if err != nil {
		g.internalError(w, r, "look up input file", err)
		return
	}
	if file == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "this attempt has no such file", false)
		return
	}
	g.serveStoredFile(w, r, file)
}

// serveStoredFile sends a stored file as an opaque download. Stored files are
// untrusted bytes, so a browser must never render them.
func (g *Gateway) serveStoredFile(w http.ResponseWriter, r *http.Request, stored *ArtifactDownload) {
	file, err := os.Open(filepath.Join(g.cfg.filesDir(), stored.ID))
	if err != nil {
		g.internalError(w, r, "open stored file", err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		g.internalError(w, r, "stat stored file", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, stored.Name))
	w.Header().Set("ETag", `"`+stored.SHA256+`"`)
	http.ServeContent(w, r, "", info.ModTime(), file)
}
