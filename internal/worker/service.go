package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"mactunnel/internal/protocol"
)

const (
	// maxResultBytes limits a response kept as the job's inline result. A larger
	// one belongs in a file, through response.file.
	maxResultBytes = 1 << 20
	// maxEncodedResultBytes keeps the final event under the gateway's message
	// limit. Escaping can grow JSON, and an event the gateway can't accept would
	// be resent forever.
	maxEncodedResultBytes = 3 << 19
	maxPollBytes          = 16 << 20
	maxRemoteIDBytes      = 1024
	maxServiceErrorBytes  = 2 << 10
	remoteCancelTimeout   = 10 * time.Second
)

// newServiceClient makes requests to local services. It ignores proxy settings,
// because these services are reached directly, and doesn't follow a redirect to
// another host, because the manifest decides which hosts a capability talks to.
func newServiceClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// callService runs a job through an HTTP service and returns its inline result.
func (w *Worker) callService(parent context.Context, a *attempt, c *Capability, vars templateVars, work string) (json.RawMessage, *protocol.Failure) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	w.emit(a, protocol.EventStarted, nil)

	var result json.RawMessage
	var failure *protocol.Failure
	if c.Adapter == adapterHTTPAsync {
		result, failure = w.runAsync(ctx, a, c, vars, work)
	} else {
		result, failure = w.runSync(ctx, c, vars, work)
	}
	if failure != nil && ctx.Err() != nil && parent.Err() == nil {
		return nil, &protocol.Failure{Code: protocol.CodeTimedOut, Message: fmt.Sprintf("the service took longer than the %s timeout", c.timeout)}
	}
	return result, failure
}

func (w *Worker) runSync(ctx context.Context, c *Capability, vars templateVars, work string) (json.RawMessage, *protocol.Failure) {
	resp, failure := w.sendRequest(ctx, c.Request, vars)
	if failure != nil {
		return nil, failure
	}
	defer resp.Body.Close()
	return captureResponse(c.Response, resp.Body, work)
}

// runAsync submits the work, follows it to the end and collects its result. If
// this attempt gives up first, for any reason, it asks the service to stop.
func (w *Worker) runAsync(ctx context.Context, a *attempt, c *Capability, vars templateVars, work string) (json.RawMessage, *protocol.Failure) {
	resp, failure := w.sendRequest(ctx, c.Request, vars)
	if failure != nil {
		return nil, failure
	}
	body, err := readLimited(resp.Body, maxPollBytes)
	resp.Body.Close()
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeServiceUnavailable, Message: "couldn't read the service's reply: " + err.Error()}
	}
	doc, err := decodeJSON(body)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: "the service's reply isn't JSON"}
	}
	remoteID, ok := fieldText(doc, c.Async.JobIDField)
	if !ok || remoteID == "" || len(remoteID) > maxRemoteIDBytes {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: fmt.Sprintf("the service's reply has no usable %q", c.Async.JobIDField)}
	}
	vars.remoteID = remoteID
	w.log.Info("service accepted the job", "attempt", a.meta.AttemptID, "capability", c.Name, "remote_id", remoteID)
	if _, err := renderURL(c.Async.Poll.URL, vars); err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: "the service's job ID can't be used in its status URL"}
	}
	w.recordRemote(a, remoteID)

	final, failure, remoteFailed := w.pollUntilDone(ctx, a, c, vars)
	if failure != nil {
		// A job the service says has failed has nothing left to stop. One this
		// attempt walked away from may still be using the machine.
		if !remoteFailed {
			w.cancelRemote(c, remoteID)
		}
		return nil, failure
	}
	if c.Async.Fetch == nil {
		return captureResponse(c.Response, bytes.NewReader(final), work)
	}
	resp, failure = w.sendRequest(ctx, c.Async.Fetch, vars)
	if failure != nil {
		return nil, failure
	}
	defer resp.Body.Close()
	return captureResponse(c.Response, resp.Body, work)
}

