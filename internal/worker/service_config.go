package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	adapterCLI       = "cli"
	adapterHTTP      = "http"
	adapterHTTPAsync = "http_async"

	defaultPollEvery  = 2 * time.Second
	defaultPollGiveUp = 2 * time.Minute
	remoteIDVar       = "remote.id"
)

var (
	wholePlaceholderRE = regexp.MustCompile(`^\{\{\s*([A-Za-z0-9_.]+)\s*\}\}$`)
	envPlaceholderRE   = regexp.MustCompile(`\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	headerNameRE       = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	responseFieldRE    = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
)

// RequestTemplate is one HTTP request a capability may make. Its scheme and host
// are fixed here in the manifest, so job input can never choose where it goes.
type RequestTemplate struct {
	Method   string            `yaml:"method"`
	URL      string            `yaml:"url"`
	Headers  map[string]string `yaml:"headers"`
	JSON     any               `yaml:"json"`
	Body     string            `yaml:"body"`
	BodyFile string            `yaml:"body_file"`

	bodyField string // The input file named by BodyFile.
}

// ResponseConfig says what to keep from a service's response.
type ResponseConfig struct {
	File        string `yaml:"file"`
	Result      string `yaml:"result"`
	ResultField string `yaml:"result_field"`
}

// PollConfig is how to find out whether a service's job has finished.
type PollConfig struct {
	RequestTemplate `yaml:",inline"`
	Every           string   `yaml:"every"`
	GiveUpAfter     string   `yaml:"give_up_after"`
	StatusField     string   `yaml:"status_field"`
	Succeeded       []string `yaml:"succeeded"`
	Failed          []string `yaml:"failed"`
	PercentField    string   `yaml:"percent_field"`
	PercentScale    float64  `yaml:"percent_scale"`
	MessageField    string   `yaml:"message_field"`
	ErrorField      string   `yaml:"error_field"`

	every  time.Duration
	giveUp time.Duration
}

// AsyncConfig describes a service that answers with its own job ID, then runs
// the work in the background.
type AsyncConfig struct {
	JobIDField string           `yaml:"job_id_field"`
	Poll       PollConfig       `yaml:"poll"`
	Fetch      *RequestTemplate `yaml:"fetch"`
	Cancel     *RequestTemplate `yaml:"cancel"`
}

func (c *Capability) isService() bool {
	return c.Adapter == adapterHTTP || c.Adapter == adapterHTTPAsync
}

// rejectFields reports the first set field that doesn't belong to this adapter,
// so a misplaced setting can't be silently ignored.
func (c *Capability) rejectFields() error {
	if c.isService() {
		switch {
		case len(c.Command) > 0:
			return fmt.Errorf("command doesn't apply to adapter %s", c.Adapter)
		case c.Stdin != "":
			return fmt.Errorf("stdin doesn't apply to adapter %s", c.Adapter)
		case len(c.Env) > 0:
			return fmt.Errorf("env doesn't apply to adapter %s", c.Adapter)
		case c.Progress != nil:
			return fmt.Errorf("progress doesn't apply to adapter %s; http_async reports progress through poll", c.Adapter)
		}
		return nil
	}
	switch {
	case c.Request != nil:
		return errors.New("request applies only to the http and http_async adapters")
	case c.Response != nil:
		return errors.New("response applies only to the http and http_async adapters")
	case c.Async != nil:
		return errors.New("async applies only to the http_async adapter")
	case c.AllowRemote:
		return errors.New("allow_remote applies only to the http and http_async adapters")
	}
	return nil
}

func (c *Capability) validateService(files map[string]bool) error {
	if c.Request == nil {
		return errors.New("request is required")
	}
	if err := c.Request.validate("request", c.AllowRemote, submitScope(files)); err != nil {
		return err
	}
	if c.Adapter == adapterHTTP {
		if c.Async != nil {
			return errors.New("async applies only to the http_async adapter")
		}
		return nil
	}
	if c.Async == nil {
		return errors.New("async is required for adapter http_async")
	}
	return c.Async.validate(c.AllowRemote)
}

func (a *AsyncConfig) validate(allowRemote bool) error {
	if !responseFieldRE.MatchString(a.JobIDField) {
		return errors.New("async.job_id_field must name a field in the service's response")
	}
	p := &a.Poll
	if err := p.RequestTemplate.validate("async.poll", allowRemote, followUpScope); err != nil {
		return err
	}
	if !responseFieldRE.MatchString(p.StatusField) {
		return errors.New("async.poll.status_field must name a field in the service's response")
	}
	if len(p.Succeeded) == 0 {
		return errors.New("async.poll.succeeded must list at least one status")
	}
	for _, status := range p.Succeeded {
		if slices.Contains(p.Failed, status) {
			return fmt.Errorf("async.poll lists status %q as both succeeded and failed", status)
		}
	}
	for name, field := range map[string]string{"percent_field": p.PercentField, "message_field": p.MessageField, "error_field": p.ErrorField} {
		if field != "" && !responseFieldRE.MatchString(field) {
			return fmt.Errorf("async.poll.%s isn't a valid field path", name)
		}
	}
	if p.PercentScale < 0 {
		return errors.New("async.poll.percent_scale must be positive")
	}
	if p.PercentScale == 0 {
		p.PercentScale = 1
	}
	var err error
	if p.every, err = positiveDuration(p.Every, defaultPollEvery); err != nil {
		return fmt.Errorf("async.poll.every: %w", err)
	}
	if p.giveUp, err = positiveDuration(p.GiveUpAfter, defaultPollGiveUp); err != nil {
		return fmt.Errorf("async.poll.give_up_after: %w", err)
	}
	if a.Fetch != nil {
		if err := a.Fetch.validate("async.fetch", allowRemote, followUpScope); err != nil {
			return err
		}
	}
	if a.Cancel != nil {
		if err := a.Cancel.validate("async.cancel", allowRemote, followUpScope); err != nil {
			return err
		}
	}
	return nil
}

func positiveDuration(text string, fallback time.Duration) (time.Duration, error) {
	if text == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(text)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%q is not a positive duration", text)
	}
	return parsed, nil
}

// requestScope says which placeholders a request may use.
type requestScope struct {
	inURL  func(name string) bool
	inBody func(name string) bool
	hint   string
}

// submitScope applies to the request that starts the work. A local service can
// read input files and write results directly, so bodies may name their paths.
func submitScope(files map[string]bool) requestScope {
	isInput := func(name string) bool { return inputVarRE.MatchString(name) }
	return requestScope{
		inURL: isInput,
		inBody: func(name string) bool {
			if field, isFile := strings.CutPrefix(name, "files."); isFile {
				return files[field]
			}
			return name == "workdir" || name == "input" || isInput(name)
		},
		hint: "use {{input.field}}, and in a body also {{input}}, {{workdir}} or {{files.field}}",
	}
}

// followUpScope applies to requests about a job already running. These can also
// happen after a worker restart, when the job's input is no longer available.
var followUpScope = requestScope{
	inURL:  func(name string) bool { return name == remoteIDVar },
	inBody: func(name string) bool { return name == remoteIDVar },
	hint:   "use {{remote.id}}",
}

func (t *RequestTemplate) validate(where string, allowRemote bool, scope requestScope) error {
	if t.Method == "" {
		t.Method = "GET"
		if t.JSON != nil || t.Body != "" || t.BodyFile != "" {
			t.Method = "POST"
		}
	}
	t.Method = strings.ToUpper(t.Method)
	if !slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, t.Method) {
		return fmt.Errorf("%s.method %q is not supported", where, t.Method)
	}
	if err := checkServiceURL(t.URL, allowRemote); err != nil {
		return fmt.Errorf("%s.url: %w", where, err)
	}
	for _, match := range placeholderRE.FindAllStringSubmatch(t.URL, -1) {
		if !scope.inURL(match[1]) {
			return fmt.Errorf("%s.url: placeholder %q isn't allowed here (%s)", where, match[0], scope.hint)
		}
	}

	bodies := 0
	for _, set := range []bool{t.JSON != nil, t.Body != "", t.BodyFile != ""} {
		if set {
			bodies++
		}
	}
	if bodies > 1 {
		return fmt.Errorf("%s can set only one of json, body and body_file", where)
	}
	if t.Body != "" {
		if err := checkBodyString(t.Body, scope); err != nil {
			return fmt.Errorf("%s.body: %w", where, err)
		}
	}
	if t.JSON != nil {
		if _, err := json.Marshal(t.JSON); err != nil {
			return fmt.Errorf("%s.json must use only text keys: %w", where, err)
		}
		if err := checkBodyJSON(t.JSON, scope); err != nil {
			return fmt.Errorf("%s.json: %w", where, err)
		}
	}
	if t.BodyFile != "" {
		match := wholePlaceholderRE.FindStringSubmatch(t.BodyFile)
		if match == nil || !strings.HasPrefix(match[1], "files.") || !scope.inBody(match[1]) {
			return fmt.Errorf("%s.body_file must be a single {{files.field}} listed in input_files", where)
		}
		t.bodyField = strings.TrimPrefix(match[1], "files.")
	}

	resolved, err := resolveHeaders(t.Headers)
	if err != nil {
		return fmt.Errorf("%s.headers: %w", where, err)
	}
	t.Headers = resolved
	return nil
}

// checkServiceURL requires a fixed scheme and host, so that input can only ever
// fill in the path and query, and limits the host to this machine by default.
func checkServiceURL(raw string, allowRemote bool) error {
	if raw == "" {
		return errors.New("is required")
	}
	// This comes first, because a placeholder in a port wouldn't parse as a URL.
	scheme, rest, found := strings.Cut(raw, "://")
	authority, _, _ := strings.Cut(rest, "/")
	authority, _, _ = strings.Cut(authority, "?")
	if !found {
		return errors.New("must start with http:// or https://")
	}
	if placeholderRE.MatchString(scheme) || placeholderRE.MatchString(authority) {
		return errors.New("can't use placeholders in the scheme or host")
	}
	// Placeholders aren't valid URL syntax, so check a copy with them filled in.
	parsed, err := url.Parse(placeholderRE.ReplaceAllString(raw, "x"))
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("must start with http:// or https://")
	}
	if parsed.Host == "" {
		return errors.New("must include a host")
	}
	if parsed.User != nil {
		return errors.New("can't include a username or password; use headers")
	}
	if strings.Contains(raw, "#") {
		return errors.New("can't include a fragment")
	}
	if !allowRemote && !isLoopback(parsed.Hostname()) {
		return fmt.Errorf("host %q isn't on this machine; set allow_remote: true if this service is meant to be reached over the network", parsed.Hostname())
	}
	return nil
}

func checkBodyString(text string, scope requestScope) error {
	for _, match := range placeholderRE.FindAllStringSubmatch(text, -1) {
		if match[1] == "input" {
			return fmt.Errorf("%q works only as a whole value in json", match[0])
		}
		if !scope.inBody(match[1]) {
			return fmt.Errorf("placeholder %q isn't allowed here (%s)", match[0], scope.hint)
		}
	}
	return nil
}

func checkBodyJSON(value any, scope requestScope) error {
	switch v := value.(type) {
	case string:
		if match := wholePlaceholderRE.FindStringSubmatch(v); match != nil && match[1] == "input" && scope.inBody("input") {
			return nil
		}
		return checkBodyString(v, scope)
	case map[string]any:
		for key, item := range v {
			if placeholderRE.MatchString(key) {
				return fmt.Errorf("key %q can't contain a placeholder", key)
			}
			if err := checkBodyJSON(item, scope); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := checkBodyJSON(item, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveHeaders fills in {{env.NAME}} now, so a missing secret stops the worker
// at startup rather than failing each job. Job input can't reach a header.
func resolveHeaders(headers map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		if !headerNameRE.MatchString(name) {
			return nil, fmt.Errorf("%q isn't a valid header name", name)
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "transfer-encoding":
			return nil, fmt.Errorf("%s can't be set here", name)
		}
		var missing error
		resolved := envPlaceholderRE.ReplaceAllStringFunc(value, func(match string) string {
			variable := envPlaceholderRE.FindStringSubmatch(match)[1]
			if variable == "FLEET_NODE_TOKEN" {
				missing = errors.New("the node token can't be sent to a service")
				return ""
			}
			text, ok := os.LookupEnv(variable)
			if !ok && missing == nil {
				missing = fmt.Errorf("environment variable %s isn't set", variable)
			}
			return text
		})
		if missing != nil {
			return nil, fmt.Errorf("%s: %w", name, missing)
		}
		// Checked on the template, because a secret could contain any characters.
		if rest := envPlaceholderRE.ReplaceAllString(value, ""); placeholderRE.MatchString(rest) {
			return nil, fmt.Errorf("%s: only {{env.NAME}} placeholders can be used in a header", name)
		}
		if strings.ContainsAny(resolved, "\r\n") {
			return nil, fmt.Errorf("%s: a header value can't contain a line break", name)
		}
		out[name] = resolved
	}
	return out, nil
}

// validateResponse runs after outputs are checked, because a saved response is
// delivered by naming its file in outputs.
func (c *Capability) validateResponse() error {
	r := c.Response
	if r == nil {
		return nil
	}
	if r.File != "" && r.Result != "" {
		return errors.New("response can set file or result, not both")
	}
	if r.File != "" {
		delivered := false
		for _, rel := range c.Outputs {
			delivered = delivered || rel == r.File
		}
		if !delivered {
			return fmt.Errorf("response.file %q must also be listed in outputs, or it would never be delivered", r.File)
		}
	}
	switch r.Result {
	case "", "json":
	case "text":
		if r.ResultField != "" {
			return errors.New("response.result_field needs result: json")
		}
	default:
		return fmt.Errorf("response.result %q must be json or text", r.Result)
	}
	if r.ResultField != "" {
		if r.Result != "json" {
			return errors.New("response.result_field needs result: json")
		}
		if !responseFieldRE.MatchString(r.ResultField) {
			return errors.New("response.result_field isn't a valid field path")
		}
	}
	return nil
}
