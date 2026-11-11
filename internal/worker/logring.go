package worker

import (
	"bytes"
	"sync"
)

// LogRing keeps the newest log lines in memory, so the configuration page can
// show what the worker is doing without reading its log file.
type LogRing struct {
	mu      sync.Mutex
	lines   []LogLine
	next    int64
	partial []byte
	max     int
}

// LogLine is one line of the log, numbered so a reader can ask for what's new.
type LogLine struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

// NewLogRing keeps up to max lines.
func NewLogRing(max int) *LogRing { return &LogRing{max: max, next: 1} }

func (r *LogRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := append(r.partial, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		r.lines = append(r.lines, LogLine{Seq: r.next, Text: string(data[:i])})
		r.next++
		data = data[i+1:]
	}
	// A line that never ends mustn't grow without limit.
	if len(data) > maxLineBytes {
		data = data[:0]
	}
	r.partial = append([]byte(nil), data...)
	if len(r.lines) > r.max {
		r.lines = append([]LogLine(nil), r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

// After returns the lines numbered after seq.
func (r *LogRing) After(seq int64) []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []LogLine{}
	for _, line := range r.lines {
		if line.Seq > seq {
			out = append(out, line)
		}
	}
	return out
}
