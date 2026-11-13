package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pairing connects this machine to a gateway without anyone copying a token to
// it. The machine asks the gateway to pair, the gateway shows a code on the
// server, and the person types that code here. A machine can also be connected
// with a node token made on the server, which needs no code.

// PairTimeout bounds each request made while pairing.
const PairTimeout = 20 * time.Second

var (
	// ErrWrongCode means the gateway didn't accept the pairing code.
	ErrWrongCode = errors.New("that code isn't right")
	// ErrPairingExpired means the request ran out of time or guesses.
	ErrPairingExpired = errors.New("this pairing request has expired")
)

// PairingRequest is a request the gateway is holding until its code arrives.
type PairingRequest struct {
	ID        string `json:"id"`
	Gateway   string `json:"gateway"`
	Name      string `json:"name"`
	ExpiresIn int    `json:"expires_in_seconds"`
}

// NormalizeGatewayURL turns what a person typed, such as 203.0.113.10 or
// fleet.example.com, into the gateway's address.
func NormalizeGatewayURL(text string, allowInsecure bool) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("give the gateway's address, such as 203.0.113.10 or fleet.example.com")
	}
	if !strings.Contains(text, "://") {
		text = "https://" + text
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%q isn't an address; give the server's IP address or domain", text)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("give only the server's address, such as https://fleet.example.com")
	}
	// A pasted MCP or dashboard link still names the right server.
	address := parsed.Scheme + "://" + parsed.Host
	if err := checkGatewayURL(address, allowInsecure); err != nil {
		return "", errors.New(strings.Replace(err.Error(), "gateway_url", "the gateway's address", 1))
	}
	return address, nil
}

// DefaultNodeName is what this machine is called until someone names it.
func DefaultNodeName() string {
	name, _ := os.Hostname()
	name, _, _ = strings.Cut(name, ".")
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	name = strings.TrimLeft(b.String(), "-_")
	if name == "" {
		return "worker"
	}
	return name
}

func pairClient() *http.Client { return &http.Client{Timeout: PairTimeout} }

// gatewayError is what the gateway said when it refused a request.
type gatewayError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	AttemptsLeft int `json:"attempts_left"`
}

func pairCall(ctx context.Context, method, address, token string, body, out any) (int, *gatewayError, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := pairClient().Do(req)
	if err != nil {
		return 0, nil, explainDialError(address, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode >= 300 {
		var refused gatewayError
		_ = json.Unmarshal(raw, &refused)
		return resp.StatusCode, &refused, nil
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("%s answered with something that isn't a gateway's reply; check the address", hostOf(address))
		}
	}
	return resp.StatusCode, nil, nil
}

func hostOf(address string) string {
	if parsed, err := url.Parse(address); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return address
}

// explainDialError says what to check when a gateway can't be reached, since the
// raw error means little to someone who has only typed an address.
func explainDialError(address string, err error) error {
	host := hostOf(address)
	var dns *net.DNSError
	text := err.Error()
	switch {
	case errors.As(err, &dns):
		return fmt.Errorf("there's no server named %s; check the address", host)
	case strings.Contains(text, "certificate") || strings.Contains(text, "tls:"):
		return fmt.Errorf("%s answered, but its HTTPS certificate isn't ready or isn't valid. If the gateway was only just installed, wait a minute and try again (%v)", host, err)
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(text, "timeout") || strings.Contains(text, "deadline"):
		return fmt.Errorf("%s didn't answer. Check the address, and that ports 80 and 443 are open on the server", host)
	case strings.Contains(text, "refused"):
		return fmt.Errorf("%s refused the connection. Check that the gateway is installed and running there", host)
	}
	return fmt.Errorf("couldn't reach %s: %w", host, err)
}

func refusal(status int, refused *gatewayError, address string) error {
	if refused.Error.Message != "" {
		return errors.New(refused.Error.Message)
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return fmt.Errorf("%s doesn't offer pairing. It may not be a gateway, or its gateway is older than this worker; upgrade it by running its install command again", hostOf(address))
	}
	return fmt.Errorf("%s answered %d", hostOf(address), status)
}

// RequestPairing asks a gateway to pair this machine. The gateway then shows a
// code on the server, which ConfirmPairing needs.
func RequestPairing(ctx context.Context, gatewayURL, name string) (*PairingRequest, error) {
	out := &PairingRequest{Gateway: gatewayURL}
	status, refused, err := pairCall(ctx, http.MethodPost, gatewayURL+"/v1/pair", "", map[string]string{
		"name": name, "os": runtime.GOOS, "arch": runtime.GOARCH,
	}, out)
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refusal(status, refused, gatewayURL)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("%s answered with something that isn't a gateway's reply; check the address", hostOf(gatewayURL))
	}
	return out, nil
}

// ConfirmPairing sends the code from the server. On success it returns the name
// the gateway gave this machine and its token. For a wrong code the error is
// ErrWrongCode and attemptsLeft says how many more guesses the request allows.
func ConfirmPairing(ctx context.Context, request *PairingRequest, code string) (name, token string, attemptsLeft int, err error) {
	var out struct {
		NodeName  string `json:"node_name"`
		NodeToken string `json:"node_token"`
	}
	status, refused, err := pairCall(ctx, http.MethodPost, request.Gateway+"/v1/pair/"+url.PathEscape(request.ID), "", map[string]string{"code": code}, &out)
	if err != nil {
		return "", "", 0, err
	}
	if refused != nil {
		switch refused.Error.Code {
		case "WRONG_CODE":
			return "", "", refused.AttemptsLeft, ErrWrongCode
		case "PAIRING_EXPIRED":
			return "", "", 0, ErrPairingExpired
		}
		return "", "", 0, refusal(status, refused, request.Gateway)
	}
	if out.NodeName == "" || out.NodeToken == "" {
		return "", "", 0, errors.New("the gateway's reply was incomplete")
	}
	return out.NodeName, out.NodeToken, 0, nil
}

