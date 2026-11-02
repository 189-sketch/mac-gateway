package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"mactunnel/internal/protocol"
)

// attemptIDRE guards the path each attempt's directory is built from, because
// the ID comes from the network.
var attemptIDRE = regexp.MustCompile(`^att_[0-9a-f]{24}$`)

const (
	metaFile   = "meta.json"
	eventsFile = "events.jsonl"
	workDir    = "work"
)

// attemptMeta is the part of an attempt that must survive a worker crash.
type attemptMeta struct {
	JobID      string `json:"job_id"`
	AttemptID  string `json:"attempt_id"`
	Capability string `json:"capability"`
	PID        int    `json:"pid,omitempty"`
	// PIDStart distinguishes our process from a later one reusing its PID.
	PIDStart string `json:"pid_start,omitempty"`
}

func writeMeta(dir string, meta attemptMeta) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, metaFile+".tmp")
	if err := writeSynced(tmp, raw); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, metaFile))
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// appendEvent durably records an event before it can be sent.
func appendEvent(f *os.File, ev protocol.Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// loadEvents reads a spool file, dropping a final line torn by a crash so that
// later appends can't merge into it.
func loadEvents(path string) ([]protocol.Event, *os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	var events []protocol.Event
	var good int64
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break // Anything left without a newline is torn.
		}
		var ev protocol.Event
		if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil || ev.Seq != int64(len(events)+1) {
			break
		}
		events = append(events, ev)
		good += int64(len(line))
	}
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("truncate %s: %w", path, err)
	}
	if _, err := f.Seek(good, 0); err != nil {
		f.Close()
		return nil, nil, err
	}
	return events, f, nil
}

// readMeta loads an attempt's metadata. A missing file means the directory was
// being deleted when the worker stopped.
func readMeta(dir string) (attemptMeta, bool, error) {
	var meta attemptMeta
	raw, err := os.ReadFile(filepath.Join(dir, metaFile))
	if errors.Is(err, os.ErrNotExist) {
		return meta, false, nil
	}
	if err != nil {
		return meta, false, err
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, false, nil
	}
	return meta, true, nil
}
