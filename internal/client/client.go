// Package client talks to the gateway's agent API.
package client

import (
	"bufio"
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
	"strconv"
	"strings"
	"time"

	"mactunnel/internal/transfer"
)

const (
	maxEventBytes     = 4 << 20
	streamIdleTimeout = 60 * time.Second // The gateway pings every 15 seconds.
	maxStreamBackoff  = 30 * time.Second
	requestTries      = 4
)

// Error is a failure reported by the gateway.
type Error struct {
	Status    int    `json:"-"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Progress is a job's latest reported progress.
type Progress struct {
	Stage   string   `json:"stage,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
	Current string   `json:"current,omitempty"`
	Total   string   `json:"total,omitempty"`
	Message string   `json:"message,omitempty"`
}

// Artifact is a result file.
type Artifact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// Job is the gateway's view of a job.
type Job struct {
	ID         string     `json:"id"`
	Capability string     `json:"capability"`
	Status     string     `json:"status"`
	Terminal   bool       `json:"terminal"`
	Test       bool       `json:"test,omitempty"`
	Attempts   int        `json:"attempts"`
	Node       string     `json:"node,omitempty"`
	Progress   *Progress  `json:"progress,omitempty"`
	Error      *Error     `json:"error,omitempty"`
	Artifacts  []Artifact `json:"artifacts"`
	// Result is a small inline answer from an HTTP service, if it gives one.
	Result json.RawMessage `json:"result,omitempty"`
	// ExitCode is the command's exit code, for a job that ran one.
	ExitCode   *int    `json:"exit_code,omitempty"`
	Output     string  `json:"output,omitempty"`
	Events     []Event `json:"events,omitempty"`
	CreatedAt  string  `json:"created_at"`
	StartedAt  string  `json:"started_at,omitempty"`
	FinishedAt string  `json:"finished_at,omitempty"`
}

