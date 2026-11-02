package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"mactunnel/internal/protocol"
	"mactunnel/internal/schema"
)

const defaultTimeout = 30 * time.Minute
const defaultUploadWindow = 6 * time.Hour

var (
	capabilityNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	outputNameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	placeholderRE    = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.]+)\s*\}\}`)
	inputVarRE       = regexp.MustCompile(`^input\.[A-Za-z0-9_]+$`)
	inputFileRE      = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
)

// Manifest is the worker's local configuration. It is the only place that says
// what this machine will run: the gateway can't add to it or change it.
type Manifest struct {
	GatewayURL        string       `yaml:"gateway_url"`
	TokenFile         string       `yaml:"token_file"`
	StateDir          string       `yaml:"state_dir"`
	AllowInsecureHTTP bool         `yaml:"allow_insecure_http"`
	UploadRetryWindow string       `yaml:"upload_retry_window"`
	Node              NodeConfig   `yaml:"node"`
	Capabilities      []Capability `yaml:"capabilities"`

	uploadWindow time.Duration
	byName       map[string]*Capability
}

// NodeConfig describes this machine.
type NodeConfig struct {
	Name      string            `yaml:"name"`
	Labels    map[string]string `yaml:"labels"`
	Resources map[string]int    `yaml:"resources"`
	MaxJobs   int               `yaml:"max_jobs"`
}

// ProgressConfig extracts progress from command output. The regex may use the
// named groups percent, current, total and message.
type ProgressConfig struct {
	Regex string `yaml:"regex"`
}

// Capability is one service this machine offers.
type Capability struct {
	Name        string            `yaml:"name"`
	Adapter     string            `yaml:"adapter"`
	Command     []string          `yaml:"command"`
	Stdin       string            `yaml:"stdin"`
	Env         map[string]string `yaml:"env"`
	InputSchema map[string]any    `yaml:"input_schema"`
	InputFiles  []string          `yaml:"input_files"`
	Outputs     map[string]string `yaml:"outputs"`
	Progress    *ProgressConfig   `yaml:"progress"`
	Requires    map[string]int    `yaml:"requires"`
	Timeout     string            `yaml:"timeout"`
	RetryIfLost bool              `yaml:"retry_if_lost"`

	// The http and http_async adapters use these instead of Command.
	Request     *RequestTemplate `yaml:"request"`
	Response    *ResponseConfig  `yaml:"response"`
	Async       *AsyncConfig     `yaml:"async"`
	AllowRemote bool             `yaml:"allow_remote"`

	timeout    time.Duration
	schemaJSON []byte
	compiled   *jsonschema.Schema
	progressRE *regexp.Regexp
	executable string
}

// LoadManifest reads and validates a manifest. Problems are reported at startup
// so a typo can't turn into a capability that fails on every job.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var m Manifest
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := m.validate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func (m *Manifest) validate(baseDir string) error {
	if m.GatewayURL == "" {
		return errors.New("gateway_url is required")
	}
	parsed, err := url.Parse(m.GatewayURL)
	if err != nil {
		return fmt.Errorf("gateway_url: %w", err)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !m.AllowInsecureHTTP && !isLoopback(parsed.Hostname()) {
			return errors.New("gateway_url must use https, because plain http would expose the node token and job data")
		}
	default:
		return errors.New("gateway_url must start with https://")
	}
	m.GatewayURL = strings.TrimRight(m.GatewayURL, "/")

	if m.Node.Name == "" {
		return errors.New("node.name is required")
	}
	if m.Node.MaxJobs == 0 {
		m.Node.MaxJobs = 1
	}
	if m.Node.MaxJobs < 0 {
		return errors.New("node.max_jobs must be positive")
	}
	if m.StateDir == "" {
		return errors.New("state_dir is required")
	}
	m.StateDir = resolvePath(baseDir, m.StateDir)
	if m.TokenFile != "" {
		m.TokenFile = resolvePath(baseDir, m.TokenFile)
	}

	m.uploadWindow = defaultUploadWindow
	if m.UploadRetryWindow != "" {
		if m.uploadWindow, err = time.ParseDuration(m.UploadRetryWindow); err != nil {
			return fmt.Errorf("upload_retry_window: %w", err)
		}
	}

	if len(m.Capabilities) == 0 {
		return errors.New("at least one capability is required")
	}
	m.byName = make(map[string]*Capability, len(m.Capabilities))
	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		if err := c.validate(m.Node.Resources); err != nil {
			return fmt.Errorf("capability %q: %w", c.Name, err)
		}
		if m.byName[c.Name] != nil {
			return fmt.Errorf("capability %q is declared twice", c.Name)
		}
		m.byName[c.Name] = c
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func resolvePath(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func (c *Capability) validate(resources map[string]int) error {
	if !capabilityNameRE.MatchString(c.Name) {
		return errors.New("name must be lowercase letters, digits, '.', '_' or '-'")
	}
	switch c.Adapter {
	case adapterCLI, adapterHTTP, adapterHTTPAsync:
	default:
		return fmt.Errorf("adapter %q is not supported (use cli, http or http_async)", c.Adapter)
	}
	if err := c.rejectFields(); err != nil {
		return err
	}
	seenFiles := make(map[string]bool, len(c.InputFiles))
	for _, field := range c.InputFiles {
		if !inputFileRE.MatchString(field) {
			return fmt.Errorf("input file name %q is invalid", field)
		}
		if seenFiles[field] {
			return fmt.Errorf("input file %q is declared twice", field)
		}
		seenFiles[field] = true
	}
	var err error
	if c.isService() {
		err = c.validateService(seenFiles)
	} else {
		err = c.validateCommand(seenFiles)
	}
	if err != nil {
		return err
	}

	if c.InputSchema == nil {
		return errors.New("input_schema is required")
	}
	if c.schemaJSON, err = json.Marshal(c.InputSchema); err != nil {
		return fmt.Errorf("input_schema: %w", err)
	}
	if c.compiled, err = schema.Compile(c.schemaJSON); err != nil {
		return fmt.Errorf("input_schema: %w", err)
	}

	for name, rel := range c.Outputs {
		if !outputNameRE.MatchString(name) {
			return fmt.Errorf("output name %q is invalid", name)
		}
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("output %q must be a relative path inside the job's working directory", name)
		}
		if !outputNameRE.MatchString(strings.TrimPrefix(filepath.Ext(rel), ".")) && filepath.Ext(rel) != "" {
			return fmt.Errorf("output %q has an invalid file extension", name)
		}
	}
	if err := c.validateResponse(); err != nil {
		return err
	}

	if c.Progress != nil {
		if c.progressRE, err = regexp.Compile(c.Progress.Regex); err != nil {
			return fmt.Errorf("progress.regex: %w", err)
		}
	}

	for name, need := range c.Requires {
		if need < 1 {
			return fmt.Errorf("requires.%s must be positive", name)
		}
		if need > resources[name] {
			return fmt.Errorf("requires %d of %q, but node.resources provides %d", need, name, resources[name])
		}
	}

	c.timeout = defaultTimeout
	if c.Timeout != "" {
		if c.timeout, err = time.ParseDuration(c.Timeout); err != nil || c.timeout <= 0 {
			return fmt.Errorf("timeout %q is not a positive duration", c.Timeout)
		}
	}
	return nil
}

func (c *Capability) validateCommand(files map[string]bool) error {
	if len(c.Command) == 0 {
		return errors.New("command is required")
	}
	if placeholderRE.MatchString(c.Command[0]) {
		return errors.New("the program in command[0] cannot contain placeholders")
	}
	// Resolve now, so a missing tool stops the capability being advertised.
	executable, err := exec.LookPath(c.Command[0])
	if err != nil {
		return fmt.Errorf("cannot find program: %w", err)
	}
	if c.executable, err = filepath.Abs(executable); err != nil {
		return err
	}
	if err := checkExecutable(c.executable, runtime.GOOS); err != nil {
		return err
	}
	for _, arg := range c.Command[1:] {
		if err := checkPlaceholders(arg, files); err != nil {
			return err
		}
	}
	return checkPlaceholders(c.Stdin, files)
}

// checkExecutable refuses Windows batch files. Windows runs them through cmd.exe,
// whatever the caller asks for, and cmd.exe reparses the arguments. That would
// let job input run commands, which this worker promises can't happen.
func checkExecutable(path, goos string) error {
	if goos != "windows" {
		return nil
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".bat", ".cmd":
		return errors.New("batch files can't be used as a program, because cmd.exe would reparse job input as commands; run the tool directly")
	}
	return nil
}

func checkPlaceholders(tpl string, files map[string]bool) error {
	for _, match := range placeholderRE.FindAllStringSubmatch(tpl, -1) {
		if field, isFile := strings.CutPrefix(match[1], "files."); isFile {
			if !files[field] {
				return fmt.Errorf("placeholder %q needs %q listed in input_files", match[0], field)
			}
			continue
		}
		if match[1] != "workdir" && !inputVarRE.MatchString(match[1]) {
			return fmt.Errorf("unknown placeholder %q (use {{workdir}}, {{input.field}} or {{files.field}})", match[0])
		}
	}
	return nil
}

// templateVars holds the values placeholders can refer to.
type templateVars struct {
	workdir string
	input   map[string]any
	files   map[string]string // Input file field to its local path.
	// remoteID is a service's own ID for this job, once http_async has one.
	remoteID string
}

// render fills a template in one pass, so placeholder syntax inside a value is
// never expanded again.
func render(tpl string, vars templateVars) (string, error) {
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(tpl, func(match string) string {
		name := placeholderRE.FindStringSubmatch(match)[1]
		if name == "workdir" {
			return vars.workdir
		}
		if name == remoteIDVar {
			return vars.remoteID
		}
		if field, isFile := strings.CutPrefix(name, "files."); isFile {
			path, ok := vars.files[field]
			if !ok && firstErr == nil {
				firstErr = fmt.Errorf("input file %q is missing", field)
			}
			return path
		}
		value, err := inputText(strings.TrimPrefix(name, "input."), vars.input)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return value
	})
	return out, firstErr
}

func inputText(field string, input map[string]any) (string, error) {
	value, ok := input[field]
	if !ok {
		return "", fmt.Errorf("input field %q is missing", field)
	}
	switch v := value.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		return fmt.Sprint(v), nil
	default:
		return "", fmt.Errorf("input field %q must be a string, number or boolean", field)
	}
}

// renderCommand builds argv. A value starting with '-' in the leading position
// of an argument is refused, because the program would read it as an option.
func renderCommand(args []string, vars templateVars) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		rendered, err := render(arg, vars)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(rendered, "-") {
			if loc := placeholderRE.FindStringSubmatchIndex(arg); loc != nil && loc[0] == 0 && strings.HasPrefix(arg[loc[2]:loc[3]], "input.") {
				return nil, fmt.Errorf("the value for argument %q must not start with '-'", arg)
			}
		}
		out = append(out, rendered)
	}
	return out, nil
}

// protocolCapabilities is what the worker advertises in its hello message.
func (m *Manifest) protocolCapabilities() []protocol.Capability {
	out := make([]protocol.Capability, 0, len(m.Capabilities))
	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		out = append(out, protocol.Capability{
			Name:           c.Name,
			InputSchema:    c.schemaJSON,
			Requires:       c.Requires,
			TimeoutSeconds: int64(c.timeout.Seconds()),
			RetryIfLost:    c.RetryIfLost,
			InputFiles:     c.InputFiles,
		})
	}
	return out
}

// ReadToken loads the node token from FLEET_NODE_TOKEN or token_file. Like SSH
// keys, a token file must be private to its owner.
func (m *Manifest) ReadToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("FLEET_NODE_TOKEN")); token != "" {
		return token, nil
	}
	if m.TokenFile == "" {
		return "", errors.New("set token_file in the manifest or FLEET_NODE_TOKEN in the environment")
	}
	info, err := os.Stat(m.TokenFile)
	if err != nil {
		return "", err
	}
	// Windows has no mode bits to check. Its installer restricts the file instead.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is accessible by other users; run: chmod 600 %s", m.TokenFile, m.TokenFile)
	}
	raw, err := os.ReadFile(m.TokenFile)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s is empty", m.TokenFile)
	}
	return token, nil
}
