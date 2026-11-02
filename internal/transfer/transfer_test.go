package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// serveContent answers like the gateway, with Range support and a required token.
func serveContent(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(server.Close)
	return server
}

func download(server *httptest.Server, dest string, content []byte, onResume func(int64)) error {
	return Download(context.Background(), server.Client(), DownloadRequest{
		URL: server.URL, Token: "token", Dest: dest, Size: int64(len(content)), SHA256: sum(content), OnResume: onResume,
	})
}

func TestDownloadFromScratch(t *testing.T) {
	content := []byte("the whole file")
	dest := filepath.Join(t.TempDir(), "out")
	if err := download(serveContent(t, content), dest, content, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Errorf("got %q", got)
	}
}

func TestDownloadContinuesFromPartialFile(t *testing.T) {
	content := []byte("0123456789")
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(dest, content[:4], 0o600); err != nil {
		t.Fatal(err)
	}
	var resumedAt int64 = -1
	if err := download(serveContent(t, content), dest, content, func(offset int64) { resumedAt = offset }); err != nil {
		t.Fatal(err)
	}
	if resumedAt != 4 {
		t.Errorf("resumed at %d, want 4", resumedAt)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Errorf("got %q", got)
	}
}

func TestDownloadSkipsCompleteFile(t *testing.T) {
	content := []byte("already here")
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	if err := download(server, dest, content, nil); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Errorf("made %d requests for a file it already had", requests)
	}
}

func TestDownloadReplacesPartialFileItCannotUse(t *testing.T) {
	content := []byte("0123456789")
	for name, existing := range map[string][]byte{
		"longer than expected": []byte("0123456789-and-more"),
		"right size, wrong":    []byte("xxxxxxxxxx"),
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			if err := os.WriteFile(dest, existing, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := download(serveContent(t, content), dest, content, nil); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
				t.Errorf("got %q", got)
			}
		})
	}
}

// A partial file whose start doesn't match the real file only shows up once the
// whole thing is hashed. It must be rejected and discarded, so a retry can work.
func TestDownloadRejectsCorruptResult(t *testing.T) {
	content := []byte("0123456789")
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(dest, []byte("xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := serveContent(t, content)
	if err := download(server, dest, content, nil); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if info, err := os.Stat(dest); err != nil || info.Size() != 0 {
		t.Errorf("the corrupt file wasn't discarded: %v %v", info, err)
	}
	if err := download(server, dest, content, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Errorf("got %q", got)
	}
}

func TestDownloadIgnoringRangeRestartsCleanly(t *testing.T) {
	content := []byte("0123456789")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	}))
	defer server.Close()
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(dest, []byte("xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := download(server, dest, content, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Errorf("got %q", got)
	}
}

func TestDownloadRefusesWrongResumePosition(t *testing.T) {
	content := []byte("0123456789")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 2-9/%d", len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[2:])
	}))
	defer server.Close()
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(dest, content[:4], 0o600); err != nil {
		t.Fatal(err)
	}
	err := download(server, dest, content, nil)
	if err == nil || !strings.Contains(err.Error(), "wrong position") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content[:4]) {
		t.Errorf("the partial file changed: %q", got)
	}
}

func TestDownloadReportsServerErrors(t *testing.T) {
	content := []byte("x")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()
	var status *StatusError
	err := download(server, filepath.Join(t.TempDir(), "out"), content, nil)
	if !errors.As(err, &status) || status.Code != http.StatusGone {
		t.Fatalf("err = %v", err)
	}
}
