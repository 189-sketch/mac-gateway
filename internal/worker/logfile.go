package worker

import (
	"fmt"
	"os"
	"sync"
)

const (
	// LogFileMaxBytes and LogFileKeep limit a worker's logs to about 40 MB, so a
	// machine left running for years can't fill its disk.
	LogFileMaxBytes = 10 << 20
	LogFileKeep     = 3
)

// RotatingFile is a log file that moves aside when it gets too big, keeping the
// newest few as path.1, path.2 and so on. It's what lets someone run tail -f on
// the current log while old ones are still there to read.
type RotatingFile struct {
	path string
	max  int64
	keep int

	mu   sync.Mutex
	file *os.File
	size int64
}

// OpenRotatingFile opens a log for appending. If it's already full, it moves aside
// at the first write.
func OpenRotatingFile(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{path: path, max: maxBytes, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	r.file, r.size = file, info.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		r.rotate()
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate never loses the ability to log. The file is closed first, because Windows
// can't rename an open file. It's moved out of the way before the old logs shift:
// on Windows, someone reading the log can stop it moving, and shifting first would
// then discard an old log on every attempt.
func (r *RotatingFile) rotate() {
	r.file.Close()
	moving := r.path + ".rotating"
	moved := os.Rename(r.path, moving) == nil
	if moved {
		for i := r.keep - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		_ = os.Rename(moving, r.path+".1")
	}
	if err := r.open(); err != nil {
		// With nowhere to log, writes are dropped, but jobs keep running.
		r.file, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		r.size = 0
		return
	}
	if !moved {
		// Try again after another full file, not on every line.
		r.size = 0
	}
}
