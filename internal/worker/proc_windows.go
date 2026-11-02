//go:build windows

package worker

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// commandGroup is how a running command can be stopped. A Job Object holds the
// command and everything it starts, as a process group does on other systems.
type commandGroup struct {
	job     windows.Handle
	process windows.Handle // Used only if the Job Object couldn't be set up.
}

var (
	groupsMu sync.Mutex
	groups   = map[int]commandGroup{}
)

// setProcessGroup stops each console tool from opening a window of its own.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// groupStarted puts a new command in a Job Object. Closing the object's last
// handle kills everything in it, so commands can't outlive a crashed worker.
//
// Windows can start a process and add it to a job only as two steps here, so a
// child started in the instant between them would escape. A process takes far
// longer than that to start anything.
func groupStarted(cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	group := commandGroup{process: process}
	if job, err := newKillOnCloseJob(); err == nil {
		if err := windows.AssignProcessToJobObject(job, process); err == nil {
			group = commandGroup{job: job}
			windows.CloseHandle(process)
		} else {
			windows.CloseHandle(job)
		}
	}
	groupsMu.Lock()
	groups[pid] = group
	groupsMu.Unlock()
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// terminateGroup has no gentler form here. Windows has no equivalent of SIGTERM
// for a program without a console, so a command is stopped at once.
func terminateGroup(pid int) { killGroup(pid) }

// killGroup stops a command this worker started, then forgets it. Being called
// again is safe: it never touches a PID it no longer holds a handle for, since
// Windows may have given that PID to something else.
func killGroup(pid int) {
	groupsMu.Lock()
	group, ok := groups[pid]
	delete(groups, pid)
	groupsMu.Unlock()
	if !ok {
		return
	}
	if group.job != 0 {
		_ = windows.TerminateJobObject(group.job, 1)
		windows.CloseHandle(group.job)
		return
	}
	_ = windows.TerminateProcess(group.process, 1)
	windows.CloseHandle(group.process)
}

// killOrphanProcess stops a command left by a crashed worker, which the caller
// has identified by its start time. Normally its Job Object has already done this.
func killOrphanProcess(pid int) {
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(process)
	_ = windows.TerminateProcess(process, 1)
}

// processStart returns a process's start time, which together with its PID
// identifies it. It returns "" if the process doesn't exist.
func processStart(pid int) string {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(process)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return ""
	}
	return fmt.Sprint(created.Nanoseconds())
}
