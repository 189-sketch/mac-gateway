package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file holds the parts of a tool declaration that say how a command is
// called: optional arguments, the folder it runs in, its environment, where its
// result comes from and which exit codes count as success.

const maxDescriptionBytes = 2000

var (
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	envVarRE   = regexp.MustCompile(`^env\.([A-Za-z_][A-Za-z0-9_]*)$`)
	fieldRefRE = regexp.MustCompile(`^(?:input\.)?([A-Za-z0-9_]+)$`)
)

// decodeStrict decodes a mapping, refusing keys it doesn't know. yaml.v3 doesn't
// pass the outer decoder's strictness on to custom unmarshalers.
func decodeStrict(node *yaml.Node, out any, allowed ...string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping", node.Line)
	}
	for i := 0; i < len(node.Content); i += 2 {
		if key := node.Content[i].Value; !slices.Contains(allowed, key) {
			return fmt.Errorf("line %d: field %s not found (use %s)", node.Content[i].Line, key, strings.Join(allowed, ", "))
		}
	}
	return node.Decode(out)
}

// --- Arguments ---

// ArgSpec is one item in args. A plain string is always passed, like an item in
// command. A mapping is passed only when the input it needs is there:
//
//	{ flag: --model, value: "{{input.model}}" }  adds both when model is given
//	{ flag: --fast, when: input.fast }          adds the flag when fast is true
//	{ flag: --file, each: input.paths }          adds the pair for each list item
type ArgSpec struct {
	Text  string
	Flag  string
	Value string
	When  string
	Each  string

	plain     bool
	whenField string
	eachField string
}

func (s *ArgSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		s.plain = true
		return node.Decode(&s.Text)
	}
	var raw struct {
		Flag  string `yaml:"flag"`
		Value string `yaml:"value"`
		When  string `yaml:"when"`
		Each  string `yaml:"each"`
	}
	if err := decodeStrict(node, &raw, "flag", "value", "when", "each"); err != nil {
		return fmt.Errorf("args: %w", err)
	}
	s.Flag, s.Value, s.When, s.Each = raw.Flag, raw.Value, raw.When, raw.Each
	return nil
}

// MarshalYAML writes a plain item back as a string.
func (s ArgSpec) MarshalYAML() (any, error) {
	if s.plain {
		return s.Text, nil
	}
	out := map[string]string{}
	for key, value := range map[string]string{"flag": s.Flag, "value": s.Value, "when": s.When, "each": s.Each} {
		if value != "" {
			out[key] = value
		}
	}
	return out, nil
}

func (s *ArgSpec) validate(files map[string]bool) error {
	if s.plain {
		return checkPlaceholders(s.Text, files)
	}
	if s.Flag == "" && s.Value == "" {
		return errors.New("an item needs flag, value or both")
	}
	if placeholderRE.MatchString(s.Flag) {
		return fmt.Errorf("flag %q can't contain placeholders; put them in value", s.Flag)
	}
	if s.When != "" && s.Each != "" {
		return errors.New("an item can use when or each, not both")
	}
	if s.When != "" {
		match := fieldRefRE.FindStringSubmatch(s.When)
		if match == nil {
			return fmt.Errorf("when %q must name an input field, such as input.fast", s.When)
		}
		s.whenField = match[1]
	}
	if s.Each != "" {
		match := fieldRefRE.FindStringSubmatch(s.Each)
		if match == nil {
			return fmt.Errorf("each %q must name an input field, such as input.files", s.Each)
		}
		s.eachField = match[1]
		if s.Value == "" {
			s.Value = "{{item}}"
		}
	}
	for _, match := range placeholderRE.FindAllStringSubmatch(s.Value, -1) {
		if match[1] == "item" {
			if s.eachField == "" {
				return errors.New("{{item}} can only be used with each")
			}
			continue
		}
		if err := checkPlaceholders(match[0], files); err != nil {
			return err
		}
	}
	return nil
}

