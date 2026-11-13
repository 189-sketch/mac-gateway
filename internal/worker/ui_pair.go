package worker

import (
	"errors"
	"net/http"
	"strings"
)

// The page pairs the machine in two steps, like the installer: ask the gateway,
// then send the code the server shows. The request is kept here between the two,
// so the page only ever sends the code.

type uiPairRequest struct {
	Gateway string `json:"gateway"`
	Name    string `json:"name"`
}

func (s *uiServer) pairName(requested string) string {
	if name := strings.TrimSpace(requested); name != "" {
		return name
	}
	if name := s.w.config().Node.Name; name != "" {
		return name
	}
	return DefaultNodeName()
}

func (s *uiServer) pairStart(w http.ResponseWriter, r *http.Request) {
	var req uiPairRequest
	if !readUIJSON(w, r, &req) {
		return
	}
	gatewayURL, err := NormalizeGatewayURL(req.Gateway, s.w.config().AllowInsecureHTTP)
	if err != nil {
		writeUIError(w, http.StatusBadRequest, err.Error())
		return
	}
	pairing, err := RequestPairing(r.Context(), gatewayURL, s.pairName(req.Name))
	if err != nil {
		writeUIError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	s.pairing = pairing
	s.mu.Unlock()
	writeUIJSON(w, map[string]any{"gateway": gatewayURL, "name": pairing.Name, "expires_in_seconds": pairing.ExpiresIn})
}

func (s *uiServer) pairConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !readUIJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	pairing := s.pairing
	s.mu.Unlock()
	if pairing == nil {
		writeUIError(w, http.StatusConflict, "ask for a pairing code first")
		return
	}
	name, token, left, err := ConfirmPairing(r.Context(), pairing, req.Code)
	switch {
	case errors.Is(err, ErrWrongCode):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		writeUIJSON(w, map[string]any{"error": "That code isn't right. Check it against the one the server shows.", "attempts_left": left})
		return
	case errors.Is(err, ErrPairingExpired):
		s.mu.Lock()
		s.pairing = nil
		s.mu.Unlock()
		writeUIError(w, http.StatusGone, "That pairing request has expired. Get a new code.")
		return
	case err != nil:
		writeUIError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	s.pairing = nil
	s.mu.Unlock()
	s.finishPairing(w, pairing.Gateway, name, token)
}

func (s *uiServer) pairWithKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Gateway string `json:"gateway"`
		Key     string `json:"key"`
	}
	if !readUIJSON(w, r, &req) {
		return
	}
	gatewayURL, err := NormalizeGatewayURL(req.Gateway, s.w.config().AllowInsecureHTTP)
	if err != nil {
		writeUIError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeUIError(w, http.StatusBadRequest, "paste the key the server printed; it starts with fn_")
		return
	}
	name, err := IdentifyToken(r.Context(), gatewayURL, key)
	if err != nil {
		writeUIError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.finishPairing(w, gatewayURL, name, key)
}

// finishPairing saves the pairing and has the running worker connect.
func (s *uiServer) finishPairing(w http.ResponseWriter, gatewayURL, name, token string) {
	if s.configPath == "" {
		writeUIError(w, http.StatusInternalServerError, "the worker doesn't know where its settings file is")
		return
	}
	if err := SavePairing(s.configPath, gatewayURL, name, token); err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The worker would notice the changed files by itself in a moment. This
	// switches now, so the page can show the connection straight away.
	if err := s.w.ReloadFrom(s.configPath); err != nil {
		writeUIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeUIJSON(w, map[string]string{"gateway": gatewayURL, "node": name})
}

func (s *uiServer) mcpTest(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !readUIJSON(w, r, &req) {
		return
	}
	writeUIJSON(w, s.w.TestMCP(r.Context()))
}