// IdentifyToken asks a gateway which machine a node token was made for.
func IdentifyToken(ctx context.Context, gatewayURL, token string) (string, error) {
	var out struct {
		NodeName string `json:"node_name"`
	}
	status, refused, err := pairCall(ctx, http.MethodGet, gatewayURL+"/v1/worker/identity", token, nil, &out)
	if err != nil {
		return "", err
	}
	if refused != nil {
		if status == http.StatusUnauthorized {
			return "", fmt.Errorf("%s doesn't know that key. Make a new one on the server with: sudo fleet-add-worker NAME", hostOf(gatewayURL))
		}
		return "", refusal(status, refused, gatewayURL)
	}
	if out.NodeName == "" {
		return "", errors.New("the gateway's reply was incomplete")
	}
	return out.NodeName, nil
}

// SavePairing records a pairing in worker.yaml and the token file, so it lasts
// across restarts. A running worker notices the change and connects.
//
// Only the lines for gateway_url, node.name and token_file are touched, so
// comments and everything else in worker.yaml stay as they were. If the result
// wouldn't load, worker.yaml is left alone.
func SavePairing(configPath, gatewayURL, nodeName, token string) error {
	original, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	updated, tokenFile, err := withPairing(original, gatewayURL, nodeName)
	if err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}
	tokenPath := resolvePath(filepath.Dir(configPath), tokenFile)

	// The token goes first: a worker.yaml that names a gateway with no token only
	// leaves the machine unpaired, which is where it started.
	if err := writePrivateFile(tokenPath, []byte(token+"\n")); err != nil {
		return fmt.Errorf("save the token: %w", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		return err
	}
	tmp := configPath + ".pairing"
	if err := os.WriteFile(tmp, updated, info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := LoadManifest(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("pairing would leave %s invalid, so it wasn't changed: %s", configPath, strings.ReplaceAll(err.Error(), tmp, configPath))
	}
	if err := os.Rename(tmp, configPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writePrivateFile writes a file only its owner can read. An existing file is
// written in place, which on Windows keeps the access list its installer set.
func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// withPairing returns worker.yaml with the gateway and name set, and the token
// file it names.
func withPairing(original []byte, gatewayURL, nodeName string) (updated []byte, tokenFile string, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		return nil, "", fmt.Errorf("it isn't valid YAML: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, "", errors.New("it must be a list of settings")
	}
	root := doc.Content[0]
	lines := strings.Split(string(original), "\n")

	// Each edit replaces or adds whole lines. They're applied from the bottom up,
	// so earlier line numbers stay right.
	type edit struct {
		line    int // 1-based; the line to replace, or to insert before.
		insert  bool
		content string
	}
	var edits []edit
	set := func(parent *yaml.Node, key, value, indent string) error {
		k, v := mappingEntry(parent, key)
		line := fmt.Sprintf("%s%s: %s", indent, key, yamlScalar(value))
		if k == nil {
			at := len(lines) + 1
			if len(parent.Content) > 0 {
				at = parent.Content[0].Line
			}
			edits = append(edits, edit{line: at, insert: true, content: line})
			return nil
		}
		if v.Kind != yaml.ScalarNode || v.Line != k.Line || strings.Contains(v.Value, "\n") {
			return fmt.Errorf("%s is written in a way this can't change; set it by hand to %s", key, value)
		}
		// Keep the key's own indentation, and any comment after the value.
		current := lines[k.Line-1]
		line = current[:k.Column-1] + key + ": " + yamlScalar(value)
		if v.LineComment != "" {
			line += " " + v.LineComment
		}
		edits = append(edits, edit{line: k.Line, content: line})
		return nil
	}

	if err := set(root, "gateway_url", gatewayURL, ""); err != nil {
		return nil, "", err
	}
	tokenFile = "token"
	if _, v := mappingEntry(root, "token_file"); v != nil && v.Value != "" {
		tokenFile = v.Value
	} else if err := set(root, "token_file", tokenFile, ""); err != nil {
		return nil, "", err
	}
	_, node := mappingEntry(root, "node")
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, "", errors.New("it has no node: section to put this machine's name in")
	}
	indent := "  "
	if len(node.Content) > 0 {
		indent = strings.Repeat(" ", node.Content[0].Column-1)
	}
	if node.Style&yaml.FlowStyle != 0 {
		return nil, "", fmt.Errorf("node: is written on one line, which this can't change; set its name by hand to %s", nodeName)
	}
	if err := set(node, "name", nodeName, indent); err != nil {
		return nil, "", err
	}

	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			if edits[j].line > edits[i].line {
				edits[i], edits[j] = edits[j], edits[i]
			}
		}
	}
	for _, e := range edits {
		index := min(e.line-1, len(lines))
		if e.insert {
			lines = append(lines[:index], append([]string{e.content}, lines[index:]...)...)
		} else {
			lines[index] = e.content
		}
	}
	return []byte(strings.Join(lines, "\n")), tokenFile, nil
}

func mappingEntry(mapping *yaml.Node, key string) (k, v *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// yamlScalar writes a value so YAML reads it back as the same text.
func yamlScalar(value string) string {
	out, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%q", value)
	}
	return strings.TrimSpace(string(out))
}