// inputRefs lists the input fields a template uses.
func inputRefs(tpl string) []string {
	var fields []string
	for _, match := range placeholderRE.FindAllStringSubmatch(tpl, -1) {
		if field, ok := strings.CutPrefix(match[1], "input."); ok {
			fields = append(fields, field)
		}
	}
	return fields
}

// renderArgs builds the arguments that args adds after command.
func renderArgs(specs []ArgSpec, vars templateVars) ([]string, error) {
	var out []string
	for i := range specs {
		s := &specs[i]
		switch {
		case s.plain:
			arg, err := renderArg(s.Text, vars)
			if err != nil {
				return nil, err
			}
			out = append(out, arg)
		case s.eachField != "":
			value := vars.input[s.eachField]
			if value == nil {
				continue
			}
			list, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("input field %q must be a list", s.eachField)
			}
			for _, item := range list {
				text, err := scalarText(item)
				if err != nil {
					return nil, fmt.Errorf("input field %q: %w", s.eachField, err)
				}
				itemVars := vars
				itemVars.item, itemVars.hasItem = text, true
				arg, err := renderArg(s.Value, itemVars)
				if err != nil {
					return nil, err
				}
				if s.Flag != "" {
					out = append(out, s.Flag)
				}
				out = append(out, arg)
			}
		default:
			if s.whenField != "" && !given(vars.input[s.whenField]) {
				continue
			}
			if missingInput(s.Value, vars.input) {
				continue
			}
			if s.Flag != "" {
				out = append(out, s.Flag)
			}
			if s.Value != "" {
				arg, err := renderArg(s.Value, vars)
				if err != nil {
					return nil, err
				}
				out = append(out, arg)
			}
		}
	}
	return out, nil
}

// given is how when decides: a field counts if it's true, or set to anything
// other than false, null or an empty string.
func given(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	}
	return true
}

func missingInput(tpl string, input map[string]any) bool {
	for _, field := range inputRefs(tpl) {
		if input[field] == nil {
			return true
		}
	}
	return false
}

func scalarText(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		return fmt.Sprint(v), nil
	}
	return "", errors.New("list items must be strings, numbers or booleans")
}

// --- Input schema ---

// schemaFields reads the top level of an object schema. ok is false for a schema
// with no properties, which then can't be checked.
type schemaFields struct {
	ok       bool
	known    map[string]bool
	required map[string]bool
	defaults map[string]any
}

func readSchemaFields(schemaMap map[string]any) (schemaFields, error) {
	f := schemaFields{known: map[string]bool{}, required: map[string]bool{}, defaults: map[string]any{}}
	props, _ := schemaMap["properties"].(map[string]any)
	if props == nil {
		return f, nil
	}
	f.ok = true
	if list, ok := schemaMap["required"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				f.required[name] = true
			}
		}
	}
	for name, prop := range props {
		f.known[name] = true
		propMap, ok := prop.(map[string]any)
		if !ok {
			continue
		}
		value, has := propMap["default"]
		if !has {
			continue
		}
		// A round trip through JSON gives the default the same types job input has.
		raw, err := json.Marshal(value)
		if err != nil {
			return f, fmt.Errorf("input_schema: default for %q: %w", name, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return f, err
		}
		f.defaults[name] = decoded
	}
	return f, nil
}

// needAlways checks that a template that's always used refers only to fields a
// job is sure to have, so a missing optional field fails at startup rather than
// on some later job.
func (f schemaFields) needAlways(where, tpl string) error {
	if !f.ok {
		return nil
	}
	for _, field := range inputRefs(tpl) {
		if !f.known[field] {
			return fmt.Errorf("%s uses {{input.%s}}, which isn't in input_schema.properties", where, field)
		}
		if !f.required[field] && f.defaults[field] == nil {
			return fmt.Errorf("%s always uses {{input.%s}}, so it must be required or have a default; to pass it only when given, use an args item such as { flag: --%s, value: \"{{input.%s}}\" }", where, field, field, field)
		}
	}
	return nil
}