// pollUntilDone returns the body of the response that reported success. A brief
// outage isn't a failure, since the service may just be restarting. remoteFailed
// means the service itself said the job failed.
func (w *Worker) pollUntilDone(ctx context.Context, a *attempt, c *Capability, vars templateVars) (final []byte, failure *protocol.Failure, remoteFailed bool) {
	p := &c.Async.Poll
	var firstFailure time.Time
	var lastProgress protocol.Progress
	var lastEmit time.Time
	for {
		var body []byte
		body, failure = w.pollOnce(ctx, p, vars)
		switch {
		case ctx.Err() != nil:
			return nil, &protocol.Failure{Code: protocol.CodeServiceUnavailable, Message: "stopped waiting for the service"}, false
		case failure != nil && failure.Code != protocol.CodeServiceUnavailable:
			return nil, failure, false
		case failure != nil:
			if firstFailure.IsZero() {
				firstFailure = time.Now()
			}
			if time.Since(firstFailure) > p.giveUp {
				return nil, &protocol.Failure{Code: protocol.CodeServiceUnavailable, Message: fmt.Sprintf("the service didn't answer for %s: %s", p.giveUp, failure.Message)}, false
			}
			w.log.Warn("service check failed, will retry", "attempt", a.meta.AttemptID, "error", failure.Message)
		default:
			firstFailure = time.Time{}
			doc, err := decodeJSON(body)
			if err != nil {
				return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: "the service's status reply isn't JSON"}, false
			}
			// A status that isn't listed, or isn't there yet, means keep waiting.
			status, _ := fieldText(doc, p.StatusField)
			if slices.Contains(p.Succeeded, status) {
				return body, nil, false
			}
			if slices.Contains(p.Failed, status) {
				return nil, &protocol.Failure{Code: protocol.CodeServiceError, Message: remoteFailureMessage(p, doc, status)}, true
			}
			if progress := pollProgress(p, doc); progress != nil && time.Since(lastEmit) >= progressInterval && !sameProgress(*progress, lastProgress) {
				w.emit(a, protocol.EventProgress, *progress)
				lastProgress, lastEmit = *progress, time.Now()
			}
		}
		select {
		case <-time.After(p.every):
		case <-ctx.Done():
		}
	}
}

func (w *Worker) pollOnce(ctx context.Context, p *PollConfig, vars templateVars) ([]byte, *protocol.Failure) {
	resp, failure := w.sendRequest(ctx, &p.RequestTemplate, vars)
	if failure != nil {
		return nil, failure
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxPollBytes)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeServiceUnavailable, Message: err.Error()}
	}
	return body, nil
}

func remoteFailureMessage(p *PollConfig, doc any, status string) string {
	if p.ErrorField != "" {
		if message, ok := fieldText(doc, p.ErrorField); ok && message != "" {
			return truncateText(message, maxServiceErrorBytes)
		}
	}
	return fmt.Sprintf("the service reported status %q", truncateText(status, 200))
}

func pollProgress(p *PollConfig, doc any) *protocol.Progress {
	progress := protocol.Progress{Stage: "running"}
	found := false
	if p.PercentField != "" {
		if text, ok := fieldText(doc, p.PercentField); ok {
			if value, err := strconv.ParseFloat(text, 64); err == nil {
				if value *= p.PercentScale; value >= 0 && value <= 100 {
					progress.Percent = &value
					found = true
				}
			}
		}
	}
	if p.MessageField != "" {
		if text, ok := fieldText(doc, p.MessageField); ok && text != "" {
			progress.Message = truncateText(text, 500)
			found = true
		}
	}
	if !found {
		return nil
	}
	return &progress
}

func sameProgress(a, b protocol.Progress) bool {
	if a.Message != b.Message || (a.Percent == nil) != (b.Percent == nil) {
		return false
	}
	return a.Percent == nil || *a.Percent == *b.Percent
}

// cancelRemote makes a best effort to stop the service's job. It's only a
// courtesy to free the machine, so a failure is logged and goes no further.
func (w *Worker) cancelRemote(c *Capability, remoteID string) {
	if c.Async == nil || c.Async.Cancel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteCancelTimeout)
	defer cancel()
	resp, failure := w.sendRequest(ctx, c.Async.Cancel, templateVars{remoteID: remoteID})
	if failure != nil {
		w.log.Warn("could not cancel the service's job", "capability", c.Name, "error", failure.Message)
		return
	}
	resp.Body.Close()
}

