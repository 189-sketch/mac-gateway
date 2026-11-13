package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The connection test does what an agent does: it opens the gateway's MCP
// endpoint over the internet, lists the tools, and calls one of this machine's.
// It uses a key the gateway makes for the test, which lasts two minutes and can
// reach only this machine's tools, so no agent key ever has to be on a worker.

const (
	mcpTestProtocol = "2025-11-25"
	mcpTestTimeout  = 90 * time.Second
)

// MCPTestStep is one thing the connection test tried.
type MCPTestStep struct {
	Name string `json:"name"`
	// State is ok, failed or skipped.
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	Millis int64  `json:"ms"`
}

// MCPTestReport is the outcome of a connection test.
type MCPTestReport struct {
	OK     bool          `json:"ok"`
	MCPURL string        `json:"mcp_url,omitempty"`
	Steps  []MCPTestStep `json:"steps"`
}

type mcpTestClient struct {
	w      *Worker
	url    string
	key    string
	nextID int
}

// call sends one JSON-RPC request and returns its result.
func (c *mcpTestClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		message["params"] = params
	}
	notification := strings.HasPrefix(method, "notifications/")
	if !notification {
		message["id"] = c.nextID
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", mcpTestProtocol)
	}
	resp, err := c.w.httpClient.Do(req)
	if err != nil {
		return nil, explainDialError(c.url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if notification {
		return nil, nil
	}
	var answer struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("the gateway answered %s with something that isn't MCP", resp.Status)
	}
	if answer.Error != nil {
		return nil, errors.New(answer.Error.Message)
	}
	if resp.StatusCode != http.StatusOK || answer.Result == nil {
		return nil, fmt.Errorf("the gateway answered %s", resp.Status)
	}
	return answer.Result, nil
}

// TestMCP checks, from this machine, that an agent could reach its tools through
// the gateway's MCP endpoint. Each step is reported whether or not it worked,
// and a step that fails ends the test.
func (w *Worker) TestMCP(parent context.Context) MCPTestReport {
	ctx, cancel := context.WithTimeout(parent, mcpTestTimeout)
	defer cancel()
	report := MCPTestReport{Steps: []MCPTestStep{}}
	failed := false
	step := func(name string, run func() (detail string, skipped bool, err error)) {
		if failed {
			return
		}
		started := time.Now()
		detail, skipped, err := run()
		s := MCPTestStep{Name: name, State: "ok", Detail: detail, Millis: time.Since(started).Milliseconds()}
		switch {
		case err != nil:
			s.State, s.Detail, failed = "failed", err.Error(), true
		case skipped:
			s.State = "skipped"
		}
		report.Steps = append(report.Steps, s)
	}

	client := &mcpTestClient{w: w}
	step("Reach the gateway", func() (string, bool, error) {
		gatewayURL, token := w.connection()
		if gatewayURL == "" || token == "" {
			return "", false, errors.New("this machine isn't paired with a gateway yet")
		}
		var out struct {
			MCPURL string `json:"mcp_url"`
			Key    string `json:"key"`
		}
		callCtx, cancel := context.WithTimeout(ctx, PairTimeout)
		defer cancel()
		status, refused, err := pairCall(callCtx, http.MethodPost, gatewayURL+"/v1/worker/mcp-test", token, struct{}{}, &out)
		if err != nil {
			return "", false, err
		}
		if refused != nil {
			if status == http.StatusUnauthorized {
				return "", false, errRejected
			}
			if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
				return "", false, errors.New("the gateway is older than this worker and can't run this test; upgrade it by running its install command again")
			}
			return "", false, refusal(status, refused, gatewayURL)
		}
		// The address in the reply is the one agents are given. The test itself
		// goes to the address this machine already trusts with its token.
		report.MCPURL = out.MCPURL
		client.url, client.key = gatewayURL+"/mcp", out.Key
		return "The gateway at " + hostOf(gatewayURL) + " knows this machine.", false, nil
	})

	step("Open an MCP session", func() (string, bool, error) {
		raw, err := client.call(ctx, "initialize", map[string]any{
			"protocolVersion": mcpTestProtocol,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]string{"name": "fleet-worker-connection-test", "version": "1"},
		})
		if err != nil {
			return "", false, err
		}
		var out struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.ProtocolVersion == "" {
			return "", false, errors.New("the gateway's answer to initialize wasn't valid")
		}
		_, _ = client.call(ctx, "notifications/initialized", nil)
		return fmt.Sprintf("%s %s, MCP %s.", out.ServerInfo.Name, out.ServerInfo.Version, out.ProtocolVersion), false, nil
	})

	// The gateway names MCP tools after capabilities, and gives the capability
	// as each tool's title.
	toolFor := map[string]string{}
	step("List this machine's tools", func() (string, bool, error) {
		raw, err := client.call(ctx, "tools/list", map[string]any{})
		if err != nil {
			return "", false, err
		}
		var out struct {
			Tools []struct {
				Name  string `json:"name"`
				Title string `json:"title"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", false, errors.New("the gateway's tool list wasn't valid")
		}
		var names []string
		for _, t := range out.Tools {
			toolFor[t.Title] = t.Name
			names = append(names, t.Title)
		}
		sort.Strings(names)
		if len(names) == 0 {
			if len(w.config().Capabilities) == 0 {
				return "This machine has no tools switched on yet. Add one on the Tools tab.", true, nil
			}
			if w.connectedAt.Load() == 0 {
				return "", false, errors.New("the gateway lists none of this machine's tools, because the worker isn't connected to it right now")
			}
			return "", false, errors.New("the gateway lists none of this machine's tools")
		}
		shown := names
		if len(shown) > 8 {
			shown = append(append([]string{}, shown[:8]...), fmt.Sprintf("and %d more", len(names)-8))
		}
		return fmt.Sprintf("An agent would see %s: %s.", plural(len(names), "tool"), strings.Join(shown, ", ")), false, nil
	})

	step("Call a tool through MCP", func() (string, bool, error) {
		m := w.config()
		var candidates []*Capability
		w.mu.Lock()
		for i := range m.Capabilities {
			c := &m.Capabilities[i]
			if _, down := w.notReady[c.Name]; !down && len(c.testInput) > 0 && toolFor[c.Name] != "" {
				candidates = append(candidates, c)
			}
		}
		w.mu.Unlock()
		if len(candidates) == 0 {
			return "None of this machine's ready tools has a test input, so there's nothing safe to call. Add one in a tool's Test section.", true, nil
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
		c := candidates[0]
		raw, err := client.call(ctx, "tools/call", map[string]any{"name": toolFor[c.Name], "arguments": json.RawMessage(c.testInput)})
		if err != nil {
			return "", false, err
		}
		var out struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Structured struct {
				Status string `json:"status"`
			} `json:"structuredContent"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", false, errors.New("the gateway's answer to the call wasn't valid")
		}
		var texts []string
		for _, block := range out.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				texts = append(texts, strings.TrimSpace(block.Text))
			}
		}
		said := truncateText(strings.Join(texts, "\n"), 600)
		if out.IsError {
			return "", false, fmt.Errorf("%s was called, but it failed:\n%s", c.Name, said)
		}
		if out.Structured.Status != "" && out.Structured.Status != "succeeded" {
			return fmt.Sprintf("%s was called and is still running, so the connection works.\n%s", c.Name, said), false, nil
		}
		return fmt.Sprintf("%s ran on this machine and answered:\n%s", c.Name, said), false, nil
	})

	report.OK = !failed
	return report
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
