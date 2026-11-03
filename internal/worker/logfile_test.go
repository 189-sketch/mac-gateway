package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readLog(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLogRotatesWhileRunningAndKeepsANumberOfOldLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	log, err := OpenRotatingFile(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	line := func(n int) string { return fmt.Sprintf("%039d\n", n) }
	for n := range 15 {
		if _, err := log.Write([]byte(line(n))); err != nil {
			t.Fatal(err)
		}
	}
	// Two 40-byte lines fit in each file, so 15 lines end with one in the current
	// file, and the three kept logs hold the six before it.
	for name, want := range map[string]string{
		path:        line(14),
		path + ".1": line(12) + line(13),
		path + ".2": line(10) + line(11),
		path + ".3": line(8) + line(9),
	} {
		if got := readLog(t, name); got != want {
			t.Errorf("%s = %q, want %q", filepath.Base(name), got, want)
		}
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error("kept more old logs than allowed")
	}
	if _, err := os.Stat(path + ".rotating"); err == nil {
		t.Error("left a partly rotated file behind")
	}
}

func TestLogRotatesAFullFileLeftByAnEarlierRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := OpenRotatingFile(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	log.Write([]byte("new\n"))
	if got := readLog(t, path); got != "new\n" {
		t.Errorf("current log = %q", got)
	}
	if got := readLog(t, path+".1"); len(got) != 100 {
		t.Errorf("old log has %d bytes", len(got))
	}
}

func TestLogNeverSplitsASingleLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	log, err := OpenRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("y", 50)
	log.Write([]byte(long))
	if got := readLog(t, path); got != long {
		t.Errorf("log = %q", got)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("an empty log was rotated")
	}
}

// When the log can't be moved, as on Windows while something has it open, old logs
// must survive and logging must carry on. A directory in the way stops only that
// one move, so old logs would still shift if the code tried to.
func TestLogKeepsWorkingWhenItCantRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.log")
	log, err := OpenRotatingFile(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".rotating", 0o700); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("z", 39) + "\n"
	// Fails to rotate at lines 3, 5, 7, 9 and 11, leaving room for one more line.
	for range 11 {
		if _, err := log.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got := readLog(t, path+".1"); got != "older\n" {
		t.Errorf("an old log was lost: %q", got)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("old logs moved even though the current one couldn't")
	}
	if got := readLog(t, path); got != strings.Repeat(line, 11) {
		t.Errorf("lost lines while unable to rotate: got %d bytes", len(got))
	}

	// It tries again after another full file, not on every line.
	if err := os.Remove(path + ".rotating"); err != nil {
		t.Fatal(err)
	}
	log.Write([]byte(line))
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("tried to rotate again before writing another full file")
	}

	// It then rotates by itself, without waiting for a restart.
	log.Write([]byte(line))
	if got := readLog(t, path+".2"); got != "older\n" {
		t.Errorf("didn't rotate after recovering: .2 = %q", got)
	}
	if got := readLog(t, path+".1"); got != strings.Repeat(line, 12) {
		t.Errorf("the rotated log has %d bytes", len(got))
	}
	if got := readLog(t, path); got != line {
		t.Errorf("the new log has %d bytes", len(got))
	}
}

func TestLogKeepsLinesWholeUnderConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	log, err := OpenRotatingFile(path, 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for writer := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for n := range 200 {
				fmt.Fprintf(log, "%d-%03d\n", writer, n)
			}
		}()
	}
	wait.Wait()
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]bool{}
	for _, file := range files {
		for _, line := range strings.Fields(readLog(t, file)) {
			if len(line) != 5 || lines[line] {
				t.Errorf("bad or repeated line %q", line)
			}
			lines[line] = true
		}
	}
	if len(lines) != 1600 {
		t.Errorf("kept %d of 1600 lines", len(lines))
	}
}
