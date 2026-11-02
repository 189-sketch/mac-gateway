// Package transfer moves large files over unreliable connections. It is shared
// by the worker and the agent CLI.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// StallTimeout is how long a transfer may go without moving any data. A total
// timeout wouldn't work, because a large video can legitimately take hours.
const StallTimeout = 60 * time.Second

// ErrChecksum means a completed download didn't match its expected hash. The
// partial file has been discarded, so a retry starts from the beginning.
var ErrChecksum = errors.New("downloaded file does not match its checksum")

// StatusError is an unexpected HTTP response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Code, e.Body)
}

// StallReader records when data last moved.
type StallReader struct {
	Reader io.Reader
	Closer io.Closer
	lastNS atomic.Int64
	done   atomic.Bool
}

// Touch marks data as having just moved.
func (s *StallReader) Touch() { s.lastNS.Store(time.Now().UnixNano()) }

func (s *StallReader) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.Touch()
	if err != nil {
		s.done.Store(true)
	}
	return n, err
}

// Close ends stall tracking and closes the underlying stream, if any.
func (s *StallReader) Close() error {
	s.done.Store(true)
	if s.Closer != nil {
		return s.Closer.Close()
	}
	return nil
}

// WatchStall cancels a transfer if no data moves for StallTimeout. Once the
// reader has finished, waiting for a response is someone else's timeout.
func WatchStall(ctx context.Context, reader *StallReader, cancel context.CancelFunc) (stop func()) {
	reader.Touch()
	stopped := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !reader.done.Load() && time.Since(time.Unix(0, reader.lastNS.Load())) > StallTimeout {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(stopped) }
}

// HashFile returns a file's size and SHA-256 hex digest.
func HashFile(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// DownloadRequest describes a file to fetch.
type DownloadRequest struct {
	URL    string
	Token  string
	Dest   string
	Size   int64
	SHA256 string
	// OnResume, if set, is called when part of the file is already on disk.
	OnResume func(offset int64)
}

// Download fetches a file to Dest, continuing from whatever a previous try left
// there. The result is verified against SHA256 before it returns nil.
func Download(ctx context.Context, client *http.Client, d DownloadRequest) error {
	file, err := os.OpenFile(d.Dest, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	offset := info.Size()
	if offset > d.Size {
		offset = 0
	}
	if offset == d.Size {
		ok, err := matches(d.Dest, d.SHA256)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		offset = 0
	}

	transferCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(transferCtx, http.MethodGet, d.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)) {
			return errors.New("server resumed the download from the wrong position")
		}
		if d.OnResume != nil {
			d.OnResume(offset)
		}
	case http.StatusOK:
		offset = 0
	case http.StatusRequestedRangeNotSatisfiable:
		if err := file.Truncate(0); err != nil {
			return err
		}
		return errors.New("server refused the resume position; restarting")
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &StatusError{Code: resp.StatusCode, Body: string(body)}
	}

	if err := file.Truncate(offset); err != nil {
		return err
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	body := &StallReader{Reader: io.LimitReader(resp.Body, d.Size-offset)}
	stop := WatchStall(transferCtx, body, cancel)
	defer stop()
	_, copyErr := io.Copy(file, body)
	// Keep what arrived, even after an error, so the next try can continue.
	if err := file.Sync(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return copyErr
	}

	ok, err := matches(d.Dest, d.SHA256)
	if err != nil {
		return err
	}
	if !ok {
		if err := file.Truncate(0); err != nil {
			return err
		}
		return ErrChecksum
	}
	return nil
}

func matches(path, want string) (bool, error) {
	_, got, err := HashFile(path)
	if err != nil {
		return false, err
	}
	return got == want, nil
}