// sendRequest renders and sends a request. It returns a response only for a 2xx
// status. The caller closes the body.
func (w *Worker) sendRequest(ctx context.Context, t *RequestTemplate, vars templateVars) (*http.Response, *protocol.Failure) {
	req, err := buildRequest(ctx, t, vars)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeInvalidInput, Message: err.Error()}
	}
	// t.URL is the manifest's template, so job input never reaches the log this way.
	sent := time.Now()
	resp, err := w.serviceClient.Do(req)
	if err != nil {
		w.log.Debug("service request failed", "method", t.Method, "url", t.URL, "duration", time.Since(sent).Round(time.Millisecond).String())
		// The details name this machine's address and ports, which are no business
		// of whoever submitted the job, so they stay in the local log. A url.Error
		// also repeats the full URL, which can contain a secret.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		if ctx.Err() == nil {
			w.log.Warn("couldn't reach a service", "error", err)
		}
		return nil, &protocol.Failure{Code: protocol.CodeServiceUnavailable, Message: "couldn't reach the service; the worker's log has the details"}
	}
	w.log.Debug("service request", "method", t.Method, "url", t.URL, "status", resp.StatusCode, "duration", time.Since(sent).Round(time.Millisecond).String())
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxServiceErrorBytes))
	message := fmt.Sprintf("the service answered %d: %s", resp.StatusCode, strings.ToValidUTF8(string(body), "?"))
	return nil, &protocol.Failure{Code: statusFailureCode(resp.StatusCode), Message: strings.TrimSpace(message)}
}

// statusFailureCode separates a service that's temporarily unable to answer,
// which is worth trying again, from one that refused this particular job.
func statusFailureCode(status int) string {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return protocol.CodeServiceUnavailable
	}
	return protocol.CodeServiceError
}

func buildRequest(ctx context.Context, t *RequestTemplate, vars templateVars) (*http.Request, error) {
	target, err := renderURL(t.URL, vars)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	var length int64
	contentType := ""
	switch {
	case t.JSON != nil:
		rendered, omit, err := renderJSON(t.JSON, vars)
		if err != nil {
			return nil, err
		}
		if omit {
			return nil, errors.New("the request body depends on an input field that is missing")
		}
		encoded, err := json.Marshal(rendered)
		if err != nil {
			return nil, err
		}
		body, length, contentType = bytes.NewReader(encoded), int64(len(encoded)), "application/json"
	case t.Body != "":
		rendered, err := render(t.Body, vars)
		if err != nil {
			return nil, err
		}
		body, length = strings.NewReader(rendered), int64(len(rendered))
	case t.bodyField != "":
		file, err := os.Open(vars.files[t.bodyField])
		if err != nil {
			return nil, fmt.Errorf("input file %q: %w", t.bodyField, err)
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("input file %q: %w", t.bodyField, err)
		}
		// The HTTP transport closes the file when it finishes with the body.
		body, length, contentType = file, info.Size(), "application/octet-stream"
	}
	req, err := http.NewRequestWithContext(ctx, t.Method, target, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = length
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, value := range t.Headers {
		req.Header.Set(name, value)
	}
	return req, nil
}

// renderURL fills in a URL, escaping each value for where it lands. A value used
// in the path has to stay one segment: otherwise it could reach a different
// endpoint of the service, which the manifest never offered.
func renderURL(tpl string, vars templateVars) (string, error) {
	query := strings.IndexByte(tpl, '?')
	var out strings.Builder
	last := 0
	for _, loc := range placeholderRE.FindAllStringSubmatchIndex(tpl, -1) {
		out.WriteString(tpl[last:loc[0]])
		last = loc[1]
		value, err := render(tpl[loc[0]:loc[1]], vars)
		if err != nil {
			return "", err
		}
		if query >= 0 && loc[0] > query {
			out.WriteString(url.QueryEscape(value))
			continue
		}
		if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
			return "", fmt.Errorf("the value for %s can't be used in a URL path", tpl[loc[0]:loc[1]])
		}
		out.WriteString(url.PathEscape(value))
	}
	out.WriteString(tpl[last:])
	return out.String(), nil
}

