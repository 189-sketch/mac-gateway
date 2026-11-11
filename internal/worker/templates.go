package worker

import (
	"bufio"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

//go:embed templates/*.yaml
var templateFiles embed.FS

// Template is a built-in starting point for a tool. Its body is a commented tool
// file with TODOs to fill in, and it starts disabled so it can't run half-made.
type Template struct {
	Name        string
	Title       string
	Description string
	Platforms   []string // Empty means every platform.
	Body        string   // With the header lines removed and TOOL_NAME as the name.
}

const templateNameToken = "TOOL_NAME"

// Templates lists the templates for a platform, such as runtime.GOOS. An empty
// platform lists them all.
func Templates(goos string) []Template {
	entries, _ := fs.ReadDir(templateFiles, "templates")
	var out []Template
	for _, entry := range entries {
		t, err := parseTemplate(entry.Name())
		if err != nil {
			continue
		}
		if goos == "" || len(t.Platforms) == 0 || slices.Contains(t.Platforms, goos) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func parseTemplate(file string) (Template, error) {
	raw, err := templateFiles.ReadFile("templates/" + file)
	if err != nil {
		return Template{}, err
	}
	t := Template{Name: strings.TrimSuffix(file, filepath.Ext(file))}
	var body strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := scanner.Text()
		if key, value, ok := strings.Cut(strings.TrimPrefix(line, "# "), ": "); ok && strings.HasPrefix(line, "# ") {
			switch key {
			case "template":
				t.Title = value
				continue
			case "about":
				t.Description = value
				continue
			case "platforms":
				t.Platforms = strings.Fields(value)
				continue
			}
		}
		body.WriteString(line + "\n")
	}
	// Drop the blank comment lines left between the header and the explanation.
	text := body.String()
	for strings.HasPrefix(text, "#\n") || strings.HasPrefix(text, "\n") {
		_, text, _ = strings.Cut(text, "\n")
	}
	t.Body = text
	if t.Title == "" {
		return Template{}, errors.New("template has no title")
	}
	return t, nil
}

// NewToolFile writes a new tool, from a template, into the manifest's tools
// folder. It never replaces an existing file.
func NewToolFile(m *Manifest, template, name, goos string) (string, error) {
	if !capabilityNameRE.MatchString(name) {
		return "", errors.New("the name must be lowercase letters, digits, '.', '_' or '-', such as tts.say")
	}
	if m.byName[name] != nil || slices.Contains(m.Disabled, name) {
		return "", fmt.Errorf("a tool named %q already exists", name)
	}
	var found *Template
	for _, t := range Templates(goos) {
		if t.Name == template {
			found = &t
			break
		}
	}
	if found == nil {
		return "", fmt.Errorf("no template named %q for this platform; see `fleet-worker tools templates`", template)
	}
	if err := os.MkdirAll(m.ToolsPath(), 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(m.ToolsPath(), name+".yaml")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("%s already exists", path)
	}
	if err != nil {
		return "", err
	}
	body := strings.ReplaceAll(found.Body, templateNameToken, name)
	if _, err := file.WriteString(body); err != nil {
		file.Close()
		return "", err
	}
	return path, file.Close()
}
