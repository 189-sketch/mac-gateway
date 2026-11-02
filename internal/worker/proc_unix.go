//go:build unix

package worker

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// setProcessGroup makes the command lead its own process group, so stopping a
// job also stops anything the command started, such as ffmpeg under a wrapper.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// These ignore errors: the usual one is that the group has already exited.
func terminateGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGTERM) }
func killGroup(pid int)      { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// processStart returns a process's start time, which together with its PID
// identifies it. It returns "" if the process doesn't exist.
func processStart(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
