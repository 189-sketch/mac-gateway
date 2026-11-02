package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mactunnel/internal/client"
)

func artifacts(names ...string) []client.Artifact {
	out := make([]client.Artifact, len(names))
	for i, name := range names {
		out[i] = client.Artifact{Name: name}
	}
	return out
}

// Result names come from the gateway, which must not be able to choose where
// files land on the agent's machine.
func TestDestinationsRefuseUnsafeNames(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/passwd", "..", ".hidden", "a/b", `a\b`, ""} {
		if _, err := destinations(artifacts(name), t.TempDir(), false); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestDestinationsChoosePaths(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name   string
		names  []string
		output string
		want   []string
	}{
		{"one result to a file", []string{"a.wav"}, filepath.Join(dir, "voice.wav"), []string{filepath.Join(dir, "voice.wav")}},
		{"one result to a directory", []string{"a.wav"}, dir, []string{filepath.Join(dir, "a.wav")}},
		{"one result to a new directory", []string{"a.wav"}, filepath.Join(dir, "new") + "/", []string{filepath.Join(dir, "new", "a.wav")}},
		{"several results", []string{"a.wav", "b.txt"}, filepath.Join(dir, "many"), []string{filepath.Join(dir, "many", "a.wav"), filepath.Join(dir, "many", "b.txt")}},
		{"default is the current directory", []string{"a.wav"}, "", []string{"a.wav"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := destinations(artifacts(tt.names...), tt.output, false)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDestinationsRespectExistingFiles(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "a.wav")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := destinations(artifacts("a.wav"), dir, false); err == nil {
		t.Error("would overwrite without --force")
	}
	if _, err := destinations(artifacts("a.wav"), dir, true); err != nil {
		t.Errorf("--force: %v", err)
	}
	// A dangling symlink counts too, since writing through it lands elsewhere.
	link := filepath.Join(dir, "b.wav")
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := destinations(artifacts("b.wav"), dir, false); err == nil {
		t.Error("would write through a symlink")
	}
}