func (f schemaFields) mustKnow(where, field string) error {
	if f.ok && !f.known[field] {
		return fmt.Errorf("%s names %q, which isn't in input_schema.properties", where, field)
	}
	return nil
}

// applyDefaults fills in schema defaults for fields a job left out.
func (c *Capability) applyDefaults(input map[string]any) {
	for name, value := range c.fields.defaults {
		if _, ok := input[name]; !ok {
			input[name] = value
		}
	}
}

// --- Results ---

// ResultConfig says where a command's answer comes from: what it prints, or a
// file it writes. The answer is returned with the job, rather than as a file.
type ResultConfig struct {
	From      string `yaml:"from,omitempty"`
	Path      string `yaml:"path,omitempty"`
	Format    string `yaml:"format,omitempty"`
	Field     string `yaml:"field,omitempty"`
	TailLines int    `yaml:"tail_lines,omitempty"`
}

const (
	resultFromStdout = "stdout"
	resultFromFile   = "file"
	formatText       = "text"
	formatJSON       = "json"
)

func (r *ResultConfig) validate() error {
	switch r.From {
	case "":
		r.From = resultFromStdout
	case resultFromStdout, resultFromFile:
	default:
		return fmt.Errorf("result.from %q must be stdout or file", r.From)
	}
	if (r.From == resultFromFile) != (r.Path != "") {
		return errors.New("result.path is needed with from: file, and only then")
	}
	if r.Path != "" && !filepath.IsLocal(r.Path) {
		return errors.New("result.path must be a relative path inside the job's working directory")
	}
	switch r.Format {
	case "":
		r.Format = formatText
	case formatText, formatJSON:
	default:
		return fmt.Errorf("result.format %q must be text or json", r.Format)
	}
	if r.Field != "" && (r.Format != formatJSON || !responseFieldRE.MatchString(r.Field)) {
		return errors.New("result.field needs format: json and a field path such as data.text")
	}
	if r.TailLines < 0 || (r.TailLines > 0 && r.Format != formatText) {
		return errors.New("result.tail_lines needs format: text and a positive number")
	}
	return nil
}

// --- Outputs ---

// OutputSpec is a file the job returns. A plain string is a path. A mapping can
// return a folder as a zip, or skip a file the command didn't make.
type OutputSpec struct {
	Path     string `yaml:"path"`
	Archive  string `yaml:"archive,omitempty"`
	Optional bool   `yaml:"optional,omitempty"`
}

const archiveZip = "zip"

func (o *OutputSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&o.Path)
	}
	type plain OutputSpec
	var raw plain
	if err := decodeStrict(node, &raw, "path", "archive", "optional"); err != nil {
		return fmt.Errorf("outputs: %w", err)
	}
	*o = OutputSpec(raw)
	return nil
}

// MarshalYAML writes a plain output back as just its path.
func (o OutputSpec) MarshalYAML() (any, error) {
	if o.Archive == "" && !o.Optional {
		return o.Path, nil
	}
	type plain OutputSpec
	return plain(o), nil
}

// artifactName is what the result file is called: the output's name plus the
// file's extension, or .zip for a folder.
func (o OutputSpec) artifactName(name string) string {
	if o.Archive == archiveZip {
		return name + ".zip"
	}
	return name + filepath.Ext(o.Path)
}

// --- Environment ---

func validateEnv(env map[string]string, files map[string]bool) error {
	for name, value := range env {
		if !envNameRE.MatchString(name) {
			return fmt.Errorf("env: %q isn't a valid variable name", name)
		}
		for _, match := range placeholderRE.FindAllStringSubmatch(value, -1) {
			if sub := envVarRE.FindStringSubmatch(match[1]); sub != nil {
				if sub[1] == "FLEET_NODE_TOKEN" {
					return errors.New("env: the node token can't be passed to a command")
				}
				continue
			}
			if err := checkPlaceholders(match[0], files); err != nil {
				return fmt.Errorf("env.%s: %w", name, err)
			}
		}
	}
	return nil
}

