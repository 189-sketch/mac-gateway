package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	GatewayURL        string     `yaml:"gateway_url"`
	TokenFile         string     `yaml:"token_file"`
	StateDir          string     `yaml:"state_dir"`
	AllowInsecureHTTP bool       `yaml:"allow_insecure_http"`
	UploadRetryWindow string     `yaml:"upload_retry_window"`
	LogLevel          string     `yaml:"log_level"`
	Node              NodeConfig `yaml:"node"`
	// Capabilities and Tools are two names for the same list, and may be combined.
	Capabilities []Capability `yaml:"capabilities"`
	Tools        []Capability `yaml:"tools"`
	// ToolsDir holds one tool per file, such as those made by `fleet-worker tools
	// new`. It defaults to the tools folder next to this file.
	ToolsDir string `yaml:"tools_dir"`

	// ToolErrors lists tool files that were skipped because they aren't valid.
	ToolErrors []ToolFileError `yaml:"-"`
	// Disabled names the tools set to enabled: false.
	Disabled []string `yaml:"-"`

	uploadWindow time.Duration
	byName       map[string]*Capability
	toolsPath    string
}

// ToolFileError is a tool file that couldn't be used. The worker runs without it,
// so one bad file doesn't stop the machine's other tools.
type ToolFileError struct {
	Path string
	Err  error
}

func (e ToolFileError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

// ToolsPath is the folder tool files are read from.
func (m *Manifest) ToolsPath() string { return m.toolsPath }

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

// Capability is one tool this machine offers.
type Capability struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Enabled false keeps a tool in its file without offering it.
	Enabled     *bool                 `yaml:"enabled,omitempty"`
	Adapter     string                `yaml:"adapter"`
	Command     []string              `yaml:"command,omitempty"`
	Args        []ArgSpec             `yaml:"args,omitempty"`
	Stdin       string                `yaml:"stdin,omitempty"`
	Cwd         string                `yaml:"cwd,omitempty"`
	Env         map[string]string     `yaml:"env,omitempty"`
	InputSchema map[string]any        `yaml:"input_schema"`
	InputFiles  []string              `yaml:"input_files,omitempty"`
	Outputs     map[string]OutputSpec `yaml:"outputs,omitempty"`
	Result      *ResultConfig         `yaml:"result,omitempty"`
	// SuccessExitCodes lists the exit codes that mean the job worked. It defaults
	// to 0 alone. xcodebuild, for one, exits with 65 when tests fail.
	SuccessExitCodes []int           `yaml:"success_exit_codes,omitempty"`
	Progress         *ProgressConfig `yaml:"progress,omitempty"`
	Requires         map[string]int  `yaml:"requires,omitempty"`
	Timeout          string          `yaml:"timeout,omitempty"`
	RetryIfLost      bool            `yaml:"retry_if_lost,omitempty"`

	// The http and http_async adapters use these instead of Command.
	Request     *RequestTemplate `yaml:"request"`
	Response    *ResponseConfig  `yaml:"response"`
	Async       *AsyncConfig     `yaml:"async"`
	AllowRemote bool             `yaml:"allow_remote"`
	Check       *CheckConfig     `yaml:"check"`
	Test        *TestConfig      `yaml:"test"`

	testInput    []byte
	fields       schemaFields
	cwd          string
	successCodes []int
	timeout      time.Duration
	schemaJSON   []byte
	compiled     *jsonschema.Schema
	progressRE   *regexp.Regexp
	executable   string
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
	m.Capabilities = append(m.Capabilities, m.Tools...)
	m.Tools = nil
	if err := m.validate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// isEnabled is false only for a tool that says enabled: false.
func (c *Capability) isEnabled() bool { return c.Enabled == nil || *c.Enabled }

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

	if _, err := ParseLogLevel(m.LogLevel); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}

	m.uploadWindow = defaultUploadWindow
	if m.UploadRetryWindow != "" {
		if m.uploadWindow, err = time.ParseDuration(m.UploadRetryWindow); err != nil {
			return fmt.Errorf("upload_retry_window: %w", err)
		}
	}

	// Tools in this file must all be valid, like the rest of it.
	enabled := make([]Capability, 0, len(m.Capabilities))
	m.byName = make(map[string]*Capability, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if !c.isEnabled() {
			m.Disabled = append(m.Disabled, c.Name)
			continue
		}
		if err := c.validate(baseDir, m.Node.Resources); err != nil {
			return fmt.Errorf("capability %q: %w", c.Name, err)
		}
		if m.byName[c.Name] != nil {
			return fmt.Errorf("capability %q is declared twice", c.Name)
		}
		enabled = append(enabled, c)
		m.byName[c.Name] = &enabled[len(enabled)-1]
	}

	m.toolsPath = resolvePath(baseDir, "tools")
	if m.ToolsDir != "" {
		m.toolsPath = resolvePath(baseDir, m.ToolsDir)
	}
	files, err := m.loadToolFiles(m.Node.Resources)
	if err != nil {
		return fmt.Errorf("tools_dir: %w", err)
	}
	enabled = append(enabled, files...)

	// byName must point into the final slice, which appending may have moved.
	m.Capabilities = enabled
	m.byName = make(map[string]*Capability, len(enabled))
	for i := range m.Capabilities {
		m.byName[m.Capabilities[i].Name] = &m.Capabilities[i]
	}
	return nil
}

