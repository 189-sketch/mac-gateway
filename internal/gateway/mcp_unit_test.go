package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMCPToolName(t *testing.T) {
	cases := map[string]string{
		"tts.say":         "tts_say",
		"video/translate": "video_translate",
		"plain-name_1":    "plain-name_1",
		"emoji✓tool":      "emoji_tool",
	}
	for in, want := range cases {
		if got := mcpToolName(in); got != want {
			t.Errorf("mcpToolName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 100)
	a, b := mcpToolName(long), mcpToolName(long+"b")
	if len(a) > maxMCPToolName || len(b) > maxMCPToolName {
		t.Fatalf("long names weren't shortened: %d, %d", len(a), len(b))
	}
	if a == b {
		t.Fatalf("two long names that differ only at the end got the same tool name %q", a)
	}
	if mcpToolName(long) != a {
		t.Fatal("the same capability got a different name the second time")
	}
}

func TestTailText(t *testing.T) {
	if got := tailText("short", 10); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	if got := tailText("one\ntwo\nthree\n", 9); got != "three\n" {
		t.Fatalf("didn't start at a whole line: %q", got)
	}
	// A cut through a multi-byte character must not leave half of it.
	text := strings.Repeat("é", 20)
	for limit := 1; limit < len(text); limit++ {
		got := tailText(text, limit)
		if !utf8.ValidString(got) || len(got) > limit {
			t.Fatalf("limit %d gave %q", limit, got)
		}
	}
}

func TestWithFileField(t *testing.T) {
	schema, field := withFileField(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`, nil)
	if field != "" || schema["properties"].(map[string]any)["input_files"] != nil {
		t.Fatalf("a tool without files got a file field: %v", schema)
	}
	schema, field = withFileField(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`, []string{"audio"})
	if field != "input_files" {
		t.Fatalf("field = %q", field)
	}
	required := schema["required"].([]any)
	if len(required) != 2 || required[0] != "text" || required[1] != "input_files" {
		t.Fatalf("required = %v", required)
	}
	_, field = withFileField(`{"properties":{"input_files":{"type":"string"}}}`, []string{"audio"})
	if field != "fleet_input_files" {
		t.Fatalf("a clashing name gave field %q", field)
	}
	schema, _ = withFileField(`not json`, nil)
	if schema["type"] != "object" {
		t.Fatalf("a broken schema wasn't replaced: %v", schema)
	}
}

func TestDecodeHeaderValue(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"tts_say", "tts_say", true},
		{"", "", false},
		{"=?base64?dHRzX3NheQ==?=", "tts_say", true},
		{"=?base64?dHRzX3NheQ==", "", false},
		{"=?base64?!!!?=", "", false},
	}
	for _, c := range cases {
		got, ok := decodeHeaderValue(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("decodeHeaderValue(%q) = %q, %v", c.in, got, ok)
		}
	}
}

func TestSignedLinks(t *testing.T) {
	g := &Gateway{cfg: Config{ArtifactTTL: time.Hour}}
	var err error
	if g.linkKey, err = loadLinkKey(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	link, _ := g.downloadLink("https://fleet.example.com", "key_1", "file_1", time.Time{})
	token := strings.TrimPrefix(link, "https://fleet.example.com/v1/links/")
	claims, err := g.readLink(token)
	if err != nil || claims.KeyID != "key_1" || claims.FileID != "file_1" || claims.Kind != linkDownload {
		t.Fatalf("readLink = %+v, %v", claims, err)
	}

	payload, sig, _ := strings.Cut(token, ".")
	other := g.signLink(linkClaims{Kind: linkDownload, KeyID: "key_1", FileID: "file_2", Expires: claims.Expires})
	otherPayload, _, _ := strings.Cut(other, ".")
	for name, bad := range map[string]string{
		"another file's claims with this signature": otherPayload + "." + sig,
		"no signature":    payload,
		"empty signature": payload + ".",
		"garbage":         "!!!.???",
		"truncated":       token[:len(token)-2],
	} {
		if _, err := g.readLink(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	expired := g.signLink(linkClaims{Kind: linkDownload, KeyID: "key_1", FileID: "file_1", Expires: time.Now().Add(-time.Second).Unix()})
	if _, err := g.readLink(expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired link gave %v", err)
	}

	// A link never outlives its file.
	soon := time.Now().Add(time.Minute)
	if _, until := g.downloadLink("", "key_1", "file_1", soon); !until.Equal(soon) {
		t.Fatalf("link lasts until %v, file until %v", until, soon)
	}

	// A different key, as after replacing link.key, ends every link.
	g2 := &Gateway{cfg: g.cfg}
	if g2.linkKey, err = loadLinkKey(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := g2.readLink(token); err == nil {
		t.Fatal("a link signed with another key was accepted")
	}
}

func TestLinkKeyPersists(t *testing.T) {
	dir := t.TempDir()
	a, err := loadLinkKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadLinkKey(dir)
	if err != nil || string(a) != string(b) {
		t.Fatal("the link key changed between restarts")
	}
}

func TestPublicBaseTrustsOnlyLocalProxies(t *testing.T) {
	g := &Gateway{}
	r := httptest.NewRequest("POST", "http://10.0.0.5:8080/mcp", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "evil.example")
	r.RemoteAddr = "203.0.113.9:5555"
	if got := g.publicBase(r); got != "http://10.0.0.5:8080" {
		t.Fatalf("a remote client's forwarded headers were trusted: %s", got)
	}
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-Host", "fleet.example.com")
	if got := g.publicBase(r); got != "https://fleet.example.com" {
		t.Fatalf("the local proxy's headers were ignored: %s", got)
	}
	r.Header.Set("X-Forwarded-Proto", "javascript")
	if got := g.publicBase(r); got != "http://fleet.example.com" {
		t.Fatalf("an odd scheme was used: %s", got)
	}
	g.cfg.PublicURL = "https://fleet.example.org/"
	if got := g.publicBase(r); got != "https://fleet.example.org" {
		t.Fatalf("--public-url was ignored: %s", got)
	}
}