// renderEnv fills env values in one pass, so text from a job's input can never be
// read as {{env.NAME}} and reveal the worker's own variables.
func renderEnv(env map[string]string, vars templateVars) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for name, value := range env {
		vars.allowEnv = true
		rendered, err := render(value, vars)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", name, err)
		}
		out[name] = rendered
	}
	return out, nil
}

// staticEnv is the env a check gets. A check has no job, so variables that need
// one are left out.
func (c *Capability) staticEnv() map[string]string {
	out := map[string]string{}
	for name, value := range c.Env {
		usesJob := false
		for _, match := range placeholderRE.FindAllStringSubmatch(value, -1) {
			usesJob = usesJob || envVarRE.FindStringSubmatch(match[1]) == nil
		}
		if usesJob {
			continue
		}
		rendered, err := render(value, templateVars{allowEnv: true})
		if err == nil {
			out[name] = rendered
		}
	}
	return out
}

// --- Validation of the command-line settings ---

// validateDeclaration checks the parts of a command-line tool that depend on its
// input schema. It runs after the schema compiles.
func (c *Capability) validateDeclaration(baseDir string, files map[string]bool) error {
	c.Description = strings.TrimSpace(c.Description)
	if len(c.Description) > maxDescriptionBytes {
		return fmt.Errorf("description is longer than %d bytes", maxDescriptionBytes)
	}
	fields, err := readSchemaFields(c.InputSchema)
	if err != nil {
		return err
	}
	c.fields = fields
	if c.isService() {
		return nil
	}

	for i, arg := range c.Command[1:] {
		if err := fields.needAlways(fmt.Sprintf("command[%d]", i+1), arg); err != nil {
			return err
		}
	}
	if err := fields.needAlways("stdin", c.Stdin); err != nil {
		return err
	}
	for i := range c.Args {
		s := &c.Args[i]
		where := fmt.Sprintf("args[%d]", i)
		if err := s.validate(files); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if s.plain {
			if err := fields.needAlways(where, s.Text); err != nil {
				return err
			}
			continue
		}
		for _, field := range append(inputRefs(s.Value), s.whenField, s.eachField) {
			if field == "" {
				continue
			}
			if err := fields.mustKnow(where, field); err != nil {
				return err
			}
		}
	}
	if err := validateEnv(c.Env, files); err != nil {
		return err
	}
	for name, value := range c.Env {
		if err := fields.needAlways("env."+name, value); err != nil {
			return err
		}
	}

	if c.Cwd != "" {
		if placeholderRE.MatchString(c.Cwd) {
			return errors.New("cwd can't contain placeholders")
		}
		c.cwd = filepath.Clean(resolvePath(baseDir, c.Cwd))
	}

	if c.Result != nil {
		if err := c.Result.validate(); err != nil {
			return err
		}
	}

	c.successCodes = []int{0}
	if len(c.SuccessExitCodes) > 0 {
		for _, code := range c.SuccessExitCodes {
			if code < 0 {
				return fmt.Errorf("success_exit_codes: %d isn't an exit code", code)
			}
		}
		c.successCodes = c.SuccessExitCodes
	}
	return nil
}

// workingDir is where the command runs: cwd if set, or the job's own folder.
func (c *Capability) workingDir(work string) string {
	if c.cwd != "" {
		return c.cwd
	}
	return work
}

// checkWorkingDir confirms cwd is still a folder this machine can use.
func (c *Capability) checkWorkingDir() error {
	if c.cwd == "" {
		return nil
	}
	info, err := os.Stat(c.cwd)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("cwd isn't a folder")
	}
	return nil
}
