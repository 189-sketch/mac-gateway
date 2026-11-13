package worker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The configuration page is served by the running worker, on this machine only.
// Whoever can use it can make the machine run any program, so every request
// must carry a secret that only the worker's own user can read, and the page
// refuses requests that a web page from elsewhere could send.

//go:embed web
var uiFiles embed.FS

// uiCSP lets the page show pictures, audio and video a tool made, which it
// fetches with its secret and plays from blob: URLs.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data: blob:; media-src blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

const (
	// UIStateFile, in the state folder, tells `fleet-worker ui` where the page is
	// and how to sign in to it. Only the worker's user can read it.
	UIStateFile    = "ui.json"
	maxUIBodyBytes = 1 << 20
	signInCodeLife = 2 * time.Minute
	maxSignInCodes = 20
)

// UIState is what UIStateFile holds.
type UIState struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// UIOptions are what the page needs from the program running the worker.
type UIOptions struct {
	ConfigPath string
	Logs       *LogRing // May be nil.
	Version    string
}

type uiServer struct {
	w          *Worker
	opts       UIOptions
	token      string
	hosts      map[string]bool
	origins    map[string]bool
	configPath string

	mu    sync.Mutex
	codes map[string]time.Time
	// pairing is the request waiting for the code from the server, if any.
	pairing *PairingRequest

	tries *tryRunner
}

// ServeUI serves the configuration page until ctx ends. A page that can't start,
// such as when its port is taken, is logged and the worker carries on without it.
func (w *Worker) ServeUI(ctx context.Context, opts UIOptions) {
	addr := w.config().UIAddress()
	if addr == "" {
		return
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		w.log.Error("the configuration page can't start; set ui_listen to another port, or to off", "address", addr, "error", err)
		return
	}
	s, err := newUIServer(w, opts, listener.Addr().(*net.TCPAddr))
	if err != nil {
		listener.Close()
		w.log.Error("the configuration page can't start", "error", err)
		return
	}
	statePath := filepath.Join(w.stateDir, UIStateFile)
	url := "http://" + listener.Addr().String() + "/"
	if err := writeUIState(statePath, UIState{URL: url, Token: s.token, PID: os.Getpid()}); err != nil {
		listener.Close()
		w.log.Error("the configuration page can't start", "error", err)
		return
	}
	defer os.Remove(statePath)

	server := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	defer s.tries.stopAll()
	w.log.Info("configuration page ready; open it with `fleet-worker ui`", "url", url)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		w.log.Error("the configuration page stopped", "error", err)
	}
}

func newUIServer(w *Worker, opts UIOptions, addr *net.TCPAddr) (*uiServer, error) {
	token, err := randomText(32)
	if err != nil {
		return nil, err
	}
	s := &uiServer{
		w:          w,
		opts:       opts,
		token:      token,
		hosts:      map[string]bool{},
		origins:    map[string]bool{},
		configPath: opts.ConfigPath,
		codes:      map[string]time.Time{},
	}
	if s.configPath != "" {
		if abs, err := filepath.Abs(s.configPath); err == nil {
			s.configPath = abs
		}
	}
	// A page from elsewhere can point its own domain name at 127.0.0.1, so only
	// requests that name this machine are accepted.
	port := fmt.Sprint(addr.Port)
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		s.hosts[host+":"+port] = true
		s.origins["http://"+host+":"+port] = true
	}
	s.tries, err = newTryRunner(w, filepath.Join(w.stateDir, triesDir))
	if err != nil {
		return nil, err
	}
	return s, nil
}