// Event is a stored fact about a job.
type Event struct {
	ID      int64           `json:"id"`
	Attempt string          `json:"attempt_id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	At      string          `json:"at"`
}

// File describes an uploaded input file.
type File struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ExpiresAt string `json:"expires_at"`
}

// NewJob is a job request.
type NewJob struct {
	Capability     string            `json:"capability"`
	Input          json.RawMessage   `json:"input,omitempty"`
	Files          map[string]string `json:"files,omitempty"`
	Selector       map[string]string `json:"selector,omitempty"`
	QueueTimeout   string            `json:"queue_timeout,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// NewTest asks the gateway to test a capability. With no node, it tests every
// connected machine that's ready and declares a test.
type NewTest struct {
	Node           string `json:"node,omitempty"`
	QueueTimeout   string `json:"queue_timeout,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// TestRun is a test the gateway started, as a job on one machine.
type TestRun struct {
	Node string `json:"node"`
	Job  *Job   `json:"job"`
}

// TestSkip is a machine the gateway couldn't test, and why.
type TestSkip struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// TestResult is what the gateway started.
type TestResult struct {
	Tests   []TestRun  `json:"tests"`
	Skipped []TestSkip `json:"skipped"`
}

// Client is an authenticated connection to one gateway.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
	// Logf reports retries and reconnects. It may be nil.
	Logf func(format string, args ...any)
}

// New creates a client. It refuses plain HTTP to anything but this machine,
// because that would expose the API key and job data.
func New(baseURL, key string, allowInsecure bool) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("the gateway URL is not set (FLEET_URL)")
	}
	if key == "" {
		return nil, errors.New("the API key is not set (FLEET_API_KEY)")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid gateway URL: %w", err)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !allowInsecure && !isLoopback(parsed.Hostname()) {
			return nil, errors.New("the gateway URL must use https, because plain http would expose the API key")
		}
	default:
		return nil, errors.New("the gateway URL must start with https://")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), key: key, http: &http.Client{Transport: transport}}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	return req, nil
}

func readError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != nil {
		body.Error.Status = resp.StatusCode
		return body.Error
	}
	return &Error{
		Status:    resp.StatusCode,
		Code:      "HTTP_" + strconv.Itoa(resp.StatusCode),
		Message:   strings.TrimSpace(string(raw)),
		Retryable: resp.StatusCode >= 500,
	}
}

// isTransient says whether a request is worth repeating as is.
func isTransient(err error) bool {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Retryable
	}
	return !errors.Is(err, context.Canceled)
}

// requestJSON sends a request and decodes its response. Network failures are
// retried, so it must only be used for requests that are safe to repeat.
func (c *Client) requestJSON(ctx context.Context, method, path string, body []byte, out any) error {
	var lastErr error
	for try := 0; try < requestTries; try++ {
		if try > 0 {
			c.logf("retrying after: %v", lastErr)
			if err := sleep(ctx, time.Duration(1<<(try-1))*time.Second); err != nil {
				return err
			}
		}
		req, err := c.newRequest(ctx, method, path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		lastErr = c.do(req, out)
		if lastErr == nil || !isTransient(lastErr) || ctx.Err() != nil {
			return lastErr
		}
	}
	return lastErr
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Capabilities returns the capabilities this key may use, as the gateway sent them.
func (c *Client) Capabilities(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.requestJSON(ctx, http.MethodGet, "/v1/capabilities", nil, &out)
	return out, err
}

// CreateJob submits a job. Set an idempotency key if the job has side effects:
// a retry after a lost response then returns the same job.
func (c *Client) CreateJob(ctx context.Context, job NewJob) (*Job, error) {
	body, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	var out Job
	if err := c.requestJSON(ctx, http.MethodPost, "/v1/jobs", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TestCapability starts a capability's test using the input its worker declares.
func (c *Client) TestCapability(ctx context.Context, capability string, test NewTest) (*TestResult, error) {
	body, err := json.Marshal(test)
	if err != nil {
		return nil, err
	}
	var out TestResult
	path := "/v1/capabilities/" + url.PathEscape(capability) + "/test"
	if err := c.requestJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetJob returns a job, waiting up to waitSeconds for it to finish.
func (c *Client) GetJob(ctx context.Context, id string, waitSeconds int, withEvents bool) (*Job, error) {
	query := url.Values{}
	if waitSeconds > 0 {
		query.Set("wait", strconv.Itoa(waitSeconds))
	}
	if withEvents {
		query.Set("include", "events")
	}
	var out Job
	path := "/v1/jobs/" + url.PathEscape(id) + "?" + query.Encode()
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelJob stops a job. Cancelling a finished job changes nothing.
func (c *Client) CancelJob(ctx context.Context, id string) (*Job, error) {
	var out Job
	if err := c.requestJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/cancel", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadFile stores a local file as a job input.
func (c *Client) UploadFile(ctx context.Context, path string) (*File, error) {
	size, sum, err := transfer.HashFile(path)
	if err != nil {
		return nil, err
	}
	name := uploadName(filepath.Base(path))
	var lastErr error
	for try := 0; try < requestTries; try++ {
		if try > 0 {
			c.logf("retrying upload after: %v", lastErr)
			if err := sleep(ctx, time.Duration(1<<(try-1))*time.Second); err != nil {
				return nil, err
			}
		}
		var out File
		lastErr = c.uploadOnce(ctx, path, name, sum, size, &out)
		if lastErr == nil {
			return &out, nil
		}
		if !isTransient(lastErr) || ctx.Err() != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (c *Client) uploadOnce(ctx context.Context, path, name, sum string, size int64, out *File) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	// The HTTP transport closes the file when it finishes with the body.
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := &transfer.StallReader{Reader: file, Closer: file}
	stop := transfer.WatchStall(uploadCtx, body, cancel)
	defer stop()
	req, err := c.newRequest(uploadCtx, http.MethodPost, "/v1/files?name="+url.QueryEscape(name), body)
	if err != nil {
		file.Close()
		return err
	}
	req.ContentLength = size
	if size == 0 {
		body.Close()
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Content-SHA256", sum)
	return c.do(req, out)
}

// uploadName maps a local file name onto the characters the gateway accepts.
func uploadName(base string) string {
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if name == "" {
		name = "file"
	}
	return name[max(0, len(name)-128):]
}

// Watch follows a job until it finishes, reconnecting as needed, and returns its
// final state. Each stored event reaches onEvent exactly once, and onStatus gets
// every status change this connection sees. Either callback may be nil.
func (c *Client) Watch(ctx context.Context, id string, after int64, onEvent func(Event), onStatus func(*Job)) (*Job, error) {
	cursor := after
	backoff := time.Second
	for {
		job, progressed, err := c.streamOnce(ctx, id, &cursor, onEvent, onStatus)
		if job != nil {
			return job, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var apiErr *Error
		if errors.As(err, &apiErr) && !apiErr.Retryable {
			return nil, err
		}
		if progressed {
			backoff = time.Second
		}
		c.logf("connection to the gateway lost, reconnecting in %s: %v", backoff, err)
		if err := sleep(ctx, backoff); err != nil {
			return nil, err
		}
		backoff = min(backoff*2, maxStreamBackoff)
	}
}

// streamOnce reads one SSE connection. It returns the job if the stream ended
// cleanly, and whether it received anything at all.
func (c *Client) streamOnce(ctx context.Context, id string, cursor *int64, onEvent func(Event), onStatus func(*Job)) (*Job, bool, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := c.newRequest(streamCtx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/events", nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if *cursor > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(*cursor, 10))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, readError(resp)
	}

	// A connection can die without either end noticing, so silence past the
	// gateway's ping interval counts as a lost connection.
	idle := time.AfterFunc(streamIdleTimeout, cancel)
	defer idle.Stop()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	var eventType, data string
	progressed := false
	for scanner.Scan() {
		idle.Reset(streamIdleTimeout)
		progressed = true
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && eventType != "":
			job, err := c.dispatch(eventType, data, cursor, onEvent, onStatus)
			if job != nil || err != nil {
				return job, progressed, err
			}
			eventType, data = "", ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, progressed, err
	}
	return nil, progressed, errors.New("the stream ended before the job finished")
}

func (c *Client) dispatch(eventType, data string, cursor *int64, onEvent func(Event), onStatus func(*Job)) (*Job, error) {
	switch eventType {
	case "end", "status":
		var job Job
		if err := json.Unmarshal([]byte(data), &job); err != nil {
			return nil, fmt.Errorf("bad %s event: %w", eventType, err)
		}
		if onStatus != nil {
			onStatus(&job)
		}
		if eventType == "end" {
			return &job, nil
		}
	default:
		var ev Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil, fmt.Errorf("bad event: %w", err)
		}
		// Advancing the cursor here means a reconnect can't repeat this event.
		if ev.ID > *cursor {
			*cursor = ev.ID
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}
	return nil, nil
}

// Download saves a result file to dest, verified against its checksum. It writes
// to dest+".part" first, so dest only ever holds a complete file, and resumes
// from that file if a connection drops.
func (c *Client) Download(ctx context.Context, artifact Artifact, dest string) error {
	part := dest + ".part"
	var lastErr error
	for try := 0; try < requestTries; try++ {
		if try > 0 {
			c.logf("retrying download after: %v", lastErr)
			if err := sleep(ctx, time.Duration(1<<(try-1))*time.Second); err != nil {
				return err
			}
		}
		lastErr = transfer.Download(ctx, c.http, transfer.DownloadRequest{
			URL:    c.baseURL + artifact.URL,
			Token:  c.key,
			Dest:   part,
			Size:   artifact.Size,
			SHA256: artifact.SHA256,
		})
		if lastErr == nil {
			return os.Rename(part, dest)
		}
		var status *transfer.StatusError
		if (errors.As(lastErr, &status) && status.Code < 500) || ctx.Err() != nil {
			return lastErr
		}
	}
	return lastErr
}