// renderJSON fills in a JSON template. A string that's only a placeholder takes
// the value with its own type, so a number stays a number and an object stays an
// object, and escaping is left to the encoder, so input can't change the
// document's structure. A key whose optional input field is missing is left out.
func renderJSON(value any, vars templateVars) (rendered any, omit bool, err error) {
	switch v := value.(type) {
	case string:
		if match := wholePlaceholderRE.FindStringSubmatch(v); match != nil {
			if match[1] == "input" {
				return vars.input, false, nil
			}
			if field, isInput := strings.CutPrefix(match[1], "input."); isInput {
				item, ok := vars.input[field]
				return item, !ok, nil
			}
		}
		text, err := render(v, vars)
		return text, false, err
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			renderedItem, omitItem, err := renderJSON(item, vars)
			if err != nil {
				return nil, false, err
			}
			if !omitItem {
				out[key] = renderedItem
			}
		}
		return out, false, nil
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			renderedItem, omitItem, err := renderJSON(item, vars)
			if err != nil {
				return nil, false, err
			}
			if omitItem {
				return nil, false, fmt.Errorf("the template item %v needs an input field that is missing", item)
			}
			out = append(out, renderedItem)
		}
		return out, false, nil
	default:
		return value, false, nil
	}
}

// captureResponse keeps what the manifest asked for from a response.
func captureResponse(r *ResponseConfig, body io.Reader, work string) (json.RawMessage, *protocol.Failure) {
	switch {
	case r == nil:
		return nil, nil
	case r.File != "":
		if err := saveResponse(body, work, r.File); err != nil {
			return nil, &protocol.Failure{Code: protocol.CodeServiceUnavailable, Message: "couldn't save the service's reply: " + err.Error()}
		}
		return nil, nil
	case r.Result != "":
		return readResult(r, body)
	}
	return nil, nil
}

func saveResponse(body io.Reader, work, rel string) error {
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	file, err := root.Create(rel)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, body); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func readResult(r *ResponseConfig, body io.Reader) (json.RawMessage, *protocol.Failure) {
	raw, err := readLimited(body, maxResultBytes)
	if errors.Is(err, errTooLarge) {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: fmt.Sprintf("the reply is larger than the %d bytes a result can hold; save it with response.file instead", maxResultBytes)}
	}
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeServiceUnavailable, Message: "couldn't read the service's reply: " + err.Error()}
	}
	var result json.RawMessage
	if r.Result == "text" {
		if result, err = json.Marshal(strings.ToValidUTF8(string(raw), "�")); err != nil {
			return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: err.Error()}
		}
	} else {
		doc, err := decodeJSON(raw)
		if err != nil {
			return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: "the service's reply isn't JSON"}
		}
		if r.ResultField != "" {
			var ok bool
			if doc, ok = fieldAt(doc, r.ResultField); !ok {
				return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: fmt.Sprintf("the service's reply has no %q", r.ResultField)}
			}
		}
		if result, err = json.Marshal(doc); err != nil {
			return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: err.Error()}
		}
	}
	if len(result) > maxEncodedResultBytes {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: "the reply is too large to send as a result once encoded; save it with response.file instead"}
	}
	return result, nil
}

var errTooLarge = errors.New("the reply is too large")

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errTooLarge
	}
	return raw, nil
}

// decodeJSON keeps numbers exact and rejects anything after the first value.
func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc any
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("extra data after the JSON value")
	}
	return doc, nil
}

// fieldAt follows a dotted path through objects and arrays, such as "data.0.id".
func fieldAt(doc any, path string) (any, bool) {
	current := doc
	for _, part := range strings.Split(path, ".") {
		switch v := current.(type) {
		case map[string]any:
			next, ok := v[part]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(v) {
				return nil, false
			}
			current = v[index]
		default:
			return nil, false
		}
	}
	return current, true
}

// fieldText reads a field that holds a string, number or boolean as text.
func fieldText(doc any, path string) (string, bool) {
	value, ok := fieldAt(doc, path)
	if !ok {
		return "", false
	}
	switch v := value.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "")
}