func randomText(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// writeUIState replaces the file in one step, so a reader never sees half of it.
func writeUIState(path string, state UIState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// WriteFile keeps the mode of a file that was already there.
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ReadUIState reads where a running worker's page is.
func ReadUIState(stateDir string) (UIState, error) {
	var state UIState
	data, err := os.ReadFile(filepath.Join(stateDir, UIStateFile))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func (s *uiServer) handler() http.Handler {
	mux := http.NewServeMux()
	files, err := fs.Sub(uiFiles, "web")
	if err != nil {
		panic(err) // The embedded folder is checked at build time.
	}
	mux.Handle("GET /{$}", http.FileServerFS(files))
	mux.Handle("GET /app.js", http.FileServerFS(files))
	mux.Handle("GET /style.css", http.FileServerFS(files))

	// Signing in turns a short-lived code from `fleet-worker ui` into the secret.
	mux.HandleFunc("POST /api/sign-in", s.signIn)
	mux.HandleFunc("POST /api/sign-in-code", s.withToken(s.signInCode))

	mux.HandleFunc("GET /api/status", s.withToken(s.status))
	mux.HandleFunc("GET /api/logs", s.withToken(s.logs))
	mux.HandleFunc("GET /api/templates", s.withToken(s.templates))
	mux.HandleFunc("GET /api/browse", s.withToken(s.browse))
	mux.HandleFunc("GET /api/detect", s.withToken(s.detect))

	mux.HandleFunc("GET /api/files/{file}", s.withToken(s.readFile))
	mux.HandleFunc("PUT /api/files/{file}", s.withToken(s.saveFile))
	mux.HandleFunc("DELETE /api/files/{file}", s.withToken(s.deleteFile))
	mux.HandleFunc("POST /api/files", s.withToken(s.newFile))
	mux.HandleFunc("POST /api/files/{file}/enabled", s.withToken(s.setEnabled))
	mux.HandleFunc("GET /api/builtin/{name}", s.withToken(s.readBuiltin))

	mux.HandleFunc("POST /api/pair/start", s.withToken(s.pairStart))
	mux.HandleFunc("POST /api/pair/confirm", s.withToken(s.pairConfirm))
	mux.HandleFunc("POST /api/pair/key", s.withToken(s.pairWithKey))
	mux.HandleFunc("POST /api/mcp-test", s.withToken(s.mcpTest))

	mux.HandleFunc("POST /api/convert", s.withToken(s.convert))
	mux.HandleFunc("POST /api/check", s.withToken(s.check))
	mux.HandleFunc("POST /api/preview", s.withToken(s.preview))
	mux.HandleFunc("POST /api/try", s.withToken(s.startTry))
	mux.HandleFunc("GET /api/try/{id}", s.withToken(s.tryStatus))
	mux.HandleFunc("DELETE /api/try/{id}", s.withToken(s.cancelTry))
	mux.HandleFunc("GET /api/try/{id}/files/{name}", s.withToken(s.tryFile))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", uiCSP)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Cache-Control", "no-store")
		if !s.hosts[r.Host] {
			writeUIError(w, http.StatusMisdirectedRequest, "open the page at http://127.0.0.1 with its port, not by another name")
			return
		}
		// Browsers send Origin on every request that changes something, so a page
		// from elsewhere can't get one through.
		if origin := r.Header.Get("Origin"); origin != "" && !s.origins[origin] {
			writeUIError(w, http.StatusForbidden, "requests from other pages aren't accepted")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodDelete {
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mediaType != "application/json" {
				writeUIError(w, http.StatusUnsupportedMediaType, "send JSON")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUIBodyBytes)
		mux.ServeHTTP(w, r)
	})
}

func (s *uiServer) withToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeUIError(w, http.StatusUnauthorized, "not signed in; run `fleet-worker ui` on this machine to open the page")
			return
		}
		next(w, r)
	}
}

// signInCode makes a code that can be put in the page's address. The address
// may be seen by other users of this machine, in the list of running programs,
// so the code works once and only briefly.
func (s *uiServer) signInCode(w http.ResponseWriter, r *http.Request) {
	code, err := randomText(16)
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	now := time.Now()
	for c, expires := range s.codes {
		if now.After(expires) {
			delete(s.codes, c)
		}
	}
	if len(s.codes) >= maxSignInCodes {
		s.mu.Unlock()
		writeUIError(w, http.StatusTooManyRequests, "too many sign-in codes are waiting to be used")
		return
	}
	s.codes[code] = now.Add(signInCodeLife)
	s.mu.Unlock()
	writeUIJSON(w, map[string]string{"code": code})
}

func (s *uiServer) signIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !readUIJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	expires, ok := s.codes[req.Code]
	delete(s.codes, req.Code)
	s.mu.Unlock()
	if !ok || time.Now().After(expires) {
		writeUIError(w, http.StatusUnauthorized, "this sign-in link has expired or was already used; run `fleet-worker ui` again")
		return
	}
	writeUIJSON(w, map[string]string{"token": s.token})
}

func writeUIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeUIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func readUIJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		writeUIError(w, http.StatusBadRequest, "the request isn't valid: "+err.Error())
		return false
	}
	return true
}