// loadToolFiles reads one tool from each .yaml or .yml file in the tools folder.
// A file that isn't valid is skipped and listed in ToolErrors.
func (m *Manifest) loadToolFiles(resources map[string]int) ([]Capability, error) {
	entries, err := os.ReadDir(m.toolsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Capability
	for _, entry := range entries {
		name := entry.Name()
		ext := filepath.Ext(name)
		if entry.IsDir() || strings.HasPrefix(name, ".") || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		path := filepath.Join(m.toolsPath, name)
		c, err := readToolFile(path)
		if err == nil && !c.isEnabled() {
			m.Disabled = append(m.Disabled, c.Name)
			continue
		}
		if err == nil {
			err = c.validate(m.toolsPath, resources)
		}
		if err == nil && m.byName[c.Name] != nil {
			err = fmt.Errorf("a tool named %q is already declared", c.Name)
		}
		if err != nil {
			m.ToolErrors = append(m.ToolErrors, ToolFileError{Path: path, Err: err})
			continue
		}
		out = append(out, *c)
		m.byName[c.Name] = &out[len(out)-1]
	}
	return out, nil
}

// readToolFile decodes one tool, refusing fields it doesn't know.
func readToolFile(path string) (*Capability, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var c Capability
	if err := decoder.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &c, nil
}

// TestConfig says how to show a capability working from start to finish, when
// someone asks the gateway to test it. Unlike a check, it runs as a real job.
type TestConfig struct {
	Input any `yaml:"input"`
}

// maxTestInputBytes keeps a test's input small enough to send on every connection.
const maxTestInputBytes = 64 << 10

func (c *Capability) validateTest() error {
	if c.Test == nil {
		return nil
	}
	if len(c.InputFiles) > 0 {
		return errors.New("test can't be used with input_files, because a test has no files to send")
	}
	input := c.Test.Input
	if input == nil {
		input = map[string]any{}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("test input: %w", err)
	}
	if len(raw) > maxTestInputBytes {
		return fmt.Errorf("test input is larger than %d bytes", maxTestInputBytes)
	}
	if err := schema.Validate(c.compiled, raw); err != nil {
		return fmt.Errorf("test input doesn't match input_schema: %w", err)
	}
	c.testInput = raw
	return nil
}

// ParseLogLevel reads a level name. Empty means info.
func ParseLogLevel(text string) (slog.Level, error) {
	switch text {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%q must be debug, info, warn or error", text)
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

func (c *Capability) validate(baseDir string, resources map[string]int) error {
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

	if err := c.validateDeclaration(baseDir, seenFiles); err != nil {
		return err
	}

	for name, out := range c.Outputs {
		if !outputNameRE.MatchString(name) {
			return fmt.Errorf("output name %q is invalid", name)
		}
		rel := out.Path
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("output %q must be a relative path inside the job's working directory", name)
		}
		if out.Archive != "" && out.Archive != archiveZip {
			return fmt.Errorf("output %q: archive %q must be zip", name, out.Archive)
		}
		if out.Archive == "" && !outputNameRE.MatchString(strings.TrimPrefix(filepath.Ext(rel), ".")) && filepath.Ext(rel) != "" {
			return fmt.Errorf("output %q has an invalid file extension", name)
		}
	}
	if err := c.validateResponse(); err != nil {
		return err
	}
	if err := c.validateCheck(); err != nil {
		return err
	}
	if err := c.validateTest(); err != nil {
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
		if match[1] == "item" {
			return errors.New("{{item}} can only be used in an args item with each")
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
	// item is the current list item in an args item that uses each.
	item    string
	hasItem bool
	// allowEnv lets {{env.NAME}} read this worker's environment, which only env
	// values may do.
	allowEnv bool
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
		if name == "item" && vars.hasItem {
			return vars.item
		}
		if sub := envVarRE.FindStringSubmatch(name); sub != nil && vars.allowEnv {
			value, ok := os.LookupEnv(sub[1])
			if (!ok || sub[1] == "FLEET_NODE_TOKEN") && firstErr == nil {
				firstErr = fmt.Errorf("environment variable %s isn't set", sub[1])
				return ""
			}
			return value
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
		rendered, err := renderArg(arg, vars)
		if err != nil {
			return nil, err
		}
		out = append(out, rendered)
	}
	return out, nil
}

func renderArg(arg string, vars templateVars) (string, error) {
	rendered, err := render(arg, vars)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rendered, "-") {
		if loc := placeholderRE.FindStringSubmatchIndex(arg); loc != nil && loc[0] == 0 {
			if name := arg[loc[2]:loc[3]]; strings.HasPrefix(name, "input.") || name == "item" {
				return "", fmt.Errorf("the value for argument %q must not start with '-'", arg)
			}
		}
	}
	return rendered, nil
}

// protocolCapabilities is what the worker advertises in its hello message.
func (m *Manifest) protocolCapabilities() []protocol.Capability {
	out := make([]protocol.Capability, 0, len(m.Capabilities))
	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		out = append(out, protocol.Capability{
			Name:           c.Name,
			Description:    c.Description,
			InputSchema:    c.schemaJSON,
			Requires:       c.Requires,
			TimeoutSeconds: int64(c.timeout.Seconds()),
			RetryIfLost:    c.RetryIfLost,
			InputFiles:     c.InputFiles,
			TestInput:      c.testInput,
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
