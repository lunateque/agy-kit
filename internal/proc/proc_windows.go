//go:build windows

package proc

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	jobObjectExtendedLimitInfoClass = 9
	jobObjectLimitKillOnJobClose    = 0x00002000
	processAccessForJob             = 0x0001 | 0x0100 // PROCESS_TERMINATE | PROCESS_SET_QUOTA

	createNewProcessGroup  = 0x00000200
	createNoWindow         = 0x08000000
	createBreakawayFromJob = 0x01000000
)

type jobObjectBasicLimitInfo struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInfo
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW   = kernel32.NewProc("CreateJobObjectW")
	procSetInfoJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject = kernel32.NewProc("TerminateJobObject")
)

// Handle tracks a child process tree inside a Windows Job Object (with taskkill
// tree fallback if job assignment fails).
type Handle struct {
	mu       sync.Mutex
	job      syscall.Handle
	pid      int
	assigned atomic.Bool
}

// ResolveExecutable resolves bin via PATH and refuses .cmd/.bat shims on Windows
// to prevent cmd.exe argument-parsing / BatBadBut command injection.
func ResolveExecutable(bin string) (string, error) {
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(resolved))
	if ext == ".cmd" || ext == ".bat" {
		return "", fmt.Errorf("refusing to invoke batch shim %q (command-injection risk on Windows); install native .exe binary", resolved)
	}
	return resolved, nil
}

// Configure prepares a tracked subprocess (hidden console, 5s WaitDelay).
func Configure(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// StartDetached starts a detached background worker without flashing a console
// window. Tries CREATE_BREAKAWAY_FROM_JOB first so a parent hook Job Object
// won't terminate the worker; falls back cleanly and reports usedFallback.
func StartDetached(bin string, args []string, dir string, env []string) (usedFallback bool, err error) {
	spawn := func(flags uint32) error {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: flags,
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		_ = cmd.Process.Release()
		return nil
	}

	if err := spawn(createBreakawayFromJob | createNewProcessGroup | createNoWindow); err == nil {
		return false, nil
	}
	return true, spawn(createNewProcessGroup | createNoWindow)
}

func killTreeFallback(pid int) {
	if pid <= 0 {
		return
	}
	c := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = c.Run()
}

// StartTracked starts cmd inside a Job Object with KILL_ON_JOB_CLOSE.
// If job assignment fails, falls back to taskkill /T /F on cancel/close so
// child process trees never survive timeout or shutdown.
func StartTracked(cmd *exec.Cmd) (*Handle, error) {
	jobH, _, err := procCreateJobObjectW.Call(0, 0)
	if jobH == 0 {
		return nil, err
	}
	job := syscall.Handle(jobH)

	var info jobObjectExtendedLimitInfo
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, err := procSetInfoJobObject.Call(
		uintptr(job),
		uintptr(jobObjectExtendedLimitInfoClass),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		_ = syscall.CloseHandle(job)
		return nil, err
	}

	h := &Handle{job: job}
	cmd.Cancel = func() error {
		h.mu.Lock()
		j := h.job
		h.mu.Unlock()
		if h.assigned.Load() && j != 0 {
			_, _, _ = procTerminateJobObject.Call(uintptr(j), 1)
		} else if cmd.Process != nil {
			killTreeFallback(cmd.Process.Pid)
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil
	}

	if err := cmd.Start(); err != nil {
		_ = syscall.CloseHandle(job)
		return nil, err
	}

	if cmd.Process != nil {
		h.mu.Lock()
		h.pid = cmd.Process.Pid
		h.mu.Unlock()
		pH, oerr := syscall.OpenProcess(processAccessForJob, false, uint32(cmd.Process.Pid))
		if oerr == nil {
			ar, _, _ := procAssignProcessToJob.Call(uintptr(job), uintptr(pH))
			if ar != 0 {
				h.assigned.Store(true)
			}
			_ = syscall.CloseHandle(pH)
		}
	}

	return h, nil
}

// Close terminates any lingering descendants in the Job Object (or via taskkill)
// and closes the job handle.
func (h *Handle) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	j := h.job
	pid := h.pid
	h.job = 0
	h.pid = 0
	h.mu.Unlock()

	if h.assigned.Load() && j != 0 {
		_, _, _ = procTerminateJobObject.Call(uintptr(j), 1)
	} else if pid > 0 {
		killTreeFallback(pid)
	}
	if j != 0 {
		_ = syscall.CloseHandle(j)
	}
}
